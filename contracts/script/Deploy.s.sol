// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {Script, console} from "forge-std/Script.sol";
import {ProofLedger} from "../src/ProofLedger.sol";

/// Usage (local Anvil):
///   forge script script/Deploy.s.sol --rpc-url local --broadcast --private-key $CHAIN_PRIVATE_KEY
contract Deploy is Script {
    function run() external {
        vm.startBroadcast();
        // The deployer is also the first relayer (the backend uses the same key to anchor).
        ProofLedger ledger = new ProofLedger(msg.sender);
        vm.stopBroadcast();
        console.log("ProofLedger deployed at:", address(ledger));
    }
}
