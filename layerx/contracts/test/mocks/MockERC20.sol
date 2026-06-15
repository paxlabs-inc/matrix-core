// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

import { IERC20 } from "../../src/interfaces/IERC20.sol";

/// @notice Standard, well-behaved ERC-20 for tests (returns bool).
contract MockERC20 is IERC20 {
  string public name;
  string public symbol;
  uint8 public immutable _decimals;

  uint256 public totalSupply;
  mapping(address => uint256) public balanceOf;
  mapping(address => mapping(address => uint256)) public allowance;

  constructor(string memory n, string memory s, uint8 d) {
    name = n;
    symbol = s;
    _decimals = d;
  }

  function decimals() external view returns (uint8) {
    return _decimals;
  }

  function mint(address to, uint256 value) external {
    totalSupply += value;
    balanceOf[to] += value;
    emit Transfer(address(0), to, value);
  }

  function transfer(address to, uint256 value) public virtual returns (bool) {
    balanceOf[msg.sender] -= value;
    balanceOf[to] += value;
    emit Transfer(msg.sender, to, value);
    return true;
  }

  function approve(address spender, uint256 value) public virtual returns (bool) {
    allowance[msg.sender][spender] = value;
    emit Approval(msg.sender, spender, value);
    return true;
  }

  function transferFrom(address from, address to, uint256 value) public virtual returns (bool) {
    uint256 allowed = allowance[from][msg.sender];
    if (allowed != type(uint256).max) {
      allowance[from][msg.sender] = allowed - value;
    }
    balanceOf[from] -= value;
    balanceOf[to] += value;
    emit Transfer(from, to, value);
    return true;
  }
}
