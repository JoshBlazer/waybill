// SPDX-License-Identifier: MIT
pragma solidity 0.8.30;

import {Test} from "forge-std/Test.sol";
import {IAccessControl} from "@openzeppelin/contracts/access/IAccessControl.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {Pausable} from "@openzeppelin/contracts/utils/Pausable.sol";
import {MockStablecoin} from "../src/MockStablecoin.sol";
import {Vault} from "../src/Vault.sol";

contract VaultTest is Test {
    MockStablecoin internal usdc;
    IERC20 internal token;
    Vault internal vault;

    address internal admin = makeAddr("admin");
    address internal payer = makeAddr("payoutKey");
    address internal pauser = makeAddr("pauser");
    address internal alice = makeAddr("alice");

    uint128 internal constant PER_TX = 1_000e6; // 1,000 USDC
    uint128 internal constant PER_DAY = 2_500e6; // 2,500 USDC

    function setUp() public {
        usdc = new MockStablecoin("Mock USD Coin", "mUSDC");
        token = IERC20(address(usdc));
        vault = new Vault(admin);
        vm.startPrank(admin);
        vault.grantRole(vault.PAYOUT_ROLE(), payer);
        vault.grantRole(vault.PAUSER_ROLE(), pauser);
        vault.setLimits(token, PER_TX, PER_DAY);
        vm.stopPrank();
        usdc.mint(address(vault), 1_000_000e6);
        vm.warp(1_760_000_000);
    }

    function _payout(uint256 amount, bytes32 id) internal {
        vm.prank(payer);
        vault.payout(token, alice, amount, id);
    }

    function test_PayoutWithinLimits() public {
        _payout(500e6, "p1");
        assertEq(usdc.balanceOf(alice), 500e6);
        assertTrue(vault.paid("p1"));
    }

    function test_RevertWhen_PayoutIdReused() public {
        _payout(1e6, "p1");
        vm.expectRevert(abi.encodeWithSelector(Vault.PayoutIdReused.selector, bytes32("p1")));
        _payout(1e6, "p1");
        assertEq(usdc.balanceOf(alice), 1e6, "paid once, not twice");
    }

    function test_RevertWhen_OverTransactionLimit() public {
        vm.expectRevert(
            abi.encodeWithSelector(Vault.OverTransactionLimit.selector, PER_TX + 1, PER_TX)
        );
        _payout(PER_TX + 1, "p1");
    }

    function test_RevertWhen_OverDailyLimit() public {
        _payout(1_000e6, "p1");
        _payout(1_000e6, "p2");
        vm.expectRevert(abi.encodeWithSelector(Vault.OverDailyLimit.selector, 3_000e6, PER_DAY));
        _payout(1_000e6, "p3");
    }

    function test_DailyLimitResetsNextDay() public {
        _payout(1_000e6, "p1");
        _payout(1_000e6, "p2");
        vm.warp(block.timestamp + 1 days);
        _payout(1_000e6, "p3");
        assertEq(usdc.balanceOf(alice), 3_000e6);
    }

    function test_RevertWhen_TokenHasNoLimits() public {
        MockStablecoin other = new MockStablecoin("Other", "OTH");
        other.mint(address(vault), 100);
        vm.expectRevert(abi.encodeWithSelector(Vault.OverTransactionLimit.selector, 1, 0));
        vm.prank(payer);
        vault.payout(IERC20(address(other)), alice, 1, "p1");
    }

    function test_RevertWhen_NotPayoutRole() public {
        vm.expectRevert(
            abi.encodeWithSelector(
                IAccessControl.AccessControlUnauthorizedAccount.selector, alice, vault.PAYOUT_ROLE()
            )
        );
        vm.prank(alice);
        vault.payout(token, alice, 1, "p1");
    }

    function test_PauseStopsPayoutsAndOnlyAdminUnpauses() public {
        vm.prank(pauser);
        vault.pause();

        vm.expectRevert(Pausable.EnforcedPause.selector);
        _payout(1e6, "p1");

        vm.expectRevert(
            abi.encodeWithSelector(
                IAccessControl.AccessControlUnauthorizedAccount.selector,
                pauser,
                vault.DEFAULT_ADMIN_ROLE()
            )
        );
        vm.prank(pauser);
        vault.unpause();

        vm.prank(admin);
        vault.unpause();
        _payout(1e6, "p1");
        assertEq(usdc.balanceOf(alice), 1e6);
    }

    function test_RevertWhen_ZeroAmountOrZeroRecipient() public {
        vm.expectRevert(Vault.ZeroAmount.selector);
        _payout(0, "p1");
        vm.expectRevert(Vault.ZeroAddress.selector);
        vm.prank(payer);
        vault.payout(token, address(0), 1, "p2");
    }

    /// Whatever sequence of amounts is attempted in one day, the total paid
    /// never exceeds the daily limit and no single payout exceeds per-tx.
    /// Amounts are bounded to straddle both limits so every branch is hit.
    function testFuzz_DailyTotalNeverExceedsLimit(uint256[12] calldata raw) public {
        uint256 before = usdc.balanceOf(address(vault));
        for (uint256 i = 0; i < raw.length; i++) {
            uint256 amount = bound(raw[i], 0, 2 * uint256(PER_TX));
            vm.prank(payer);
            try vault.payout(token, alice, amount, bytes32(i + 1)) {
                assertLe(amount, PER_TX);
            } catch {}
        }
        uint256 out = before - usdc.balanceOf(address(vault));
        assertLe(out, PER_DAY);
        assertEq(out, usdc.balanceOf(alice));
        assertEq(out, vault.paidOnDay(token, block.timestamp / 1 days));
    }
}
