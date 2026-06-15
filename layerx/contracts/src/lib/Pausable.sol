// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

/// @title Pausable
/// @notice In-house emergency-stop. A guardian can freeze deposits, settlement,
///         and exits if an anomaly is detected. Pause is a higher-trust circuit
///         breaker; the force-exit escape hatch defends against a different
///         threat (a dark/withholding operator), see LayerXVault.
abstract contract Pausable {
  bool private _paused;

  /// @notice Action blocked because the contract is paused.
  error EnforcedPause();
  /// @notice Action blocked because the contract is not paused.
  error ExpectedPause();

  event Paused(address account);
  event Unpaused(address account);

  modifier whenNotPaused() {
    if (_paused) {
      revert EnforcedPause();
    }
    _;
  }

  modifier whenPaused() {
    if (!_paused) {
      revert ExpectedPause();
    }
    _;
  }

  /// @notice True while the contract is paused.
  function paused() public view returns (bool) {
    return _paused;
  }

  function _pause() internal {
    _paused = true;
    emit Paused(msg.sender);
  }

  function _unpause() internal {
    _paused = false;
    emit Unpaused(msg.sender);
  }
}
