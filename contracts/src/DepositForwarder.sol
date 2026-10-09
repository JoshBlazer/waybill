// SPDX-License-Identifier: MIT
pragma solidity 0.8.30;

import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";

/// @title DepositForwarder
/// @notice Implementation behind every per-invoice deposit address. Each
///         invoice gets an EIP-1167 clone of this contract at a CREATE2
///         address; payers send tokens there, and the factory sweeps them
///         into the vault.
/// @dev `vault` and `factory` are immutables in the implementation's code,
///      so every clone (which delegatecalls this code) shares them. Clones
///      need no initialiser, and nothing in a clone's storage can redirect
///      funds. The contract has no receive or fallback function, so native
///      ETH sent to a deposit address after deployment is rejected.
contract DepositForwarder {
    using SafeERC20 for IERC20;

    /// @notice Where every sweep goes. Fixed at deployment.
    address public immutable vault;
    /// @notice The only caller allowed to sweep.
    address public immutable factory;

    event Swept(address indexed token, uint256 amount);

    error OnlyFactory();
    error ZeroAddress();

    constructor(address vault_) {
        if (vault_ == address(0)) revert ZeroAddress();
        vault = vault_;
        factory = msg.sender;
    }

    /// @notice Move this address's entire balance of `token` to the vault.
    /// @return amount The amount swept, possibly zero.
    function sweep(IERC20 token) external returns (uint256 amount) {
        if (msg.sender != factory) revert OnlyFactory();
        amount = token.balanceOf(address(this));
        if (amount > 0) token.safeTransfer(vault, amount);
        emit Swept(address(token), amount);
    }
}
