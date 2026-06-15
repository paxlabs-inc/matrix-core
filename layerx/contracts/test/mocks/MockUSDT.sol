// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

/// @notice USDT-style token that returns NO boolean from transfer/transferFrom/
///         approve and requires allowance reset-to-zero before a new non-zero
///         approval. Exercises the SafeERC20 + forceApprove paths. Declared
///         without the IERC20 interface precisely because its signatures differ.
contract MockUSDT {
  string public name = "Tether USD";
  string public symbol = "USDT";
  uint8 public constant decimals = 6;

  uint256 public totalSupply;
  mapping(address => uint256) public balanceOf;
  mapping(address => mapping(address => uint256)) public allowance;

  event Transfer(address indexed from, address indexed to, uint256 value);
  event Approval(address indexed owner, address indexed spender, uint256 value);

  function mint(address to, uint256 value) external {
    totalSupply += value;
    balanceOf[to] += value;
    emit Transfer(address(0), to, value);
  }

  /// @dev No return value (non-standard, like real USDT).
  function transfer(address to, uint256 value) external {
    balanceOf[msg.sender] -= value;
    balanceOf[to] += value;
    emit Transfer(msg.sender, to, value);
  }

  /// @dev Reverts if setting a non-zero allowance over an existing non-zero one
  ///      (the classic USDT approve-race protection). No return value.
  function approve(address spender, uint256 value) external {
    require(value == 0 || allowance[msg.sender][spender] == 0, "USDT: approve from non-zero");
    allowance[msg.sender][spender] = value;
    emit Approval(msg.sender, spender, value);
  }

  /// @dev No return value (non-standard).
  function transferFrom(address from, address to, uint256 value) external {
    uint256 allowed = allowance[from][msg.sender];
    if (allowed != type(uint256).max) {
      allowance[from][msg.sender] = allowed - value;
    }
    balanceOf[from] -= value;
    balanceOf[to] += value;
    emit Transfer(from, to, value);
  }
}
