// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

import { MockERC20 } from "./MockERC20.sol";

/// @notice Minimal WETH9-style wrapped native token for tests (WPAX9 stand-in).
contract MockWrappedNative is MockERC20 {
  constructor() MockERC20("Wrapped PAX", "WPAX9", 18) { }

  function deposit() external payable {
    balanceOf[msg.sender] += msg.value;
    totalSupply += msg.value;
    emit Transfer(address(0), msg.sender, msg.value);
  }

  function withdraw(uint256 amount) external {
    balanceOf[msg.sender] -= amount;
    totalSupply -= amount;
    (bool ok,) = msg.sender.call{ value: amount }("");
    require(ok, "WPAX9: native transfer failed");
    emit Transfer(msg.sender, address(0), amount);
  }
}
