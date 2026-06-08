// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

interface ISettlementAnchor {
  event SettlementAnchored(
    address indexed developer,
    bytes32 receiptsRoot,
    uint256 totalWei,
    uint256 count,
    uint64 windowEnd
  );

  function anchor(address developer, bytes32 receiptsRoot, uint256 totalWei, uint256 count) external;
}
