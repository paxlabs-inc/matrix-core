// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

/// @title IERC20
/// @notice Minimal in-house ERC-20 interface. No external dependency.
/// @dev `decimals` is included because LayerX accounting is denominated in the
///      reserve token's native units (USDL = 6 decimals == 1e6 micro-USDX).
interface IERC20 {
  event Transfer(address indexed from, address indexed to, uint256 value);
  event Approval(address indexed owner, address indexed spender, uint256 value);

  function totalSupply() external view returns (uint256);

  function balanceOf(address account) external view returns (uint256);

  function transfer(address to, uint256 value) external returns (bool);

  function allowance(address owner, address spender) external view returns (uint256);

  function approve(address spender, uint256 value) external returns (bool);

  function transferFrom(address from, address to, uint256 value) external returns (bool);

  function decimals() external view returns (uint8);
}
