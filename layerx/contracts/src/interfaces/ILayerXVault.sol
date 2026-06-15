// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

/// @title ILayerXVault
/// @notice External surface + wire types for the LayerX custody vault.
interface ILayerXVault {
  /// @notice A single net payout in a settlement batch (DID's mapped EVM addr + USDL amount).
  struct Payout {
    address recipient;
    uint256 amount;
  }

  /// @notice An operator-submitted settlement batch for one window.
  /// @dev `payouts` are the per-account NET withdrawal deltas; pure agent<->agent
  ///      transfers net to zero externally and never appear here.
  struct Batch {
    bytes32 batchId;
    bytes32 root;
    uint64 windowEnd;
    Payout[] payouts;
  }

  /// @notice The operator's EIP-712 co-signed statement of an account's
  ///         withdrawable balance, used by the force-exit escape hatch.
  struct BalanceProof {
    address account; // the agent's mapped Paxeer EVM address (== msg.sender on exit)
    uint256 balance; // USDL the account may withdraw at this epoch
    uint64 epoch; // strictly monotonic; only a newer epoch supersedes
    uint64 expiry; // unix seconds; 0 = no expiry
  }

  event Deposit(
    bytes32 indexed did,
    address indexed depositor,
    address indexed tokenIn,
    uint256 amountIn,
    uint256 usdxMinted
  );
  event Settled(
    bytes32 indexed batchId, bytes32 root, uint256 totalSettled, uint256 count, uint64 windowEnd
  );
  event ExitInitiated(address indexed account, uint256 amount, uint64 epoch, uint64 claimableAt);
  event ExitChallenged(address indexed account, uint256 newAmount, uint64 newEpoch);
  event ExitFinalized(address indexed account, uint256 amount, uint64 epoch);

  event OperatorUpdated(address indexed previous, address indexed current);
  event GuardianUpdated(address indexed previous, address indexed current);
  event DexRouterUpdated(address indexed previous, address indexed current);
  event AnchorUpdated(address indexed previous, address indexed current);
  event WrappedNativeUpdated(address indexed previous, address indexed current);
  event ExitDelayUpdated(uint64 previous, uint64 current);
  event MaxSettlementPerBatchUpdated(uint256 previous, uint256 current);
  event SwapTokenAllowed(address indexed token, bool allowed);

  function depositUSDL(uint256 amount, bytes32 did) external returns (uint256 usdxMinted);
  function depositSwap(
    address tokenIn,
    uint256 amountIn,
    uint256 minUsdlOut,
    uint256 deadline,
    bytes32 did
  ) external returns (uint256 usdxMinted);
  function depositNative(uint256 minUsdlOut, uint256 deadline, bytes32 did)
    external
    payable
    returns (uint256 usdxMinted);

  function settle(Batch calldata batch) external;

  function initiateExit(BalanceProof calldata proof, bytes calldata operatorSig) external;
  function challengeExit(BalanceProof calldata proof, bytes calldata operatorSig) external;
  function finalizeExit(address account) external;
}
