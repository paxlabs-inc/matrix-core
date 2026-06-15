// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

import { MockERC20 } from "./MockERC20.sol";

/// @notice A malicious token that attempts to re-enter the Vault during a
///         `transferFrom` (i.e. mid-deposit). Used to prove `nonReentrant`
///         blocks reentrancy on the value-moving entrypoints.
contract ReentrantToken is MockERC20 {
  address public target;
  bytes public payload;
  bool public armed;
  bool public reentryAttempted;
  bool public reentrySucceeded;

  constructor() MockERC20("Reentrant", "RE", 6) { }

  function arm(address target_, bytes calldata payload_) external {
    target = target_;
    payload = payload_;
    armed = true;
  }

  function transferFrom(address from, address to, uint256 value) public override returns (bool) {
    if (armed) {
      armed = false; // single shot
      reentryAttempted = true;
      // Attempt to re-enter the Vault mid-deposit. We record whether it
      // succeeded rather than bubbling, so the test can assert the guard blocked
      // it (SafeERC20 would otherwise mask the inner revert reason).
      (bool ok,) = target.call(payload);
      reentrySucceeded = ok;
    }
    return super.transferFrom(from, to, value);
  }
}
