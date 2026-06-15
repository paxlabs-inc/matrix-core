// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

import { IPECORRouter } from "../../src/interfaces/IPECORRouter.sol";
import { IERC20 } from "../../src/interfaces/IERC20.sol";

/// @notice Test DEX router. Pulls `tokenIn` from the caller and pays out USDL at
///         a per-token configured rate (USDL base units per 1 base unit of
///         tokenIn, scaled by 1e18). Must be pre-funded with USDL liquidity.
contract MockPECORRouter is IPECORRouter {
  IERC20 public immutable usdl;
  mapping(address => uint256) public rateE18; // out(USDL base) = amountIn * rateE18 / 1e18

  constructor(address usdl_) {
    usdl = IERC20(usdl_);
  }

  function setRate(address tokenIn, uint256 rate) external {
    rateE18[tokenIn] = rate;
  }

  function _out(address tokenIn, uint256 amountIn) internal view returns (uint256) {
    return (amountIn * rateE18[tokenIn]) / 1e18;
  }

  function swapBestRoute(
    address tokenIn,
    address tokenOut,
    uint256 amountIn,
    uint256 amountOutMin,
    uint256 deadline
  ) external returns (uint256 amountOut) {
    require(tokenOut == address(usdl), "mock: tokenOut must be USDL");
    require(block.timestamp <= deadline, "mock: expired");
    // Pull input from the caller (the Vault, which approved us).
    require(IERC20(tokenIn).transferFrom(msg.sender, address(this), amountIn), "mock: pull failed");
    amountOut = _out(tokenIn, amountIn);
    require(amountOut >= amountOutMin, "mock: insufficient output");
    require(usdl.transfer(msg.sender, amountOut), "mock: payout failed");
  }

  function getBestQuote(address tokenIn, address tokenOut, uint256 amountIn)
    external
    view
    returns (BestQuote memory best)
  {
    require(tokenOut == address(usdl), "mock: tokenOut must be USDL");
    best.amountOut = _out(tokenIn, amountIn);
    best.found = best.amountOut > 0;
  }
}
