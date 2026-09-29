// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {Script, console} from "forge-std/Script.sol";
import {TestToken} from "../src/TestToken.sol";

/// Deploys TestToken and mints a balance to the victim wallet.
/// Env: DEPLOYER_KEY (private key), VICTIM (address), MINT_AMOUNT (wei, optional).
contract Deploy is Script {
    function run() external returns (TestToken token) {
        uint256 deployerKey = vm.envUint("DEPLOYER_KEY");
        address victim = vm.envAddress("VICTIM");
        uint256 amount = vm.envOr("MINT_AMOUNT", uint256(1_000 ether));

        vm.startBroadcast(deployerKey);
        token = new TestToken();
        token.mint(victim, amount);
        vm.stopBroadcast();

        console.log("TestToken deployed at", address(token));
    }
}
