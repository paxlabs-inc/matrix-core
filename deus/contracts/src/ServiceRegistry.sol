// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

import {IServiceRegistry} from "./interfaces/IServiceRegistry.sol";

/// @title ServiceRegistry
/// @notice L3 registry for Deus listings; stores hashes and ownership only.
/// @dev Immutable v1. Status: 0 draft, 1 active, 2 paused, 3 delisted.
contract ServiceRegistry is IServiceRegistry {
  uint8 public constant STATUS_DRAFT = 0;
  uint8 public constant STATUS_ACTIVE = 1;
  uint8 public constant STATUS_PAUSED = 2;
  uint8 public constant STATUS_DELISTED = 3;

  mapping(uint256 => Service) public services;
  mapping(address => uint256[]) public servicesByOwner;
  uint256 public nextId;

  address public governor;

  error NotOwner();
  error NotOwnerOrGovernor();
  error InvalidStatus(uint8 status);
  error ServiceNotFound();
  error ZeroAddress();

  modifier onlyOwner(uint256 id) {
    if (services[id].owner != msg.sender) {
      revert NotOwner();
    }
    _;
  }

  constructor(address governor_) {
    if (governor_ == address(0)) {
      revert ZeroAddress();
    }
    governor = governor_;
  }

  /// @inheritdoc IServiceRegistry
  function register(
    address payout,
    bytes32 manifestHash,
    bytes32 pricingHash,
    bool hosted,
    bool confidential
  ) external returns (uint256 id) {
    if (payout == address(0)) {
      revert ZeroAddress();
    }
    id = ++nextId;
    uint64 ts = uint64(block.timestamp);
    services[id] = Service({
      id: id,
      owner: msg.sender,
      payout: payout,
      manifestHash: manifestHash,
      pricingHash: pricingHash,
      status: STATUS_ACTIVE,
      hosted: hosted,
      confidential: confidential,
      registeredAt: ts,
      updatedAt: ts
    });
    servicesByOwner[msg.sender].push(id);
    emit ServiceRegistered(id, msg.sender, manifestHash, pricingHash, hosted, confidential);
  }

  /// @inheritdoc IServiceRegistry
  function update(uint256 id, bytes32 manifestHash, bytes32 pricingHash) external onlyOwner(id) {
    Service storage svc = services[id];
    if (svc.id == 0) {
      revert ServiceNotFound();
    }
    svc.manifestHash = manifestHash;
    svc.pricingHash = pricingHash;
    svc.updatedAt = uint64(block.timestamp);
    emit ServiceUpdated(id, manifestHash, pricingHash);
  }

  /// @inheritdoc IServiceRegistry
  function setStatus(uint256 id, uint8 status) external {
    Service storage svc = services[id];
    if (svc.id == 0) {
      revert ServiceNotFound();
    }
    if (status > STATUS_DELISTED) {
      revert InvalidStatus(status);
    }
    if (msg.sender != svc.owner && msg.sender != governor) {
      revert NotOwnerOrGovernor();
    }
    svc.status = status;
    svc.updatedAt = uint64(block.timestamp);
    emit ServiceStatusChanged(id, status);
  }

  /// @inheritdoc IServiceRegistry
  function setPayout(uint256 id, address payout) external onlyOwner(id) {
    if (payout == address(0)) {
      revert ZeroAddress();
    }
    Service storage svc = services[id];
    if (svc.id == 0) {
      revert ServiceNotFound();
    }
    svc.payout = payout;
    svc.updatedAt = uint64(block.timestamp);
    emit PayoutChanged(id, payout);
  }

  /// @inheritdoc IServiceRegistry
  function transferOwner(uint256 id, address newOwner) external onlyOwner(id) {
    if (newOwner == address(0)) {
      revert ZeroAddress();
    }
    Service storage svc = services[id];
    if (svc.id == 0) {
      revert ServiceNotFound();
    }
    address from = svc.owner;
    svc.owner = newOwner;
    svc.updatedAt = uint64(block.timestamp);
    servicesByOwner[newOwner].push(id);
    emit OwnerTransferred(id, from, newOwner);
  }

  /// @inheritdoc IServiceRegistry
  function getService(uint256 id) external view returns (Service memory) {
    Service memory svc = services[id];
    if (svc.id == 0) {
      revert ServiceNotFound();
    }
    return svc;
  }

  /// @inheritdoc IServiceRegistry
  function ownerOf(uint256 id) external view returns (address) {
    Service memory svc = services[id];
    if (svc.id == 0) {
      revert ServiceNotFound();
    }
    return svc.owner;
  }

  /// @inheritdoc IServiceRegistry
  function isActive(uint256 id) external view returns (bool) {
    Service memory svc = services[id];
    if (svc.id == 0) {
      return false;
    }
    return svc.status == STATUS_ACTIVE;
  }
}
