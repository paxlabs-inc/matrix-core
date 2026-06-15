// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

import { ILayerXVault } from "./interfaces/ILayerXVault.sol";
import { ISettlementAnchor } from "./interfaces/ISettlementAnchor.sol";
import { IERC20 } from "./interfaces/IERC20.sol";
import { IPECORRouter } from "./interfaces/IPECORRouter.sol";
import { IWrappedNative } from "./interfaces/IWrappedNative.sol";
import { SafeERC20 } from "./lib/SafeERC20.sol";
import { ReentrancyGuard } from "./lib/ReentrancyGuard.sol";
import { Pausable } from "./lib/Pausable.sol";
import { Governed } from "./lib/Governed.sol";
import { EIP712 } from "./lib/EIP712.sol";
import { ECDSA } from "./lib/ECDSA.sol";

/// @title LayerXVault
/// @notice The on-chain custody contract for LayerX. It holds the fleet's USDL
///         reserve, mints USDX accounting against DID-claimed deposits (USDL
///         direct, or PAX/USDC/USDT atomically swapped to USDL on the Paxeer
///         DEX), pays per-account net withdrawal deltas on operator-submitted
///         settlement batches, and exposes a unilateral force-exit escape hatch
///         bounded by the agent's last operator-co-signed balance.
///
/// @dev Trust model (frozen spec [invariants], DESIGN.md "trust spine"):
///      - The vault holds USDL; circulating USDX == USDL held here.
///      - The OPERATOR (sequencer EVM key) can only trigger payouts; it cannot
///        forge balances and is bounded by the reserve, a per-batch cap, and a
///        per-account exited bar.
///      - The GOVERNOR is the protocol root (params, role rotation) and the
///        GUARDIAN can pause in emergencies; neither can move agent funds
///        arbitrarily.
///      - The force-exit hatch defends against a dark/withholding operator: any
///        agent can exit with its last co-signed balance after `exitDelay`, so
///        a single sequencer can never trap funds.
///      All token movement routes through SafeERC20; all external mutators are
///      nonReentrant. No external (e.g. OpenZeppelin) imports — every safeguard
///      is in-house under src/lib.
contract LayerXVault is ILayerXVault, Governed, Pausable, ReentrancyGuard, EIP712 {
  using SafeERC20 for IERC20;

  // --------------------------------------------------------------------------
  // Constants
  // --------------------------------------------------------------------------

  /// @dev Upper bound on payouts per settlement batch to keep gas bounded.
  uint256 public constant MAX_PAYOUTS = 256;
  /// @dev Floor / ceiling on the configurable exit challenge window.
  uint64 public constant MIN_EXIT_DELAY = 1 hours;
  uint64 public constant MAX_EXIT_DELAY = 30 days;

  /// @dev EIP-712 type hash for the operator's co-signed balance proof.
  bytes32 private constant _BALANCE_PROOF_TYPEHASH =
    keccak256("BalanceProof(address account,uint256 balance,uint64 epoch,uint64 expiry)");

  // --------------------------------------------------------------------------
  // Immutable / config state
  // --------------------------------------------------------------------------

  /// @notice The canonical reserve asset. circulating USDX == this.balanceOf(vault).
  IERC20 public immutable usdl;

  /// @notice The sequencer EVM key that submits settlement batches and co-signs
  ///         balance proofs. Bounded by reserve + caps; cannot drain the vault.
  address public operator;
  /// @notice Emergency pause authority.
  address public guardian;
  /// @notice Paxeer DEX router used for the atomic deposit swap-to-USDL.
  IPECORRouter public dexRouter;
  /// @notice Immutable settled-root log.
  ISettlementAnchor public anchor;
  /// @notice Wrapped native (WPAX9) used to swap native PAX deposits.
  IWrappedNative public wrappedNative;

  /// @notice Challenge window before a force-exit can be finalized.
  uint64 public exitDelay;
  /// @notice Max USDL paid out in a single settlement batch (0 = uncapped).
  uint256 public maxSettlementPerBatch;

  // --------------------------------------------------------------------------
  // Accounting / book-keeping
  // --------------------------------------------------------------------------

  /// @notice Cumulative USDL credited via deposits (audit denormalization).
  uint256 public totalDeposited;
  /// @notice Cumulative USDL paid out via settlement.
  uint256 public totalSettledOut;
  /// @notice Cumulative USDL paid out via force-exit.
  uint256 public totalExited;

  /// @notice Tokens permitted as swap-deposit inputs (governor-allowlisted).
  mapping(address => bool) public swapAllowed;
  /// @notice Settlement idempotency: a batchId can be applied once.
  mapping(bytes32 => bool) public settledBatch;

  /// @notice A pending unilateral exit for an account.
  struct Exit {
    uint256 amount;
    uint64 epoch;
    uint64 claimableAt;
  }

  /// @notice Active pending exit per account (claimableAt == 0 means none).
  mapping(address => Exit) public pendingExit;
  /// @notice Highest balance-proof epoch already consumed by a finalized exit.
  mapping(address => uint64) public claimedEpoch;
  /// @notice Accounts that have force-exited are barred from further settlement.
  mapping(address => bool) public exited;

  // --------------------------------------------------------------------------
  // Errors
  // --------------------------------------------------------------------------

  error NotOperator();
  error NotGuardianOrGovernor();
  error ZeroAmount();
  error TokenNotAllowed();
  error SameTokenSwap();
  error SlippageExceeded();
  error DeadlineExpired();
  error RouterUnset();
  error WrappedNativeUnset();
  error BatchAlreadySettled();
  error TooManyPayouts();
  error SettlementCapExceeded();
  error InsufficientReserve();
  error AccountExited();
  error InvalidProofSigner();
  error NotProofAccount();
  error ProofExpired();
  error StaleEpoch();
  error NoPendingExit();
  error ExitNotMatured();
  error ExitDelayOutOfRange();
  error NativeValueMismatch();

  // --------------------------------------------------------------------------
  // Modifiers
  // --------------------------------------------------------------------------

  modifier onlyOperator() {
    if (msg.sender != operator) {
      revert NotOperator();
    }
    _;
  }

  modifier onlyGuardianOrGovernor() {
    if (msg.sender != guardian && msg.sender != governor) {
      revert NotGuardianOrGovernor();
    }
    _;
  }

  // --------------------------------------------------------------------------
  // Construction
  // --------------------------------------------------------------------------

  /// @param usdl_ canonical reserve asset (USDL)
  /// @param governor_ protocol root authority (two-step)
  /// @param operator_ sequencer EVM key (settlement + balance-proof signer)
  /// @param guardian_ emergency pause authority
  /// @param anchor_ immutable settled-root log
  /// @param dexRouter_ Paxeer DEX router for deposit swaps (may be zero, set later)
  /// @param wrappedNative_ WPAX9 for native-PAX deposits (may be zero, set later)
  /// @param exitDelay_ force-exit challenge window (clamped to [MIN,MAX])
  constructor(
    address usdl_,
    address governor_,
    address operator_,
    address guardian_,
    address anchor_,
    address dexRouter_,
    address wrappedNative_,
    uint64 exitDelay_
  ) Governed(governor_) EIP712("LayerX", "1") {
    if (
      usdl_ == address(0) || operator_ == address(0) || guardian_ == address(0)
        || anchor_ == address(0)
    ) {
      revert ZeroAddress();
    }
    if (exitDelay_ < MIN_EXIT_DELAY || exitDelay_ > MAX_EXIT_DELAY) {
      revert ExitDelayOutOfRange();
    }
    usdl = IERC20(usdl_);
    operator = operator_;
    guardian = guardian_;
    anchor = ISettlementAnchor(anchor_);
    dexRouter = IPECORRouter(dexRouter_);
    wrappedNative = IWrappedNative(wrappedNative_);
    exitDelay = exitDelay_;

    emit OperatorUpdated(address(0), operator_);
    emit GuardianUpdated(address(0), guardian_);
    emit AnchorUpdated(address(0), anchor_);
    emit DexRouterUpdated(address(0), dexRouter_);
    emit WrappedNativeUpdated(address(0), wrappedNative_);
    emit ExitDelayUpdated(0, exitDelay_);
  }

  /// @dev Accept native PAX only from the wrapped-native contract (on unwrap).
  receive() external payable {
    if (msg.sender != address(wrappedNative)) {
      revert NativeValueMismatch();
    }
  }

  // --------------------------------------------------------------------------
  // Deposits
  // --------------------------------------------------------------------------

  /// @inheritdoc ILayerXVault
  /// @notice Deposit USDL directly and mint USDX 1:1. The off-chain sequencer
  ///         credits `did`'s balance on observing the Deposit event.
  function depositUSDL(uint256 amount, bytes32 did)
    external
    whenNotPaused
    nonReentrant
    returns (uint256 usdxMinted)
  {
    if (amount == 0) {
      revert ZeroAmount();
    }
    usdxMinted = _pullMeasured(usdl, msg.sender, amount);
    totalDeposited += usdxMinted;
    emit Deposit(did, msg.sender, address(usdl), amount, usdxMinted);
  }

  /// @inheritdoc ILayerXVault
  /// @notice Deposit an allowlisted ERC-20 (USDC/USDT/WPAX9), atomically swap it
  ///         to USDL on the DEX, and mint USDX equal to the USDL actually
  ///         received. The swap return IS the price oracle and the reserve.
  function depositSwap(
    address tokenIn,
    uint256 amountIn,
    uint256 minUsdlOut,
    uint256 deadline,
    bytes32 did
  ) external whenNotPaused nonReentrant returns (uint256 usdxMinted) {
    if (amountIn == 0 || minUsdlOut == 0) {
      revert ZeroAmount();
    }
    if (tokenIn == address(usdl)) {
      revert SameTokenSwap();
    }
    if (!swapAllowed[tokenIn]) {
      revert TokenNotAllowed();
    }
    uint256 received = _pullMeasured(IERC20(tokenIn), msg.sender, amountIn);
    usdxMinted = _swapToUSDL(tokenIn, received, minUsdlOut, deadline);
    totalDeposited += usdxMinted;
    emit Deposit(did, msg.sender, tokenIn, amountIn, usdxMinted);
  }

  /// @inheritdoc ILayerXVault
  /// @notice Deposit native PAX: wrap to WPAX9, swap to USDL, mint USDX equal to
  ///         the USDL received.
  function depositNative(uint256 minUsdlOut, uint256 deadline, bytes32 did)
    external
    payable
    whenNotPaused
    nonReentrant
    returns (uint256 usdxMinted)
  {
    if (msg.value == 0 || minUsdlOut == 0) {
      revert ZeroAmount();
    }
    address wnative = address(wrappedNative);
    if (wnative == address(0)) {
      revert WrappedNativeUnset();
    }
    if (!swapAllowed[wnative]) {
      revert TokenNotAllowed();
    }
    uint256 wBefore = wrappedNative.balanceOf(address(this));
    wrappedNative.deposit{ value: msg.value }();
    uint256 wrapped = wrappedNative.balanceOf(address(this)) - wBefore;
    usdxMinted = _swapToUSDL(wnative, wrapped, minUsdlOut, deadline);
    totalDeposited += usdxMinted;
    emit Deposit(did, msg.sender, address(0), msg.value, usdxMinted);
  }

  // --------------------------------------------------------------------------
  // Settlement (operator)
  // --------------------------------------------------------------------------

  /// @inheritdoc ILayerXVault
  /// @notice Apply one settlement window: pay per-account net withdrawal deltas
  ///         in USDL and anchor the batch root. The operator's tx IS its
  ///         authorization; payouts are bounded by reserve, the per-batch cap,
  ///         and the exited-account bar.
  function settle(Batch calldata batch) external onlyOperator whenNotPaused nonReentrant {
    if (settledBatch[batch.batchId]) {
      revert BatchAlreadySettled();
    }
    uint256 count = batch.payouts.length;
    if (count > MAX_PAYOUTS) {
      revert TooManyPayouts();
    }

    settledBatch[batch.batchId] = true;

    uint256 total;
    for (uint256 i = 0; i < count; ++i) {
      Payout calldata p = batch.payouts[i];
      if (p.recipient == address(0)) {
        revert ZeroAddress();
      }
      if (p.amount == 0) {
        revert ZeroAmount();
      }
      if (exited[p.recipient]) {
        revert AccountExited();
      }
      total += p.amount;
    }

    uint256 cap = maxSettlementPerBatch;
    if (cap != 0 && total > cap) {
      revert SettlementCapExceeded();
    }
    if (total > usdl.balanceOf(address(this))) {
      revert InsufficientReserve();
    }

    totalSettledOut += total;

    // Anchor first (state log), then disburse. Effects precede the external
    // token interactions; the whole call is nonReentrant regardless.
    anchor.record(batch.batchId, batch.root, total, count, batch.windowEnd);

    for (uint256 i = 0; i < count; ++i) {
      Payout calldata p = batch.payouts[i];
      usdl.safeTransfer(p.recipient, p.amount);
    }

    emit Settled(batch.batchId, batch.root, total, count, batch.windowEnd);
  }

  // --------------------------------------------------------------------------
  // Force-exit escape hatch
  // --------------------------------------------------------------------------

  /// @inheritdoc ILayerXVault
  /// @notice Begin a unilateral exit using the operator's last co-signed balance
  ///         proof. After `exitDelay` the caller can finalize and be paid even
  ///         if the operator is dark. msg.sender must be the proof account.
  function initiateExit(BalanceProof calldata proof, bytes calldata operatorSig)
    external
    whenNotPaused
    nonReentrant
  {
    _verifyProof(proof, operatorSig);
    if (msg.sender != proof.account) {
      revert NotProofAccount();
    }
    if (exited[proof.account]) {
      revert AccountExited();
    }
    if (proof.epoch <= claimedEpoch[proof.account]) {
      revert StaleEpoch();
    }
    if (
      proof.epoch <= pendingExit[proof.account].epoch && pendingExit[proof.account].claimableAt != 0
    ) {
      revert StaleEpoch();
    }

    uint64 claimableAt = uint64(block.timestamp) + exitDelay;
    pendingExit[proof.account] =
      Exit({ amount: proof.balance, epoch: proof.epoch, claimableAt: claimableAt });
    emit ExitInitiated(proof.account, proof.balance, proof.epoch, claimableAt);
  }

  /// @inheritdoc ILayerXVault
  /// @notice Correct a pending exit with a strictly-newer operator-co-signed
  ///         balance (the account spent down after initiating). Permissionless
  ///         to submit, but the proof must carry the operator's signature. The
  ///         maturity time is NOT extended, so this cannot be used to grief.
  function challengeExit(BalanceProof calldata proof, bytes calldata operatorSig)
    external
    nonReentrant
  {
    _verifyProof(proof, operatorSig);
    Exit storage e = pendingExit[proof.account];
    if (e.claimableAt == 0) {
      revert NoPendingExit();
    }
    if (proof.epoch <= e.epoch) {
      revert StaleEpoch();
    }
    e.amount = proof.balance;
    e.epoch = proof.epoch;
    emit ExitChallenged(proof.account, proof.balance, proof.epoch);
  }

  /// @inheritdoc ILayerXVault
  /// @notice Finalize a matured exit and pay the account its co-signed balance.
  ///         The account becomes terminally exited (barred from settlement) to
  ///         prevent double payment.
  function finalizeExit(address account) external whenNotPaused nonReentrant {
    Exit memory e = pendingExit[account];
    if (e.claimableAt == 0) {
      revert NoPendingExit();
    }
    if (block.timestamp < e.claimableAt) {
      revert ExitNotMatured();
    }
    if (e.amount > usdl.balanceOf(address(this))) {
      revert InsufficientReserve();
    }

    delete pendingExit[account];
    claimedEpoch[account] = e.epoch;
    exited[account] = true;
    totalExited += e.amount;

    usdl.safeTransfer(account, e.amount);
    emit ExitFinalized(account, e.amount, e.epoch);
  }

  // --------------------------------------------------------------------------
  // Governance / roles
  // --------------------------------------------------------------------------

  function setOperator(address newOperator) external onlyGovernor {
    if (newOperator == address(0)) {
      revert ZeroAddress();
    }
    emit OperatorUpdated(operator, newOperator);
    operator = newOperator;
  }

  function setGuardian(address newGuardian) external onlyGovernor {
    if (newGuardian == address(0)) {
      revert ZeroAddress();
    }
    emit GuardianUpdated(guardian, newGuardian);
    guardian = newGuardian;
  }

  function setDexRouter(address newRouter) external onlyGovernor {
    emit DexRouterUpdated(address(dexRouter), newRouter);
    dexRouter = IPECORRouter(newRouter);
  }

  function setAnchor(address newAnchor) external onlyGovernor {
    if (newAnchor == address(0)) {
      revert ZeroAddress();
    }
    emit AnchorUpdated(address(anchor), newAnchor);
    anchor = ISettlementAnchor(newAnchor);
  }

  function setWrappedNative(address newWrappedNative) external onlyGovernor {
    emit WrappedNativeUpdated(address(wrappedNative), newWrappedNative);
    wrappedNative = IWrappedNative(newWrappedNative);
  }

  function setExitDelay(uint64 newDelay) external onlyGovernor {
    if (newDelay < MIN_EXIT_DELAY || newDelay > MAX_EXIT_DELAY) {
      revert ExitDelayOutOfRange();
    }
    emit ExitDelayUpdated(exitDelay, newDelay);
    exitDelay = newDelay;
  }

  function setMaxSettlementPerBatch(uint256 newCap) external onlyGovernor {
    emit MaxSettlementPerBatchUpdated(maxSettlementPerBatch, newCap);
    maxSettlementPerBatch = newCap;
  }

  function setSwapAllowed(address token, bool allowed) external onlyGovernor {
    if (token == address(0)) {
      revert ZeroAddress();
    }
    swapAllowed[token] = allowed;
    emit SwapTokenAllowed(token, allowed);
  }

  function pause() external onlyGuardianOrGovernor {
    _pause();
  }

  function unpause() external onlyGovernor {
    _unpause();
  }

  // --------------------------------------------------------------------------
  // Views
  // --------------------------------------------------------------------------

  /// @notice The USDL reserve currently held (== circulating USDX target).
  function reserveBalance() external view returns (uint256) {
    return usdl.balanceOf(address(this));
  }

  /// @notice EIP-712 digest for a balance proof (off-chain signer convenience).
  function hashBalanceProof(BalanceProof calldata proof) external view returns (bytes32) {
    return _hashTypedDataV4(_structHash(proof));
  }

  // --------------------------------------------------------------------------
  // Internal helpers
  // --------------------------------------------------------------------------

  /// @dev Pull `amount` of `token` from `from`, returning the ACTUAL increase in
  ///      this contract's balance (defends against fee-on-transfer surprises).
  function _pullMeasured(IERC20 token, address from, uint256 amount) private returns (uint256) {
    uint256 before = token.balanceOf(address(this));
    token.safeTransferFrom(from, address(this), amount);
    return token.balanceOf(address(this)) - before;
  }

  /// @dev Swap `amountIn` of `tokenIn` (already held by the vault) to USDL via
  ///      the DEX, returning the realized USDL received. Resets router allowance
  ///      to zero afterward and double-checks slippage beyond the router.
  function _swapToUSDL(address tokenIn, uint256 amountIn, uint256 minUsdlOut, uint256 deadline)
    private
    returns (uint256 usdlReceived)
  {
    if (deadline != 0 && block.timestamp > deadline) {
      revert DeadlineExpired();
    }
    IPECORRouter router = dexRouter;
    if (address(router) == address(0)) {
      revert RouterUnset();
    }
    uint256 effectiveDeadline = deadline == 0 ? block.timestamp : deadline;

    uint256 usdlBefore = usdl.balanceOf(address(this));
    IERC20(tokenIn).forceApprove(address(router), amountIn);
    router.swapBestRoute(tokenIn, address(usdl), amountIn, minUsdlOut, effectiveDeadline);
    IERC20(tokenIn).forceApprove(address(router), 0);

    usdlReceived = usdl.balanceOf(address(this)) - usdlBefore;
    if (usdlReceived < minUsdlOut) {
      revert SlippageExceeded();
    }
  }

  /// @dev Verify an operator-co-signed balance proof (EIP-712 + ECDSA, with
  ///      malleability protection) and its (optional) expiry.
  function _verifyProof(BalanceProof calldata proof, bytes calldata sig) private view {
    if (proof.expiry != 0 && block.timestamp > proof.expiry) {
      revert ProofExpired();
    }
    bytes32 digest = _hashTypedDataV4(_structHash(proof));
    address signer = ECDSA.recover(digest, sig);
    if (signer != operator) {
      revert InvalidProofSigner();
    }
  }

  function _structHash(BalanceProof calldata proof) private pure returns (bytes32) {
    return keccak256(
      abi.encode(_BALANCE_PROOF_TYPEHASH, proof.account, proof.balance, proof.epoch, proof.expiry)
    );
  }
}
