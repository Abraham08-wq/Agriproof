// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {Test} from "forge-std/Test.sol";
import {ProofLedger} from "../src/ProofLedger.sol";

contract ProofLedgerTest is Test {
    ProofLedger ledger;
    address relayer = address(0xBEEF);
    address stranger = address(0xBAD);
    bytes32 group = keccak256("group-1");

    function setUp() public {
        ledger = new ProofLedger(relayer);
    }

    function test_RelayerCanAnchor() public {
        vm.prank(relayer);
        ledger.anchor(group, keccak256("head-1"), 1);
        (bytes32 head, uint64 entries,) = ledger.latest(group);
        assertEq(head, keccak256("head-1"));
        assertEq(entries, 1);
    }

    function test_StrangerCannotAnchor() public {
        vm.prank(stranger);
        vm.expectRevert(ProofLedger.NotRelayer.selector);
        ledger.anchor(group, keccak256("head-1"), 1);
    }

    function test_CannotRollBackHistory() public {
        vm.startPrank(relayer);
        ledger.anchor(group, keccak256("head-5"), 5);
        vm.expectRevert(abi.encodeWithSelector(ProofLedger.NonIncreasingEntries.selector, uint64(5), uint64(3)));
        ledger.anchor(group, keccak256("fake-head"), 3);
        vm.expectRevert(abi.encodeWithSelector(ProofLedger.NonIncreasingEntries.selector, uint64(5), uint64(5)));
        ledger.anchor(group, keccak256("fake-head"), 5);
        vm.stopPrank();
    }

    function test_OnlyOwnerSetsRelayer() public {
        vm.prank(stranger);
        vm.expectRevert(ProofLedger.NotOwner.selector);
        ledger.setRelayer(stranger, true);

        ledger.setRelayer(stranger, true);
        assertTrue(ledger.relayers(stranger));
    }

    function testFuzz_EntriesMustIncrease(uint64 a, uint64 b) public {
        vm.assume(a > 0);
        vm.startPrank(relayer);
        ledger.anchor(group, bytes32(uint256(1)), a);
        if (b <= a) {
            vm.expectRevert();
        }
        ledger.anchor(group, bytes32(uint256(2)), b);
        vm.stopPrank();
    }
}
