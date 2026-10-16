// SPDX-License-Identifier: MIT
pragma solidity 0.8.30;

import {Script, console} from "forge-std/Script.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {BatchPayout} from "../src/BatchPayout.sol";
import {ForwarderFactory} from "../src/ForwarderFactory.sol";
import {MockStablecoin} from "../src/MockStablecoin.sol";
import {Vault} from "../src/Vault.sol";

/// @notice Deploys the mock stablecoin, vault, forwarder factory and batch
///         payout contract to a test network. The broadcaster becomes admin,
///         sweeper, batcher and payout key; that is acceptable on test
///         networks only, and MockStablecoin refuses to deploy anywhere else.
///
///   forge script script/Deploy.s.sol --rpc-url $RPC --broadcast --private-key $KEY
///
/// If WAYBILL_SIGNER_KEY is set (the local Compose stack generates one per
/// machine), that key's address also gets the sweeper and batcher roles
/// and 100 ETH, so the services sign with their own key rather than the
/// deployer's. The key itself is only read to derive its address.
///
/// Writes the addresses to deployments/<chainId>.json, which the Go services
/// load and verify against the chain at startup.
contract Deploy is Script {
    uint128 internal constant PER_TX = 10_000e6; // 10,000 mUSDC
    uint128 internal constant PER_DAY = 50_000e6; // 50,000 mUSDC

    function run()
        external
        returns (MockStablecoin usdc, Vault vault, ForwarderFactory factory, BatchPayout batch)
    {
        // The block the script simulates against. The deployment transactions
        // land in it or later, so the watcher can safely start here.
        uint256 startBlock = block.number;
        uint256 signerKey = vm.envOr("WAYBILL_SIGNER_KEY", uint256(0));
        vm.startBroadcast();
        address operator = msg.sender;

        usdc = new MockStablecoin("Waybill Mock USD Coin", "mUSDC");
        vault = new Vault(operator);
        factory = new ForwarderFactory(address(vault), operator);
        batch = new BatchPayout(vault, operator);

        vault.grantRole(vault.PAYOUT_ROLE(), operator);
        vault.grantRole(vault.PAYOUT_ROLE(), address(batch));
        vault.grantRole(vault.PAUSER_ROLE(), operator);
        vault.setLimits(IERC20(address(usdc)), PER_TX, PER_DAY);
        factory.grantRole(factory.SWEEPER_ROLE(), operator);
        batch.grantRole(batch.BATCHER_ROLE(), operator);

        if (signerKey != 0) {
            address signer = vm.addr(signerKey);
            factory.grantRole(factory.SWEEPER_ROLE(), signer);
            batch.grantRole(batch.BATCHER_ROLE(), signer);
            payable(signer).transfer(100 ether);
            console.log("Service signer  ", signer);
        }

        vm.stopBroadcast();

        string memory key = "deployment";
        vm.serializeUint(key, "chainId", block.chainid);
        vm.serializeUint(key, "startBlock", startBlock);
        vm.serializeAddress(key, "token", address(usdc));
        vm.serializeAddress(key, "vault", address(vault));
        vm.serializeAddress(key, "batchPayout", address(batch));
        vm.serializeAddress(key, "implementation", address(factory.implementation()));
        string memory json = vm.serializeAddress(key, "factory", address(factory));
        vm.writeJson(json, string.concat("deployments/", vm.toString(block.chainid), ".json"));

        console.log("chain id        ", block.chainid);
        console.log("MockStablecoin  ", address(usdc));
        console.log("Vault           ", address(vault));
        console.log("ForwarderFactory", address(factory));
        console.log("Forwarder impl  ", address(factory.implementation()));
        console.log("BatchPayout     ", address(batch));
    }
}
