// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

import { IERC20 } from "../interfaces/IERC20.sol";

/// @title SafeERC20
/// @notice In-house safe wrappers for ERC-20 calls that tolerate non-standard
///         tokens (e.g. USDT-style transfers that return no boolean) and the
///         approve-race by resetting allowance to zero first when needed.
/// @dev Holding hundreds of millions across heterogeneous stablecoins (USDL,
///      USDC, USDT) makes this non-negotiable: a bare `token.transfer` would
///      silently no-op on a token that returns no value, or revert on a token
///      that does. Every value movement in LayerX routes through here. No
///      external dependency — this is the OpenZeppelin SafeERC20 invariant set
///      reimplemented in-house and audited as part of this repo.
library SafeERC20 {
  /// @notice An ERC-20 call failed (reverted, returned false, or hit a non-contract).
  error SafeERC20FailedOperation(address token);

  /// @notice Transfer `value` tokens to `to`, reverting on any failure shape.
  function safeTransfer(IERC20 token, address to, uint256 value) internal {
    _callOptionalReturn(token, abi.encodeWithSelector(token.transfer.selector, to, value));
  }

  /// @notice Transfer `value` tokens from `from` to `to`, reverting on any failure shape.
  function safeTransferFrom(IERC20 token, address from, address to, uint256 value) internal {
    _callOptionalReturn(token, abi.encodeWithSelector(token.transferFrom.selector, from, to, value));
  }

  /// @notice Set allowance to exactly `value`, tolerating tokens that require a
  ///         reset-to-zero before a non-zero approval (USDT-style).
  function forceApprove(IERC20 token, address spender, uint256 value) internal {
    bytes memory approvalCall = abi.encodeWithSelector(token.approve.selector, spender, value);
    if (!_callOptionalReturnBool(token, approvalCall)) {
      _callOptionalReturn(
        token, abi.encodeWithSelector(token.approve.selector, spender, uint256(0))
      );
      _callOptionalReturn(token, approvalCall);
    }
  }

  /// @dev Performs a low-level call and validates the (optional) boolean return.
  ///      Reverts if the call failed, returned an explicit `false`, or the
  ///      target has no code (a non-contract masquerading as a token).
  function _callOptionalReturn(IERC20 token, bytes memory data) private {
    (bool success, bytes memory returndata) = address(token).call(data);
    if (
      !success || (returndata.length != 0 && !abi.decode(returndata, (bool)))
        || address(token).code.length == 0
    ) {
      revert SafeERC20FailedOperation(address(token));
    }
  }

  /// @dev Same as `_callOptionalReturn` but returns a success flag instead of
  ///      reverting; used by `forceApprove` to detect the approve-race path.
  function _callOptionalReturnBool(IERC20 token, bytes memory data) private returns (bool) {
    (bool success, bytes memory returndata) = address(token).call(data);
    return success && (returndata.length == 0 || abi.decode(returndata, (bool)))
      && address(token).code.length > 0;
  }
}
