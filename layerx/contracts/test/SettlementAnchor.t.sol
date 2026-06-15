// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

import { Test } from "forge-std/Test.sol";
import { SettlementAnchor } from "../src/SettlementAnchor.sol";
import { Governed } from "../src/lib/Governed.sol";

contract SettlementAnchorTest is Test {
  SettlementAnchor anchor;
  address governor = makeAddr("governor");
  address vault = makeAddr("vault");
  address stranger = makeAddr("stranger");

  function setUp() public {
    anchor = new SettlementAnchor(governor, vault);
  }

  function testRecordByWriter() public {
    vm.prank(vault);
    anchor.record(bytes32("b1"), bytes32(uint256(1)), 500, 3, uint64(block.timestamp));
    assertEq(anchor.rootOf(bytes32("b1")), bytes32(uint256(1)));
    assertTrue(anchor.anchored(bytes32("b1")));
  }

  function testRejectNonWriter() public {
    vm.prank(stranger);
    vm.expectRevert(SettlementAnchor.NotWriter.selector);
    anchor.record(bytes32("b1"), bytes32(uint256(1)), 1, 1, uint64(block.timestamp));
  }

  function testIdempotentRecord() public {
    vm.startPrank(vault);
    anchor.record(bytes32("b1"), bytes32(uint256(1)), 1, 1, uint64(block.timestamp));
    vm.expectRevert(SettlementAnchor.AlreadyAnchored.selector);
    anchor.record(bytes32("b1"), bytes32(uint256(2)), 1, 1, uint64(block.timestamp));
    vm.stopPrank();
  }

  function testRejectZeroRoot() public {
    vm.prank(vault);
    vm.expectRevert(SettlementAnchor.ZeroRoot.selector);
    anchor.record(bytes32("b1"), bytes32(0), 1, 1, uint64(block.timestamp));
  }

  function testGovernorRotatesWriter() public {
    address newVault = address(0xBEEF);
    vm.prank(governor);
    anchor.setWriter(newVault);
    assertEq(anchor.writer(), newVault);

    vm.prank(newVault);
    anchor.record(bytes32("b2"), bytes32(uint256(9)), 1, 1, uint64(block.timestamp));
    assertEq(anchor.rootOf(bytes32("b2")), bytes32(uint256(9)));
  }

  function testNonGovernorCannotRotateWriter() public {
    vm.prank(stranger);
    vm.expectRevert(Governed.NotGovernor.selector);
    anchor.setWriter(stranger);
  }
}
