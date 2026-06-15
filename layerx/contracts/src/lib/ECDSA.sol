// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

/// @title ECDSA
/// @notice In-house secp256k1 signature recovery with malleability protection.
/// @dev Used to verify the operator's EIP-712 co-signed balance proofs in the
///      force-exit escape hatch. Rejects high-`s` signatures (EIP-2) and a
///      zero recovered address to eliminate signature malleability and the
///      `ecrecover` zero-address pitfall.
library ECDSA {
  /// @notice The signature derives the zero address (invalid).
  error InvalidSignature();
  /// @notice The signature length is not 65 bytes.
  error InvalidSignatureLength();
  /// @notice The signature `s` value is in the upper half order (malleable).
  error InvalidSignatureS();

  /// @dev secp256k1 curve order n / 2, the EIP-2 upper bound for `s`.
  bytes32 private constant _HALF_N =
    0x7FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF5D576E7357A4501DDFE92F46681B20A0;

  /// @notice Recover the signer of `hash` from a packed 65-byte signature.
  function recover(bytes32 hash, bytes memory signature) internal pure returns (address) {
    if (signature.length != 65) {
      revert InvalidSignatureLength();
    }
    bytes32 r;
    bytes32 s;
    uint8 v;
    // solhint-disable-next-line no-inline-assembly
    assembly {
      r := mload(add(signature, 0x20))
      s := mload(add(signature, 0x40))
      v := byte(0, mload(add(signature, 0x60)))
    }
    return recover(hash, v, r, s);
  }

  /// @notice Recover the signer of `hash` from split `(v, r, s)` components.
  function recover(bytes32 hash, uint8 v, bytes32 r, bytes32 s) internal pure returns (address) {
    if (uint256(s) > uint256(_HALF_N)) {
      revert InvalidSignatureS();
    }
    if (v != 27 && v != 28) {
      revert InvalidSignature();
    }
    address signer = ecrecover(hash, v, r, s);
    if (signer == address(0)) {
      revert InvalidSignature();
    }
    return signer;
  }
}
