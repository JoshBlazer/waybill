// SPDX-License-Identifier: MIT
pragma solidity 0.8.30;

import {Script, console} from "forge-std/Script.sol";
import {VmSafe} from "forge-std/Vm.sol";
import {BatchPayout} from "../src/BatchPayout.sol";
import {Vault} from "../src/Vault.sol";

/// @notice Adds BatchPayout to a network deployed before it existed. Reads
///         the vault from deployments/<chainId>.json, deploys BatchPayout
///         against it, gives it the vault's PAYOUT_ROLE and the broadcaster
///         BATCHER_ROLE, and records its address in the same file.
///
///   forge script script/AddBatchPayout.s.sol --rpc-url $RPC --broadcast --private-key $KEY
///
/// The broadcaster must be the vault's admin. Refuses to run if the file
/// already records a BatchPayout.
contract AddBatchPayout is Script {
    error AlreadyDeployed(address batchPayout);

    function run() external returns (BatchPayout batch) {
        string memory path = string.concat("deployments/", vm.toString(block.chainid), ".json");
        string memory json = vm.readFile(path);
        if (vm.keyExistsJson(json, ".batchPayout")) {
            revert AlreadyDeployed(vm.parseJsonAddress(json, ".batchPayout"));
        }
        Vault vault = Vault(vm.parseJsonAddress(json, ".vault"));

        vm.startBroadcast();
        batch = new BatchPayout(vault, msg.sender);
        vault.grantRole(vault.PAYOUT_ROLE(), address(batch));
        batch.grantRole(batch.BATCHER_ROLE(), msg.sender);
        vm.stopBroadcast();

        console.log("BatchPayout", address(batch));
        // Record the address only when the transactions are really sent:
        // forge runs this code during a simulation too, and a simulated
        // address in the file would make the real run refuse.
        if (!vm.isContext(VmSafe.ForgeContext.ScriptBroadcast)) return batch;
        // writeJson takes a JSON value: the address as a quoted string.
        vm.writeJson(string.concat('"', vm.toString(address(batch)), '"'), path, ".batchPayout");
    }
}
