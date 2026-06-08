// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

import {Test} from "forge-std/Test.sol";
import {PaymentChannel} from "../src/PaymentChannel.sol";

contract PaymentChannelTest is Test {
  PaymentChannel ch;
  address caller = address(0xCA11);
  address settler = address(0xA11CE);
  address dev = address(0xDEAD);

  function setUp() public {
    ch = new PaymentChannel(caller, settler);
  }

  function testFundAndPayout() public {
    vm.deal(caller, 1 ether);
    vm.prank(caller);
    ch.fund{value: 1 ether}();
    assertEq(ch.availableWei(), 1 ether);

    vm.prank(settler);
    ch.payout(dev, 0.2 ether);
    assertEq(ch.availableWei(), 0.8 ether);
    assertEq(dev.balance, 0.2 ether);
  }

  function testCloseRefunds() public {
    vm.deal(caller, 1 ether);
    vm.prank(caller);
    ch.fund{value: 1 ether}();
    vm.prank(settler);
    ch.payout(dev, 0.3 ether);
    vm.prank(caller);
    ch.close();
    assertEq(caller.balance, 0.7 ether);
  }
}
