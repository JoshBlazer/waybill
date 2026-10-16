// SPDX-License-Identifier: MIT
pragma solidity 0.8.30;

import {Test} from "forge-std/Test.sol";
import {IAccessControl} from "@openzeppelin/contracts/access/IAccessControl.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {Pausable} from "@openzeppelin/contracts/utils/Pausable.sol";
import {BatchPayout} from "../src/BatchPayout.sol";
import {MockStablecoin} from "../src/MockStablecoin.sol";
import {Vault} from "../src/Vault.sol";

contract BatchPayoutTest is Test {
    MockStablecoin internal usdc;
    IERC20 internal token;
    Vault internal vault;
    BatchPayout internal batch;

    address internal admin = makeAddr("admin");
    address internal batcher = makeAddr("batcher");
    address internal pauser = makeAddr("pauser");
    address internal alice = makeAddr("alice");
    address internal bob = makeAddr("bob");

    uint128 internal constant PER_TX = 1_000e6;
    uint128 internal constant PER_DAY = 2_500e6;

    function setUp() public {
        usdc = new MockStablecoin("Mock USD Coin", "mUSDC");
        token = IERC20(address(usdc));
        vault = new Vault(admin);
        batch = new BatchPayout(vault, admin);
        vm.startPrank(admin);
        vault.grantRole(vault.PAYOUT_ROLE(), address(batch));
        vault.grantRole(vault.PAUSER_ROLE(), pauser);
        vault.setLimits(token, PER_TX, PER_DAY);
        batch.grantRole(batch.BATCHER_ROLE(), batcher);
        vm.stopPrank();
        usdc.mint(address(vault), 1_000_000e6);
        vm.warp(1_760_000_000);
    }

    function _items2(uint256 a, bytes32 idA, uint256 b, bytes32 idB)
        internal
        view
        returns (BatchPayout.Item[] memory items)
    {
        items = new BatchPayout.Item[](2);
        items[0] = BatchPayout.Item(alice, a, idA);
        items[1] = BatchPayout.Item(bob, b, idB);
    }

    function _pay(bytes32 batchId, BatchPayout.Item[] memory items) internal {
        vm.prank(batcher);
        batch.payBatch(token, batchId, items);
    }

    function test_PaysEveryItemThroughTheVault() public {
        vm.expectEmit(address(batch));
        emit BatchPayout.BatchPaid("b1", address(token), 2, 350e6);
        vm.expectEmit(address(vault));
        emit Vault.Payout("p1", address(token), alice, 100e6);
        vm.expectEmit(address(vault));
        emit Vault.Payout("p2", address(token), bob, 250e6);
        _pay("b1", _items2(100e6, "p1", 250e6, "p2"));

        assertEq(usdc.balanceOf(alice), 100e6);
        assertEq(usdc.balanceOf(bob), 250e6);
        assertEq(usdc.balanceOf(address(batch)), 0, "batch contract holds nothing");
        assertTrue(vault.paid("p1") && vault.paid("p2") && batch.batchPaid("b1"));
    }

    function test_RevertWhen_BatchResubmitted() public {
        _pay("b1", _items2(100e6, "p1", 100e6, "p2"));
        vm.expectRevert(abi.encodeWithSelector(BatchPayout.BatchIdReused.selector, bytes32("b1")));
        _pay("b1", _items2(100e6, "p1", 100e6, "p2"));
    }

    /// The same payouts under a new batch id: the vault's payout-id guard
    /// stops them (I16).
    function test_RevertWhen_PayoutIdReusedAcrossBatches() public {
        _pay("b1", _items2(100e6, "p1", 100e6, "p2"));
        vm.expectRevert(abi.encodeWithSelector(Vault.PayoutIdReused.selector, bytes32("p2")));
        _pay("b2", _items2(100e6, "p3", 100e6, "p2"));
        assertFalse(vault.paid("p3"), "the earlier item was rolled back");
        assertEq(usdc.balanceOf(alice), 100e6);
    }

    function test_RevertWhen_PayoutIdRepeatedWithinBatch() public {
        vm.expectRevert(abi.encodeWithSelector(Vault.PayoutIdReused.selector, bytes32("p1")));
        _pay("b1", _items2(100e6, "p1", 100e6, "p1"));
        assertEq(usdc.balanceOf(alice) + usdc.balanceOf(bob), 0);
        assertFalse(batch.batchPaid("b1"), "a failed batch can be fixed and retried");
    }

    function test_RevertWhen_AnyItemBreaksALimit_NothingIsPaid() public {
        vm.expectRevert(
            abi.encodeWithSelector(Vault.OverTransactionLimit.selector, 1_001e6, PER_TX)
        );
        _pay("b1", _items2(100e6, "p1", 1_001e6, "p2"));
        assertEq(usdc.balanceOf(alice), 0);
        assertFalse(vault.paid("p1"));

        // Three items of 900 add up past the 2,500 daily limit.
        BatchPayout.Item[] memory items = new BatchPayout.Item[](3);
        items[0] = BatchPayout.Item(alice, 900e6, "q1");
        items[1] = BatchPayout.Item(bob, 900e6, "q2");
        items[2] = BatchPayout.Item(alice, 900e6, "q3");
        vm.expectRevert(abi.encodeWithSelector(Vault.OverDailyLimit.selector, 2_700e6, PER_DAY));
        _pay("b2", items);
        assertEq(vault.paidOnDay(token, block.timestamp / 1 days), 0);
    }

    function test_RevertWhen_VaultPaused() public {
        vm.prank(pauser);
        vault.pause();
        vm.expectRevert(Pausable.EnforcedPause.selector);
        _pay("b1", _items2(100e6, "p1", 100e6, "p2"));
    }

    function test_RevertWhen_NotBatcher() public {
        BatchPayout.Item[] memory items = _items2(100e6, "p1", 100e6, "p2");
        vm.expectRevert(
            abi.encodeWithSelector(
                IAccessControl.AccessControlUnauthorizedAccount.selector,
                alice,
                batch.BATCHER_ROLE()
            )
        );
        vm.prank(alice);
        batch.payBatch(token, "b1", items);
    }

    function test_RevertWhen_BatchContractLacksPayoutRole() public {
        vm.startPrank(admin);
        vault.revokeRole(vault.PAYOUT_ROLE(), address(batch));
        vm.stopPrank();
        vm.expectRevert(
            abi.encodeWithSelector(
                IAccessControl.AccessControlUnauthorizedAccount.selector,
                address(batch),
                vault.PAYOUT_ROLE()
            )
        );
        _pay("b1", _items2(100e6, "p1", 100e6, "p2"));
    }

    function test_RevertWhen_EmptyOrTooLarge() public {
        vm.expectRevert(BatchPayout.EmptyBatch.selector);
        _pay("b1", new BatchPayout.Item[](0));

        uint256 n = batch.MAX_ITEMS() + 1;
        BatchPayout.Item[] memory items = new BatchPayout.Item[](n);
        vm.expectRevert(abi.encodeWithSelector(BatchPayout.TooManyItems.selector, n, n - 1));
        _pay("b2", items);
    }

    function test_RevertWhen_ConstructedWithZeroAddress() public {
        vm.expectRevert(BatchPayout.ZeroAddress.selector);
        new BatchPayout(Vault(address(0)), admin);
        vm.expectRevert(BatchPayout.ZeroAddress.selector);
        new BatchPayout(vault, address(0));
    }
}
