// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

/// @title ReentrancyGuard
/// @notice In-house non-reentrancy guard (storage-based for shanghai EVM compat
///         — no transient storage). Every external value-moving entrypoint in
///         the Vault is `nonReentrant` so a malicious deposit token, DEX router,
///         or payout recipient cannot re-enter mid-accounting.
abstract contract ReentrancyGuard {
  uint256 private constant _NOT_ENTERED = 1;
  uint256 private constant _ENTERED = 2;

  uint256 private _status;

  /// @notice A reentrant call into a `nonReentrant` function was detected.
  error ReentrancyGuardReentrantCall();

  constructor() {
    _status = _NOT_ENTERED;
  }

  modifier nonReentrant() {
    if (_status == _ENTERED) {
      revert ReentrancyGuardReentrantCall();
    }
    _status = _ENTERED;
    _;
    _status = _NOT_ENTERED;
  }

  /// @notice True while a `nonReentrant` call is on the stack.
  function _reentrancyGuardEntered() internal view returns (bool) {
    return _status == _ENTERED;
  }
}
