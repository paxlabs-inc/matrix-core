// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

/// @title IPECORRouter
/// @notice Minimal interface to the Paxeer PECOR V4 swap router used by the
///         Vault for the atomic deposit-swap to USDL. Matches the wallet-wired
///         router at 0x1D5f3ac9dE43Dd0665C3F527913dD825f67b3Daa on chain 125.
/// @dev `swapBestRoute` auto-routes across registered adapters and enforces
///      `amountOutMin` + `deadline` internally; the Vault double-checks the
///      realized USDL out via a balance delta as defense in depth.
interface IPECORRouter {
  /// @notice Swap `amountIn` of `tokenIn` for `tokenOut` along the best route.
  /// @dev The router pulls `tokenIn` from msg.sender (the Vault must approve it)
  ///      and sends `tokenOut` to msg.sender. Reverts if out < amountOutMin or
  ///      past `deadline`.
  function swapBestRoute(
    address tokenIn,
    address tokenOut,
    uint256 amountIn,
    uint256 amountOutMin,
    uint256 deadline
  ) external returns (uint256 amountOut);

  struct BestQuote {
    uint256 amountOut;
    uint256 priceImpactBps;
    uint256 feeBps;
    uint256 feeAmount;
    bytes32 adapterId;
    address adapter;
    bytes adapterData;
    bool found;
  }

  /// @notice View the best obtainable quote for `amountIn` (off-chain min_out sizing).
  function getBestQuote(address tokenIn, address tokenOut, uint256 amountIn)
    external
    view
    returns (BestQuote memory best);
}
