// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

import {ISettlementAnchor} from "./interfaces/ISettlementAnchor.sol";

/// @title SettlementAnchor
/// @notice Records merkle roots of settled receipt batches per developer.
contract SettlementAnchor is ISettlementAnchor {
  address public settler;
  address public governor;

  error NotSettler();
  error ZeroAddress();

  modifier onlySettler() {
    if (msg.sender != settler) {
      revert NotSettler();
    }
    _;
  }

  constructor(address settler_, address governor_) {
    if (settler_ == address(0) || governor_ == address(0)) {
      revert ZeroAddress();
    }
    settler = settler_;
    governor = governor_;
  }

  function setSettler(address newSettler) external {
    if (msg.sender != governor) {
      revert NotSettler();
    }
    if (newSettler == address(0)) {
      revert ZeroAddress();
    }
    settler = newSettler;
  }

  /// @inheritdoc ISettlementAnchor
  function anchor(address developer, bytes32 receiptsRoot, uint256 totalWei, uint256 count) external onlySettler {
    emit SettlementAnchored(developer, receiptsRoot, totalWei, count, uint64(block.timestamp));
  }
}
