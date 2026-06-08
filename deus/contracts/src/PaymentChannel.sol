// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

/// @title PaymentChannel
/// @notice Per-caller escrow for net-settlement windows (Phase 2.5).
contract PaymentChannel {
  address public settler;
  address public caller;

  uint256 public fundedWei;
  uint256 public redeemedWei;

  error NotSettler();
  error NotCaller();
  error InsufficientEscrow();
  error ZeroAddress();

  event Funded(address indexed caller, uint256 amount, uint256 total);
  event Payout(address indexed payee, uint256 amount);
  event Closed(address indexed caller, uint256 refund);

  modifier onlySettler() {
    if (msg.sender != settler) {
      revert NotSettler();
    }
    _;
  }

  constructor(address caller_, address settler_) {
    if (caller_ == address(0) || settler_ == address(0)) {
      revert ZeroAddress();
    }
    caller = caller_;
    settler = settler_;
  }

  /// @notice Caller funds the channel for the current window.
  function fund() external payable {
    if (msg.sender != caller) {
      revert NotCaller();
    }
    fundedWei += msg.value;
    emit Funded(caller, msg.value, fundedWei);
  }

  /// @notice Settler pays a developer from escrow (bounded by funded - redeemed).
  function payout(address payee, uint256 amountWei) external onlySettler {
    if (payee == address(0)) {
      revert ZeroAddress();
    }
    if (redeemedWei + amountWei > fundedWei) {
      revert InsufficientEscrow();
    }
    redeemedWei += amountWei;
    (bool ok,) = payee.call{value: amountWei}("");
    require(ok, "transfer failed");
    emit Payout(payee, amountWei);
  }

  /// @notice Close channel and refund unspent balance to caller.
  function close() external {
    if (msg.sender != caller && msg.sender != settler) {
      revert NotCaller();
    }
    uint256 remaining = fundedWei - redeemedWei;
    fundedWei = 0;
    redeemedWei = 0;
    if (remaining > 0) {
      (bool ok,) = caller.call{value: remaining}("");
      require(ok, "refund failed");
    }
    emit Closed(caller, remaining);
  }

  function availableWei() external view returns (uint256) {
    return fundedWei - redeemedWei;
  }
}
