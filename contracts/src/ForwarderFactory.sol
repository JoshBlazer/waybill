// SPDX-License-Identifier: MIT
pragma solidity 0.8.30;

import {AccessControl} from "@openzeppelin/contracts/access/AccessControl.sol";
import {Clones} from "@openzeppelin/contracts/proxy/Clones.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {DepositForwarder} from "./DepositForwarder.sol";

/// @title ForwarderFactory
/// @notice Computes and deploys per-invoice deposit addresses.
/// @dev salt = keccak256(invoice id), computed off-chain. The deposit address
///      is known before anything is deployed, so payers can pay a
///      counterfactual address; the clone is deployed only when it is swept.
contract ForwarderFactory is AccessControl {
    bytes32 public constant SWEEPER_ROLE = keccak256("SWEEPER_ROLE");

    /// @notice The implementation every clone delegates to.
    DepositForwarder public immutable implementation;

    event ForwarderDeployed(bytes32 indexed salt, address forwarder);
    event Swept(
        bytes32 indexed salt, address indexed forwarder, address indexed token, uint256 amount
    );

    error SweepMismatch(uint256 announced, uint256 moved);

    constructor(address vault_, address admin) {
        implementation = new DepositForwarder(vault_);
        _grantRole(DEFAULT_ADMIN_ROLE, admin);
    }

    /// @notice The vault every forwarder sweeps into.
    function vault() external view returns (address) {
        return implementation.vault();
    }

    /// @notice The deposit address for `salt`, whether or not it is deployed.
    function predict(bytes32 salt) public view returns (address) {
        return Clones.predictDeterministicAddress(address(implementation), salt, address(this));
    }

    /// @notice Deploy the forwarder for `salt` if needed, then sweep `token`
    ///         into the vault. Safe to call repeatedly.
    /// @dev Restricted to SWEEPER_ROLE so sweeps happen only when the
    ///      backend has decided a payment is final, keeping the on-chain
    ///      history easy to reconcile.
    ///
    ///      Events are emitted before any state-changing external call, so a
    ///      token that re-enters cannot reorder or fabricate the logs the
    ///      watcher reads. The announced amount is then enforced: the sweep
    ///      must move exactly what the event said.
    function deployAndSweep(bytes32 salt, IERC20 token)
        external
        onlyRole(SWEEPER_ROLE)
        returns (uint256 amount)
    {
        address forwarder = predict(salt);
        bool deploy = forwarder.code.length == 0;
        amount = token.balanceOf(forwarder);

        // The only call above is balanceOf, a view compiled to STATICCALL: it
        // cannot change state or emit logs, so it cannot reorder these events.
        // forge-lint: disable-next-line(reentrancy-events)
        if (deploy) emit ForwarderDeployed(salt, forwarder);
        // forge-lint: disable-next-line(reentrancy-events)
        emit Swept(salt, forwarder, address(token), amount);

        if (deploy) Clones.cloneDeterministic(address(implementation), salt);
        uint256 moved = DepositForwarder(forwarder).sweep(token);
        if (moved != amount) revert SweepMismatch(amount, moved);
    }
}
