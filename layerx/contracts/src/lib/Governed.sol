// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

/// @title Governed
/// @notice In-house two-step ownership (governor). Two-step transfer prevents
///         an irrecoverable handoff to a wrong/dead address — the new governor
///         must explicitly accept. The governor is the protocol root authority
///         (parameters, role rotation, pause) but is NEVER able to move agent
///         funds arbitrarily; custody invariants live in LayerXVault.
abstract contract Governed {
  address public governor;
  address public pendingGovernor;

  /// @notice Caller is not the governor.
  error NotGovernor();
  /// @notice Caller is not the pending governor.
  error NotPendingGovernor();
  /// @notice A zero address was supplied where a real address is required.
  error ZeroAddress();

  event GovernanceTransferStarted(
    address indexed previousGovernor, address indexed pendingGovernor
  );
  event GovernanceTransferred(address indexed previousGovernor, address indexed newGovernor);

  modifier onlyGovernor() {
    if (msg.sender != governor) {
      revert NotGovernor();
    }
    _;
  }

  constructor(address governor_) {
    if (governor_ == address(0)) {
      revert ZeroAddress();
    }
    governor = governor_;
    emit GovernanceTransferred(address(0), governor_);
  }

  /// @notice Begin transferring governance to `newGovernor` (step 1 of 2).
  function transferGovernance(address newGovernor) external onlyGovernor {
    if (newGovernor == address(0)) {
      revert ZeroAddress();
    }
    pendingGovernor = newGovernor;
    emit GovernanceTransferStarted(governor, newGovernor);
  }

  /// @notice Accept governance (step 2 of 2). Must be called by the pending governor.
  function acceptGovernance() external {
    if (msg.sender != pendingGovernor) {
      revert NotPendingGovernor();
    }
    address previous = governor;
    governor = pendingGovernor;
    pendingGovernor = address(0);
    emit GovernanceTransferred(previous, governor);
  }
}
