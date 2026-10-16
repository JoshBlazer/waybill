// SPDX-License-Identifier: MIT
pragma solidity 0.8.30;

import {AccessControl} from "@openzeppelin/contracts/access/AccessControl.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";

import {Vault} from "./Vault.sol";

/// @title BatchPayout
/// @notice Pays many recipients from the vault in one transaction.
/// @dev Every item goes through Vault.payout, so the vault's guards apply to
///      each one: a payout id is used at most once ever, per-transaction and
///      per-day limits hold, and a pause stops the whole batch. This contract
///      needs the vault's PAYOUT_ROLE and holds no funds itself.
///      A batch is all or nothing: if any item reverts, nothing is paid, so
///      a resubmitted or partly duplicated batch can never pay twice.
contract BatchPayout is AccessControl {
    bytes32 public constant BATCHER_ROLE = keccak256("BATCHER_ROLE");

    /// @notice Upper bound on items per batch, so a batch fits in a block.
    uint256 public constant MAX_ITEMS = 200;

    struct Item {
        address to;
        uint256 amount;
        bytes32 payoutId;
    }

    Vault public immutable vault;
    /// @notice Batch ids already paid. A batch id is never reused.
    mapping(bytes32 batchId => bool) public batchPaid;

    event BatchPaid(bytes32 indexed batchId, address indexed token, uint256 count, uint256 total);

    error ZeroAddress();
    error EmptyBatch();
    error TooManyItems(uint256 count, uint256 max);
    error BatchIdReused(bytes32 batchId);

    constructor(Vault vault_, address admin) {
        if (address(vault_) == address(0) || admin == address(0)) revert ZeroAddress();
        vault = vault_;
        _grantRole(DEFAULT_ADMIN_ROLE, admin);
    }

    /// @notice Pay every item in `items` in `token`, identified by `batchId`.
    function payBatch(IERC20 token, bytes32 batchId, Item[] calldata items)
        external
        onlyRole(BATCHER_ROLE)
    {
        uint256 n = items.length;
        if (n == 0) revert EmptyBatch();
        if (n > MAX_ITEMS) revert TooManyItems(n, MAX_ITEMS);
        if (batchPaid[batchId]) revert BatchIdReused(batchId);
        batchPaid[batchId] = true;

        uint256 total = 0;
        for (uint256 i = 0; i < n; ++i) {
            total += items[i].amount;
        }
        // Emitted before the external calls; if any payout reverts, so does
        // the event.
        emit BatchPaid(batchId, address(token), n, total);

        for (uint256 i = 0; i < n; ++i) {
            Item calldata it = items[i];
            // A call per item is the point: each goes through the vault's
            // guards, and any revert reverts the whole batch.
            // forge-lint: disable-next-line(calls-loop)
            vault.payout(token, it.to, it.amount, it.payoutId);
        }
    }
}
