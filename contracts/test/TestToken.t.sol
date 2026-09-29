// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {Test} from "forge-std/Test.sol";
import {TestToken} from "../src/TestToken.sol";

contract TestTokenTest is Test {
    TestToken internal token;
    address internal victim = makeAddr("victim");
    address internal scammer = makeAddr("scammer");

    event Transfer(address indexed from, address indexed to, uint256 value);
    event Approval(address indexed owner, address indexed spender, uint256 value);

    function setUp() public {
        token = new TestToken();
        token.mint(victim, 1_000 ether);
    }

    function test_MintIncreasesSupplyAndEmitsTransferFromZero() public {
        vm.expectEmit(true, true, false, true, address(token));
        emit Transfer(address(0), scammer, 5 ether);
        token.mint(scammer, 5 ether);
        assertEq(token.totalSupply(), 1_005 ether);
        assertEq(token.balanceOf(scammer), 5 ether);
    }

    function test_MintToZeroAddressReverts() public {
        vm.expectRevert(TestToken.ZeroAddress.selector);
        token.mint(address(0), 1);
    }

    function test_Transfer() public {
        vm.prank(victim);
        assertTrue(token.transfer(scammer, 10 ether));
        assertEq(token.balanceOf(victim), 990 ether);
        assertEq(token.balanceOf(scammer), 10 ether);
    }

    function test_TransferMoreThanBalanceReverts() public {
        vm.prank(victim);
        vm.expectRevert(abi.encodeWithSelector(TestToken.InsufficientBalance.selector, victim, 1_000 ether, 1_001 ether));
        token.transfer(scammer, 1_001 ether);
    }

    function test_ApproveEmitsApprovalWithIndexedOwnerAndSpender() public {
        vm.expectEmit(true, true, false, true, address(token));
        emit Approval(victim, scammer, 42);
        vm.prank(victim);
        token.approve(scammer, 42);
        assertEq(token.allowance(victim, scammer), 42);
    }

    function test_TransferFromSpendsFiniteAllowance() public {
        vm.prank(victim);
        token.approve(scammer, 100 ether);
        vm.prank(scammer);
        token.transferFrom(victim, scammer, 60 ether);
        assertEq(token.allowance(victim, scammer), 40 ether);
    }

    function test_TransferFromBeyondAllowanceReverts() public {
        vm.prank(victim);
        token.approve(scammer, 1 ether);
        vm.prank(scammer);
        vm.expectRevert(abi.encodeWithSelector(TestToken.InsufficientAllowance.selector, scammer, 1 ether, 2 ether));
        token.transferFrom(victim, scammer, 2 ether);
    }

    /// The drain scenario chainwatch exists to catch: one unlimited approval to an
    /// EOA is enough for that EOA to take the whole balance, and the allowance stays
    /// unlimited afterwards.
    function test_UnlimitedApprovalLetsSpenderDrainEverything() public {
        vm.prank(victim);
        token.approve(scammer, type(uint256).max);
        vm.prank(scammer);
        token.transferFrom(victim, scammer, 1_000 ether);
        assertEq(token.balanceOf(victim), 0);
        assertEq(token.balanceOf(scammer), 1_000 ether);
        assertEq(token.allowance(victim, scammer), type(uint256).max);
    }

    function testFuzz_TransferConservesSupply(uint256 amount) public {
        amount = bound(amount, 0, 1_000 ether);
        vm.prank(victim);
        token.transfer(scammer, amount);
        assertEq(token.balanceOf(victim) + token.balanceOf(scammer), token.totalSupply());
    }
}
