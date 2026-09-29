// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {Script, console} from "forge-std/Script.sol";
import {TestToken} from "../src/TestToken.sol";

/// Plays the classic approval drain on a local chain:
///   1. the victim signs an unlimited `approve` to an attacker-controlled EOA;
///   2. the attacker later calls `transferFrom` and takes the whole balance.
/// Env: TOKEN (address), VICTIM_KEY and SCAMMER_KEY (private keys).
contract Drain is Script {
    function run() external {
        TestToken token = TestToken(vm.envAddress("TOKEN"));
        uint256 victimKey = vm.envUint("VICTIM_KEY");
        uint256 scammerKey = vm.envUint("SCAMMER_KEY");
        address victim = vm.addr(victimKey);
        address scammer = vm.addr(scammerKey);

        vm.startBroadcast(victimKey);
        token.approve(scammer, type(uint256).max);
        vm.stopBroadcast();

        uint256 balance = token.balanceOf(victim);
        vm.startBroadcast(scammerKey);
        token.transferFrom(victim, scammer, balance);
        vm.stopBroadcast();

        console.log("drained", balance, "from", victim);
    }
}
