// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

/// @title EIP712
/// @notice In-house EIP-712 typed-data domain hashing. The domain separator is
///         cached at construction and re-derived if the chain id changes (fork
///         safety) so a signature can never be replayed across a chain split.
/// @dev No external dependency. Domain = (name, version, chainId, verifyingContract).
abstract contract EIP712 {
  bytes32 private constant _TYPE_HASH =
    keccak256("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)");

  bytes32 private immutable _hashedName;
  bytes32 private immutable _hashedVersion;
  bytes32 private immutable _cachedDomainSeparator;
  uint256 private immutable _cachedChainId;
  address private immutable _cachedThis;

  constructor(string memory name, string memory version) {
    _hashedName = keccak256(bytes(name));
    _hashedVersion = keccak256(bytes(version));
    _cachedChainId = block.chainid;
    _cachedThis = address(this);
    _cachedDomainSeparator = _buildDomainSeparator();
  }

  /// @notice The current EIP-712 domain separator (fork-safe).
  function domainSeparatorV4() public view returns (bytes32) {
    if (address(this) == _cachedThis && block.chainid == _cachedChainId) {
      return _cachedDomainSeparator;
    }
    return _buildDomainSeparator();
  }

  function _buildDomainSeparator() private view returns (bytes32) {
    return
      keccak256(abi.encode(_TYPE_HASH, _hashedName, _hashedVersion, block.chainid, address(this)));
  }

  /// @notice Produce the EIP-712 digest for a given struct hash.
  function _hashTypedDataV4(bytes32 structHash) internal view returns (bytes32) {
    return keccak256(abi.encodePacked(hex"1901", domainSeparatorV4(), structHash));
  }
}
