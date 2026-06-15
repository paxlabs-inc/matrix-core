// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

import { ISettlementAnchor } from "./interfaces/ISettlementAnchor.sol";
import { Governed } from "./lib/Governed.sol";

/// @title SettlementAnchor
/// @notice Immutable, append-only registry of settled batch Merkle roots for
///         LayerX. The Vault (the authorized `writer`) records exactly one root
///         per settlement window. Anchoring roots on Paxeer is what makes a
///         signed inclusion receipt independently verifiable and stops the
///         sequencer from equivocating undetected (frozen spec [receipts]).
/// @dev Holds NO funds. Custody and all value movement live in LayerXVault; this
///      contract is deliberately minimal so the historical root log survives any
///      future vault migration (the governor can rotate the writer).
contract SettlementAnchor is ISettlementAnchor, Governed {
  /// @notice The only address permitted to record roots (the Vault).
  address public writer;

  /// @inheritdoc ISettlementAnchor
  mapping(bytes32 => bytes32) public rootOf;
  /// @notice Whether a batchId has been anchored (idempotency, independent of root value).
  mapping(bytes32 => bool) public anchored;

  /// @notice Caller is not the authorized writer.
  error NotWriter();
  /// @notice This batchId already has an anchored root.
  error AlreadyAnchored();
  /// @notice A settled batch must carry a non-zero Merkle root.
  error ZeroRoot();

  event WriterUpdated(address indexed previous, address indexed current);

  modifier onlyWriter() {
    if (msg.sender != writer) {
      revert NotWriter();
    }
    _;
  }

  /// @param governor_ protocol root authority (can rotate the writer)
  /// @param writer_ the Vault address allowed to anchor (may be set later if zero)
  constructor(address governor_, address writer_) Governed(governor_) {
    writer = writer_;
    emit WriterUpdated(address(0), writer_);
  }

  /// @notice Point the anchor at a (new) Vault. Used at wiring time and for
  ///         vault migration; the immutable root history is preserved.
  function setWriter(address newWriter) external onlyGovernor {
    if (newWriter == address(0)) {
      revert ZeroAddress();
    }
    address previous = writer;
    writer = newWriter;
    emit WriterUpdated(previous, newWriter);
  }

  /// @inheritdoc ISettlementAnchor
  function record(
    bytes32 batchId,
    bytes32 root,
    uint256 totalSettled,
    uint256 count,
    uint64 windowEnd
  ) external onlyWriter {
    if (anchored[batchId]) {
      revert AlreadyAnchored();
    }
    if (root == bytes32(0)) {
      revert ZeroRoot();
    }
    anchored[batchId] = true;
    rootOf[batchId] = root;
    emit SettlementAnchored(batchId, root, totalSettled, count, windowEnd, uint64(block.timestamp));
  }
}
