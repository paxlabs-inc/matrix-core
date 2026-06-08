// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

/// @title IServiceRegistry
/// @notice On-chain source of truth for Deus service listings (docs/04-onchain.md §4.2).
interface IServiceRegistry {
    struct Service {
        uint256 id;
        address owner;
        address payout;
        bytes32 manifestHash;
        bytes32 pricingHash;
        uint8 status;
        bool hosted;
        bool confidential;
        uint64 registeredAt;
        uint64 updatedAt;
    }

    event ServiceRegistered(
        uint256 indexed id,
        address indexed owner,
        bytes32 manifestHash,
        bytes32 pricingHash,
        bool hosted,
        bool confidential
    );
    event ServiceUpdated(uint256 indexed id, bytes32 manifestHash, bytes32 pricingHash);
    event ServiceStatusChanged(uint256 indexed id, uint8 status);
    event PayoutChanged(uint256 indexed id, address payout);
    event OwnerTransferred(uint256 indexed id, address indexed from, address indexed to);

    function register(
        address payout,
        bytes32 manifestHash,
        bytes32 pricingHash,
        bool hosted,
        bool confidential
    ) external returns (uint256 id);

    function update(uint256 id, bytes32 manifestHash, bytes32 pricingHash) external;

    function setStatus(uint256 id, uint8 status) external;

    function setPayout(uint256 id, address payout) external;

    function transferOwner(uint256 id, address newOwner) external;

    function getService(uint256 id) external view returns (Service memory);

    function ownerOf(uint256 id) external view returns (address);

    function isActive(uint256 id) external view returns (bool);

    function nextId() external view returns (uint256);
}
