// SPDX-License-Identifier: MIT
pragma solidity 0.8.30;

import {Test} from "forge-std/Test.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {BatchPayout} from "../src/BatchPayout.sol";
import {MockStablecoin} from "../src/MockStablecoin.sol";
import {Vault} from "../src/Vault.sol";

/// @notice Drives the vault through BatchPayout with random batches, drawing
///         payout and batch ids from small pools so reuse is common, with
///         amounts that sometimes break the limits, random pauses and time
///         jumps. It records what succeeded; the invariants compare that with
///         the chain.
contract BatchHandler is Test {
    MockStablecoin public immutable usdc;
    Vault public immutable vault;
    BatchPayout public immutable batch;
    address public immutable batcher;
    address public immutable pauser;
    address public immutable admin;

    uint256 public constant ID_POOL = 64;
    address[3] public recipients;

    // Ghost state: what the handler saw succeed.
    uint256 public ghostPaidTotal;
    mapping(bytes32 payoutId => uint256) public ghostTimesPaid;
    mapping(uint256 day => uint256) public ghostPaidOnDay;
    uint256[] public daysSeen;
    bool public ghostAtomicityBroken;
    uint256 public batchesPaid;
    uint256 public batchesReverted;

    constructor(
        MockStablecoin usdc_,
        Vault vault_,
        BatchPayout batch_,
        address batcher_,
        address pauser_,
        address admin_
    ) {
        usdc = usdc_;
        vault = vault_;
        batch = batch_;
        batcher = batcher_;
        pauser = pauser_;
        admin = admin_;
        recipients = [makeAddr("r0"), makeAddr("r1"), makeAddr("r2")];
    }

    function payoutId(uint256 i) public pure returns (bytes32) {
        return keccak256(abi.encode("payout", i % ID_POOL));
    }

    function payBatch(uint256 seed, uint8 count, uint8 batchSeed) external {
        uint256 n = bound(count, 1, 5);
        BatchPayout.Item[] memory items = new BatchPayout.Item[](n);
        uint256 total;
        for (uint256 i; i < n; ++i) {
            uint256 r = uint256(keccak256(abi.encode(seed, i)));
            // Up to 1,200 USDC: some items break the 1,000 per-transaction limit.
            uint256 amount = bound(r, 1, 1_200e6);
            items[i] = BatchPayout.Item(recipients[r % 3], amount, payoutId(r >> 128));
            total += amount;
        }
        bytes32 batchId = keccak256(abi.encode("batch", batchSeed % 40));

        uint256 before = usdc.balanceOf(address(vault));
        vm.prank(batcher);
        try batch.payBatch(IERC20(address(usdc)), batchId, items) {
            batchesPaid++;
            if (before - usdc.balanceOf(address(vault)) != total) ghostAtomicityBroken = true;
            uint256 day = block.timestamp / 1 days;
            if (ghostPaidOnDay[day] == 0) daysSeen.push(day);
            ghostPaidOnDay[day] += total;
            ghostPaidTotal += total;
            for (uint256 i; i < n; ++i) {
                ghostTimesPaid[items[i].payoutId]++;
            }
        } catch {
            batchesReverted++;
            if (usdc.balanceOf(address(vault)) != before) ghostAtomicityBroken = true;
        }
    }

    /// Pauses on one call in eight, so most batches meet a running vault.
    function pause(uint8 roll) external {
        if (roll % 8 != 0 || vault.paused()) return;
        vm.prank(pauser);
        vault.pause();
    }

    function unpause() external {
        if (!vault.paused()) return;
        vm.prank(admin);
        vault.unpause();
    }

    function warp(uint8 hoursAhead) external {
        vm.warp(block.timestamp + bound(hoursAhead, 1, 30) * 1 hours);
    }

    function daysSeenCount() external view returns (uint256) {
        return daysSeen.length;
    }
}

contract BatchPayoutInvariantTest is Test {
    MockStablecoin internal usdc;
    Vault internal vault;
    BatchPayout internal batch;
    BatchHandler internal handler;

    uint256 internal constant FUNDED = 1_000_000e6;
    uint128 internal constant PER_TX = 1_000e6;
    uint128 internal constant PER_DAY = 6_000e6; // a few batches a day, then the limit bites

    function setUp() public {
        address admin = makeAddr("admin");
        address batcher = makeAddr("batcher");
        address pauser = makeAddr("pauser");
        usdc = new MockStablecoin("Mock USD Coin", "mUSDC");
        vault = new Vault(admin);
        batch = new BatchPayout(vault, admin);
        vm.startPrank(admin);
        vault.grantRole(vault.PAYOUT_ROLE(), address(batch));
        vault.grantRole(vault.PAUSER_ROLE(), pauser);
        vault.setLimits(IERC20(address(usdc)), PER_TX, PER_DAY);
        batch.grantRole(batch.BATCHER_ROLE(), batcher);
        vm.stopPrank();
        usdc.mint(address(vault), FUNDED);
        vm.warp(1_760_000_000);

        handler = new BatchHandler(usdc, vault, batch, batcher, pauser, admin);
        targetContract(address(handler));
    }

    /// I16: what left the vault is exactly what the recorded payouts paid,
    /// and it all reached recipients. Nothing stays in BatchPayout.
    function invariant_VaultOutflowEqualsRecordedPayouts() public view {
        uint256 outflow = FUNDED - usdc.balanceOf(address(vault));
        assertEq(outflow, handler.ghostPaidTotal(), "outflow != recorded payouts");
        uint256 received;
        for (uint256 i; i < 3; ++i) {
            received += usdc.balanceOf(handler.recipients(i));
        }
        assertEq(received, outflow, "recipients did not receive the outflow");
        assertEq(usdc.balanceOf(address(batch)), 0, "BatchPayout holds funds");
    }

    /// I16: every payout id is paid at most once, and the vault agrees.
    function invariant_PayoutIdPaidAtMostOnce() public view {
        for (uint256 i; i < handler.ID_POOL(); ++i) {
            bytes32 id = handler.payoutId(i);
            uint256 times = handler.ghostTimesPaid(id);
            assertLe(times, 1, "a payout id was paid twice");
            assertEq(vault.paid(id), times == 1, "vault.paid disagrees with what was paid");
        }
    }

    /// The daily limit holds through batches, and the vault's per-day
    /// counter matches what was paid that day.
    function invariant_DailyTotalWithinLimit() public view {
        uint256 n = handler.daysSeenCount();
        for (uint256 i; i < n; ++i) {
            uint256 day = handler.daysSeen(i);
            uint256 onDay = vault.paidOnDay(IERC20(address(usdc)), day);
            assertLe(onDay, PER_DAY, "daily limit exceeded");
            assertEq(onDay, handler.ghostPaidOnDay(day), "per-day counter disagrees");
        }
    }

    /// A batch pays all of its items or none of them.
    function invariant_BatchesAllOrNothing() public view {
        assertFalse(handler.ghostAtomicityBroken(), "a batch was partly paid");
    }
}
