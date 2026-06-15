// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

/// @title ISettlementAnchor
/// @notice Immutable, append-only log of settled batch Merkle roots. The Vault
///         records one root per settlement window so an agent can independently
///         verify that the batch_root in its signed inclusion receipt was
///         anchored on Paxeer. Holds no funds.
interface ISettlementAnchor {
  event SettlementAnchored(
    bytes32 indexed batchId,
    bytes32 root,
    uint256 totalSettled,
    uint256 count,
    uint64 windowEnd,
    uint64 anchoredAt
  );

  /// @notice Record a settled batch root. Callable only by the authorized writer
  ///         (the Vault). Idempotent: a given batchId can be anchored once.
  function record(
    bytes32 batchId,
    bytes32 root,
    uint256 totalSettled,
    uint256 count,
    uint64 windowEnd
  ) external;

  /// @notice The anchored root for a batch (zero if never anchored).
  function rootOf(bytes32 batchId) external view returns (bytes32);
}
