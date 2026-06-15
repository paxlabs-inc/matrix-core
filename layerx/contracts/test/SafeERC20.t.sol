// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

import { Test } from "forge-std/Test.sol";
import { SafeERC20 } from "../src/lib/SafeERC20.sol";
import { IERC20 } from "../src/interfaces/IERC20.sol";
import { MockERC20 } from "./mocks/MockERC20.sol";
import { MockUSDT } from "./mocks/MockUSDT.sol";

/// @dev Library harness: exposes the internal SafeERC20 functions for testing.
contract SafeERC20Harness {
  using SafeERC20 for IERC20;

  function doTransfer(IERC20 t, address to, uint256 v) external {
    t.safeTransfer(to, v);
  }

  function doTransferFrom(IERC20 t, address f, address to, uint256 v) external {
    t.safeTransferFrom(f, to, v);
  }

  function doForceApprove(IERC20 t, address s, uint256 v) external {
    t.forceApprove(s, v);
  }
}

contract SafeERC20Test is Test {
  SafeERC20Harness h;
  MockERC20 good;
  MockUSDT usdt;
  address alice = address(0xA11CE);
  address bob = address(0xB0B);

  function setUp() public {
    h = new SafeERC20Harness();
    good = new MockERC20("Good", "GD", 6);
    usdt = new MockUSDT();
  }

  function testStandardTransfer() public {
    good.mint(address(h), 1000);
    h.doTransfer(IERC20(address(good)), bob, 400);
    assertEq(good.balanceOf(bob), 400);
  }

  function testNonBoolUSDTTransfer() public {
    usdt.mint(address(h), 1000);
    // USDT.transfer returns nothing; SafeERC20 must treat empty returndata as success.
    h.doTransfer(IERC20(address(usdt)), bob, 400);
    assertEq(usdt.balanceOf(bob), 400);
  }

  function testNonBoolUSDTTransferFrom() public {
    usdt.mint(alice, 1000);
    vm.prank(alice);
    usdt.approve(address(h), 1000);
    h.doTransferFrom(IERC20(address(usdt)), alice, bob, 250);
    assertEq(usdt.balanceOf(bob), 250);
  }

  function testForceApproveResetPath() public {
    // USDT requires reset-to-zero before a new non-zero approval; forceApprove
    // must transparently handle it.
    h.doForceApprove(IERC20(address(usdt)), bob, 500);
    assertEq(usdt.allowance(address(h), bob), 500);
    h.doForceApprove(IERC20(address(usdt)), bob, 800);
    assertEq(usdt.allowance(address(h), bob), 800);
  }

  function testTransferToNonContractTokenReverts() public {
    // Address with no code masquerading as a token must revert, not silently pass.
    vm.expectRevert(
      abi.encodeWithSelector(SafeERC20.SafeERC20FailedOperation.selector, address(0xDEAD))
    );
    h.doTransfer(IERC20(address(0xDEAD)), bob, 1);
  }
}
