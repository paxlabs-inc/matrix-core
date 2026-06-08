// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

import {Test} from "forge-std/Test.sol";
import {SettlementAnchor} from "../src/SettlementAnchor.sol";
contract SettlementAnchorTest is Test {
  SettlementAnchor anchor;
  address settler = address(0xA11CE);
  address governor = address(0xB0B);
  address dev = address(0xDEAD);

  function setUp() public {
    anchor = new SettlementAnchor(settler, governor);
  }

  function testAnchorEmits() public {
    vm.prank(settler);
    anchor.anchor(dev, bytes32(uint256(1)), 100, 2);
  }

  function testRejectNonSettler() public {
    vm.prank(dev);
    vm.expectRevert(SettlementAnchor.NotSettler.selector);
    anchor.anchor(dev, bytes32(0), 1, 1);
  }
}
