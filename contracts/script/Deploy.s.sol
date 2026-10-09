// SPDX-License-Identifier: MIT
pragma solidity 0.8.30;

import {Script, console} from "forge-std/Script.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {ForwarderFactory} from "../src/ForwarderFactory.sol";
import {MockStablecoin} from "../src/MockStablecoin.sol";
import {Vault} from "../src/Vault.sol";

/// @notice Deploys the mock stablecoin, vault and forwarder factory to a test
///         network. The broadcaster becomes admin, sweeper and payout key;
///         that is acceptable on test networks only, and MockStablecoin
///         refuses to deploy anywhere else.
///
///   forge script script/Deploy.s.sol --rpc-url $RPC --broadcast --private-key $KEY
contract Deploy is Script {
    uint128 internal constant PER_TX = 10_000e6; // 10,000 mUSDC
    uint128 internal constant PER_DAY = 50_000e6; // 50,000 mUSDC

    function run() external returns (MockStablecoin usdc, Vault vault, ForwarderFactory factory) {
        vm.startBroadcast();
        address operator = msg.sender;

        usdc = new MockStablecoin("Waybill Mock USD Coin", "mUSDC");
        vault = new Vault(operator);
        factory = new ForwarderFactory(address(vault), operator);

        vault.grantRole(vault.PAYOUT_ROLE(), operator);
        vault.grantRole(vault.PAUSER_ROLE(), operator);
        vault.setLimits(IERC20(address(usdc)), PER_TX, PER_DAY);
        factory.grantRole(factory.SWEEPER_ROLE(), operator);

        vm.stopBroadcast();

        console.log("chain id        ", block.chainid);
        console.log("MockStablecoin  ", address(usdc));
        console.log("Vault           ", address(vault));
        console.log("ForwarderFactory", address(factory));
        console.log("Forwarder impl  ", address(factory.implementation()));
    }
}
