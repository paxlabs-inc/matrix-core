// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

import { IERC20 } from "./IERC20.sol";

/// @title IWrappedNative
/// @notice WPAX9 (Wrapped PAX) interface for wrapping native PAX before swapping
///         it to USDL on deposit. Matches the canonical WETH9 deposit/withdraw
///         shape. WPAX9 = 0xe5ccf339d1c89c7e6c6768b28507f78b861fc1de on chain 125.
interface IWrappedNative is IERC20 {
  /// @notice Wrap the attached native value into the wrapped token 1:1.
  function deposit() external payable;

  /// @notice Unwrap `amount` of the wrapped token back to native value 1:1.
  function withdraw(uint256 amount) external;
}
