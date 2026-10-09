// SPDX-License-Identifier: MIT
pragma solidity 0.8.30;

import {Test} from "forge-std/Test.sol";
import {IAccessControl} from "@openzeppelin/contracts/access/IAccessControl.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {DepositForwarder} from "../src/DepositForwarder.sol";
import {ForwarderFactory} from "../src/ForwarderFactory.sol";
import {MockStablecoin} from "../src/MockStablecoin.sol";
import {Vault} from "../src/Vault.sol";

contract ForwarderFactoryTest is Test {
    MockStablecoin internal usdc;
    Vault internal vault;
    ForwarderFactory internal factory;

    address internal admin = makeAddr("admin");
    address internal sweeper = makeAddr("sweeper");
    address internal payer = makeAddr("payer");

    bytes32 internal constant SALT = keccak256("invoice-0001");

    function setUp() public {
        usdc = new MockStablecoin("Mock USD Coin", "mUSDC");
        vault = new Vault(admin);
        factory = new ForwarderFactory(address(vault), admin);
        bytes32 role = factory.SWEEPER_ROLE();
        vm.prank(admin);
        factory.grantRole(role, sweeper);
    }

    function _pay(bytes32 salt, uint256 amount) internal returns (address deposit) {
        deposit = factory.predict(salt);
        usdc.mint(payer, amount);
        vm.prank(payer);
        assertTrue(usdc.transfer(deposit, amount));
    }

    function test_PredictMatchesDeployed() public {
        address predicted = factory.predict(SALT);
        assertEq(predicted.code.length, 0, "nothing deployed yet");
        vm.prank(sweeper);
        factory.deployAndSweep(SALT, IERC20(address(usdc)));
        assertGt(predicted.code.length, 0, "clone deployed at predicted address");
        assertEq(DepositForwarder(predicted).vault(), address(vault));
    }

    function test_CounterfactualPaymentIsSweptToVault() public {
        address deposit = _pay(SALT, 250_000_000); // 250 USDC before any deployment
        assertEq(usdc.balanceOf(deposit), 250_000_000);

        vm.prank(sweeper);
        uint256 swept = factory.deployAndSweep(SALT, IERC20(address(usdc)));

        assertEq(swept, 250_000_000);
        assertEq(usdc.balanceOf(deposit), 0);
        assertEq(usdc.balanceOf(address(vault)), 250_000_000);
    }

    function test_SecondSweepReusesDeployedForwarder() public {
        _pay(SALT, 100);
        vm.prank(sweeper);
        factory.deployAndSweep(SALT, IERC20(address(usdc)));

        _pay(SALT, 50); // a top-up after the first sweep
        vm.prank(sweeper);
        uint256 swept = factory.deployAndSweep(SALT, IERC20(address(usdc)));

        assertEq(swept, 50);
        assertEq(usdc.balanceOf(address(vault)), 150);
    }

    function test_SweepOfEmptyAddressIsHarmless() public {
        vm.prank(sweeper);
        assertEq(factory.deployAndSweep(SALT, IERC20(address(usdc))), 0);
    }

    function test_RevertWhen_NotSweeper() public {
        _pay(SALT, 100);
        vm.expectRevert(
            abi.encodeWithSelector(
                IAccessControl.AccessControlUnauthorizedAccount.selector,
                payer,
                factory.SWEEPER_ROLE()
            )
        );
        vm.prank(payer);
        factory.deployAndSweep(SALT, IERC20(address(usdc)));
    }

    function test_RevertWhen_ForwarderSweptDirectly() public {
        vm.prank(sweeper);
        factory.deployAndSweep(SALT, IERC20(address(usdc)));
        DepositForwarder fwd = DepositForwarder(factory.predict(SALT));
        vm.expectRevert(DepositForwarder.OnlyFactory.selector);
        vm.prank(payer);
        fwd.sweep(IERC20(address(usdc)));
    }

    function test_RevertWhen_ImplementationSweptDirectly() public {
        DepositForwarder impl = factory.implementation();
        vm.expectRevert(DepositForwarder.OnlyFactory.selector);
        impl.sweep(IERC20(address(usdc)));
    }

    function test_RevertWhen_SweepMovesDifferentAmountThanAnnounced() public {
        address deposit = _pay(SALT, 50);
        // First balanceOf (the factory's, used for the event) reports 100;
        // the second (the forwarder's, used for the transfer) reports the
        // real 50. The factory must refuse rather than log a wrong amount.
        bytes[] memory returns_ = new bytes[](2);
        returns_[0] = abi.encode(uint256(100));
        returns_[1] = abi.encode(uint256(50));
        vm.mockCalls(address(usdc), abi.encodeCall(IERC20.balanceOf, (deposit)), returns_);

        vm.expectRevert(abi.encodeWithSelector(ForwarderFactory.SweepMismatch.selector, 100, 50));
        vm.prank(sweeper);
        factory.deployAndSweep(SALT, IERC20(address(usdc)));
    }

    function testFuzz_DistinctSaltsGiveDistinctAddresses(bytes32 a, bytes32 b) public view {
        vm.assume(a != b);
        assertTrue(factory.predict(a) != factory.predict(b));
    }

    function testFuzz_SweepMovesExactBalance(bytes32 salt, uint128 amount) public {
        address deposit = _pay(salt, amount);
        vm.prank(sweeper);
        uint256 swept = factory.deployAndSweep(salt, IERC20(address(usdc)));
        assertEq(swept, amount);
        assertEq(usdc.balanceOf(deposit), 0);
        assertEq(usdc.balanceOf(address(vault)), amount);
    }
}
