// SPDX-License-Identifier: MIT
pragma solidity 0.8.30;

import {AccessControl} from "@openzeppelin/contracts/access/AccessControl.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import {Pausable} from "@openzeppelin/contracts/utils/Pausable.sol";

/// @title Vault
/// @notice Holds swept funds and pays them out under limits.
/// @dev Losses from a compromised payout key are capped by per-transaction
///      and per-day limits per token, and PAUSER_ROLE can stop all outflows.
///      Only the admin can unpause, so a pauser key cannot undo a pause.
///      A token with no limits configured cannot be paid out at all:
///      limits fail closed. Each payout id can be used once, ever.
contract Vault is AccessControl, Pausable {
    using SafeERC20 for IERC20;

    bytes32 public constant PAYOUT_ROLE = keccak256("PAYOUT_ROLE");
    bytes32 public constant PAUSER_ROLE = keccak256("PAUSER_ROLE");

    struct Limits {
        uint128 perTx;
        uint128 perDay;
    }

    mapping(IERC20 token => Limits) public limits;
    /// @notice Amount paid out per token per UTC day (block.timestamp / 1 days).
    mapping(IERC20 token => mapping(uint256 day => uint256 amount)) public paidOnDay;
    /// @notice Payout ids already used. A payout id is never reused.
    mapping(bytes32 payoutId => bool) public paid;

    event LimitsSet(address indexed token, uint128 perTx, uint128 perDay);
    event Payout(
        bytes32 indexed payoutId, address indexed token, address indexed to, uint256 amount
    );

    error ZeroAddress();
    error ZeroAmount();
    error PayoutIdReused(bytes32 payoutId);
    error OverTransactionLimit(uint256 amount, uint256 limit);
    error OverDailyLimit(uint256 wouldTotal, uint256 limit);

    constructor(address admin) {
        if (admin == address(0)) revert ZeroAddress();
        _grantRole(DEFAULT_ADMIN_ROLE, admin);
    }

    function setLimits(IERC20 token, uint128 perTx, uint128 perDay)
        external
        onlyRole(DEFAULT_ADMIN_ROLE)
    {
        limits[token] = Limits(perTx, perDay);
        emit LimitsSet(address(token), perTx, perDay);
    }

    function pause() external onlyRole(PAUSER_ROLE) {
        _pause();
    }

    function unpause() external onlyRole(DEFAULT_ADMIN_ROLE) {
        _unpause();
    }

    /// @notice Pay `amount` of `token` to `to`, identified by `payoutId`.
    function payout(IERC20 token, address to, uint256 amount, bytes32 payoutId)
        external
        onlyRole(PAYOUT_ROLE)
        whenNotPaused
    {
        if (to == address(0)) revert ZeroAddress();
        if (amount == 0) revert ZeroAmount();
        if (paid[payoutId]) revert PayoutIdReused(payoutId);

        Limits memory l = limits[token];
        if (amount > l.perTx) revert OverTransactionLimit(amount, l.perTx);
        uint256 day = block.timestamp / 1 days;
        uint256 total = paidOnDay[token][day] + amount;
        if (total > l.perDay) revert OverDailyLimit(total, l.perDay);

        paid[payoutId] = true;
        paidOnDay[token][day] = total;
        emit Payout(payoutId, address(token), to, amount);
        token.safeTransfer(to, amount);
    }
}
