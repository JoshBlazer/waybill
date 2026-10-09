// SPDX-License-Identifier: MIT
pragma solidity 0.8.30;

import {Test} from "forge-std/Test.sol";
import {MockStablecoin} from "../src/MockStablecoin.sol";

contract MockStablecoinTest is Test {
    MockStablecoin internal token;
    address internal alice = makeAddr("alice");
    address internal bob = makeAddr("bob");

    function setUp() public {
        token = new MockStablecoin("Mock USD Coin", "mUSDC");
    }

    function test_HasSixDecimals() public view {
        assertEq(token.decimals(), 6);
    }

    function test_MintAndTransferInMinorUnits() public {
        token.mint(alice, 125_500_000); // 125.50 tokens
        vm.prank(alice);
        assertTrue(token.transfer(bob, 25_500_000));
        assertEq(token.balanceOf(alice), 100_000_000);
        assertEq(token.balanceOf(bob), 25_500_000);
        assertEq(token.totalSupply(), 125_500_000);
    }

    function testFuzz_MintIncreasesSupplyExactly(address to, uint128 amount) public {
        vm.assume(to != address(0));
        token.mint(to, amount);
        assertEq(token.balanceOf(to), amount);
        assertEq(token.totalSupply(), amount);
    }

    function test_RevertWhen_DeployedOnMainnet() public {
        vm.chainId(1);
        vm.expectRevert(abi.encodeWithSelector(MockStablecoin.NotATestNetwork.selector, 1));
        new MockStablecoin("Mock USD Coin", "mUSDC");
    }

    function test_RevertWhen_MintingOnMainnet() public {
        vm.chainId(8453); // Base mainnet
        vm.expectRevert(abi.encodeWithSelector(MockStablecoin.NotATestNetwork.selector, 8453));
        token.mint(alice, 1);
    }
}
