// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

import { Test } from "forge-std/Test.sol";
import { LayerXVault } from "../src/LayerXVault.sol";
import { ILayerXVault } from "../src/interfaces/ILayerXVault.sol";
import { SettlementAnchor } from "../src/SettlementAnchor.sol";
import { Governed } from "../src/lib/Governed.sol";
import { Pausable } from "../src/lib/Pausable.sol";
import { IERC20 } from "../src/interfaces/IERC20.sol";
import { MockERC20 } from "./mocks/MockERC20.sol";
import { MockWrappedNative } from "./mocks/MockWrappedNative.sol";
import { MockPECORRouter } from "./mocks/MockPECORRouter.sol";
import { ReentrantToken } from "./mocks/ReentrantToken.sol";

contract LayerXVaultTest is Test {
  LayerXVault vault;
  SettlementAnchor anchor;
  MockERC20 usdl;
  MockERC20 usdc;
  MockWrappedNative wpax;
  MockPECORRouter router;

  uint256 operatorPk = 0xA11CE5EED;
  address operator;
  address governor = makeAddr("governor");
  address guardian = makeAddr("guardian");

  address alice = makeAddr("alice");
  address bob = makeAddr("bob");
  address carol = makeAddr("carol");

  uint64 constant EXIT_DELAY = 1 days;
  bytes32 constant DID = bytes32("did:matrix:alice");

  function setUp() public {
    operator = vm.addr(operatorPk);

    usdl = new MockERC20("Liquidity USD", "USDL", 6);
    usdc = new MockERC20("USD Coin", "USDC", 6);
    wpax = new MockWrappedNative();
    router = new MockPECORRouter(address(usdl));

    anchor = new SettlementAnchor(governor, address(0));
    vault = new LayerXVault(
      address(usdl),
      governor,
      operator,
      guardian,
      address(anchor),
      address(router),
      address(wpax),
      EXIT_DELAY
    );

    vm.startPrank(governor);
    anchor.setWriter(address(vault));
    vault.setSwapAllowed(address(usdc), true);
    vault.setSwapAllowed(address(wpax), true);
    vm.stopPrank();

    // Seed router liquidity + swap rates (USDL base out per 1 base unit in, x1e18).
    usdl.mint(address(router), 10_000_000e6);
    router.setRate(address(usdc), 1e18); // 1 USDC (1e6) -> 1 USDL (1e6)
    router.setRate(address(wpax), 7_170_000); // 1 PAX (1e18) -> 7.17 USDL (7.17e6)
  }

  // ---- helpers -------------------------------------------------------------

  function _signProof(ILayerXVault.BalanceProof memory proof) internal view returns (bytes memory) {
    bytes32 digest = vault.hashBalanceProof(proof);
    (uint8 v, bytes32 r, bytes32 s) = vm.sign(operatorPk, digest);
    return abi.encodePacked(r, s, v);
  }

  function _fundReserveViaDeposit(uint256 amount) internal {
    usdl.mint(alice, amount);
    vm.startPrank(alice);
    usdl.approve(address(vault), amount);
    vault.depositUSDL(amount, DID);
    vm.stopPrank();
  }

  // ---- deposits ------------------------------------------------------------

  function testDepositUSDL() public {
    usdl.mint(alice, 1000e6);
    vm.startPrank(alice);
    usdl.approve(address(vault), 1000e6);
    uint256 minted = vault.depositUSDL(1000e6, DID);
    vm.stopPrank();

    assertEq(minted, 1000e6);
    assertEq(vault.reserveBalance(), 1000e6);
    assertEq(vault.totalDeposited(), 1000e6);
  }

  function testDepositUSDLZeroReverts() public {
    vm.prank(alice);
    vm.expectRevert(LayerXVault.ZeroAmount.selector);
    vault.depositUSDL(0, DID);
  }

  function testDepositSwapUSDC() public {
    usdc.mint(alice, 500e6);
    vm.startPrank(alice);
    usdc.approve(address(vault), 500e6);
    uint256 minted = vault.depositSwap(address(usdc), 500e6, 499e6, block.timestamp, DID);
    vm.stopPrank();

    assertEq(minted, 500e6);
    assertEq(vault.reserveBalance(), 500e6);
    assertEq(usdc.balanceOf(address(router)), 500e6);
  }

  function testDepositSwapNativePAX() public {
    vm.deal(alice, 2 ether);
    vm.prank(alice);
    uint256 minted = vault.depositNative{ value: 1 ether }(7e6, block.timestamp, DID);
    assertEq(minted, 7_170_000); // 7.17 USDL
    assertEq(vault.reserveBalance(), 7_170_000);
  }

  function testDepositSwapSlippageReverts() public {
    usdc.mint(alice, 500e6);
    vm.startPrank(alice);
    usdc.approve(address(vault), 500e6);
    // demand more USDL out than the 1:1 rate can provide
    vm.expectRevert(bytes("mock: insufficient output"));
    vault.depositSwap(address(usdc), 500e6, 600e6, block.timestamp, DID);
    vm.stopPrank();
  }

  function testDepositSwapTokenNotAllowed() public {
    MockERC20 rando = new MockERC20("Rando", "RND", 6);
    rando.mint(alice, 100e6);
    vm.startPrank(alice);
    rando.approve(address(vault), 100e6);
    vm.expectRevert(LayerXVault.TokenNotAllowed.selector);
    vault.depositSwap(address(rando), 100e6, 1, block.timestamp, DID);
    vm.stopPrank();
  }

  function testDepositSwapDeadlineExpired() public {
    usdc.mint(alice, 100e6);
    vm.warp(1000);
    vm.startPrank(alice);
    usdc.approve(address(vault), 100e6);
    vm.expectRevert(LayerXVault.DeadlineExpired.selector);
    vault.depositSwap(address(usdc), 100e6, 1, 999, DID);
    vm.stopPrank();
  }

  // ---- settlement ----------------------------------------------------------

  function _batch(bytes32 id, address r1, uint256 a1, address r2, uint256 a2)
    internal
    view
    returns (ILayerXVault.Batch memory b)
  {
    ILayerXVault.Payout[] memory ps = new ILayerXVault.Payout[](2);
    ps[0] = ILayerXVault.Payout({ recipient: r1, amount: a1 });
    ps[1] = ILayerXVault.Payout({ recipient: r2, amount: a2 });
    b = ILayerXVault.Batch({
      batchId: id, root: bytes32(uint256(0xABC)), windowEnd: uint64(block.timestamp), payouts: ps
    });
  }

  function testSettle() public {
    _fundReserveViaDeposit(1000e6);
    ILayerXVault.Batch memory b = _batch(bytes32("w1"), bob, 100e6, carol, 50e6);

    vm.prank(operator);
    vault.settle(b);

    assertEq(usdl.balanceOf(bob), 100e6);
    assertEq(usdl.balanceOf(carol), 50e6);
    assertEq(vault.totalSettledOut(), 150e6);
    assertTrue(vault.settledBatch(bytes32("w1")));
    assertEq(anchor.rootOf(bytes32("w1")), bytes32(uint256(0xABC)));
  }

  function testSettleIdempotent() public {
    _fundReserveViaDeposit(1000e6);
    ILayerXVault.Batch memory b = _batch(bytes32("w1"), bob, 100e6, carol, 50e6);
    vm.startPrank(operator);
    vault.settle(b);
    vm.expectRevert(LayerXVault.BatchAlreadySettled.selector);
    vault.settle(b);
    vm.stopPrank();
  }

  function testSettleNotOperator() public {
    _fundReserveViaDeposit(1000e6);
    ILayerXVault.Batch memory b = _batch(bytes32("w1"), bob, 100e6, carol, 50e6);
    vm.prank(alice);
    vm.expectRevert(LayerXVault.NotOperator.selector);
    vault.settle(b);
  }

  function testSettleCapExceeded() public {
    _fundReserveViaDeposit(1000e6);
    vm.prank(governor);
    vault.setMaxSettlementPerBatch(100e6);
    ILayerXVault.Batch memory b = _batch(bytes32("w1"), bob, 100e6, carol, 50e6);
    vm.prank(operator);
    vm.expectRevert(LayerXVault.SettlementCapExceeded.selector);
    vault.settle(b);
  }

  function testSettleInsufficientReserve() public {
    _fundReserveViaDeposit(100e6);
    ILayerXVault.Batch memory b = _batch(bytes32("w1"), bob, 100e6, carol, 50e6);
    vm.prank(operator);
    vm.expectRevert(LayerXVault.InsufficientReserve.selector);
    vault.settle(b);
  }

  function testSettleZeroRecipientReverts() public {
    _fundReserveViaDeposit(1000e6);
    ILayerXVault.Batch memory b = _batch(bytes32("w1"), address(0), 100e6, carol, 50e6);
    vm.prank(operator);
    vm.expectRevert(Governed.ZeroAddress.selector);
    vault.settle(b);
  }

  // ---- force-exit ----------------------------------------------------------

  function _proof(address account, uint256 balance, uint64 epoch, uint64 expiry)
    internal
    pure
    returns (ILayerXVault.BalanceProof memory)
  {
    return ILayerXVault.BalanceProof({
      account: account, balance: balance, epoch: epoch, expiry: expiry
    });
  }

  function testForceExitLifecycle() public {
    _fundReserveViaDeposit(1000e6);
    ILayerXVault.BalanceProof memory p = _proof(alice, 300e6, 1, 0);
    bytes memory sig = _signProof(p);

    vm.prank(alice);
    vault.initiateExit(p, sig);

    vm.warp(block.timestamp + EXIT_DELAY);
    vault.finalizeExit(alice);

    assertEq(usdl.balanceOf(alice), 300e6);
    assertTrue(vault.exited(alice));
    assertEq(vault.claimedEpoch(alice), 1);
    assertEq(vault.totalExited(), 300e6);
  }

  function testExitNotMatured() public {
    _fundReserveViaDeposit(1000e6);
    ILayerXVault.BalanceProof memory p = _proof(alice, 300e6, 1, 0);
    bytes memory sig = _signProof(p);
    vm.prank(alice);
    vault.initiateExit(p, sig);
    vm.expectRevert(LayerXVault.ExitNotMatured.selector);
    vault.finalizeExit(alice);
  }

  function testExitWrongSigner() public {
    ILayerXVault.BalanceProof memory p = _proof(alice, 300e6, 1, 0);
    bytes32 digest = vault.hashBalanceProof(p);
    (uint8 v, bytes32 r, bytes32 s) = vm.sign(0xBADBADBAD, digest);
    bytes memory sig = abi.encodePacked(r, s, v);
    vm.prank(alice);
    vm.expectRevert(LayerXVault.InvalidProofSigner.selector);
    vault.initiateExit(p, sig);
  }

  function testExitNotProofAccount() public {
    ILayerXVault.BalanceProof memory p = _proof(alice, 300e6, 1, 0);
    bytes memory sig = _signProof(p);
    vm.prank(bob);
    vm.expectRevert(LayerXVault.NotProofAccount.selector);
    vault.initiateExit(p, sig);
  }

  function testExitExpiredProof() public {
    vm.warp(1000);
    ILayerXVault.BalanceProof memory p = _proof(alice, 300e6, 1, 999);
    bytes memory sig = _signProof(p);
    vm.prank(alice);
    vm.expectRevert(LayerXVault.ProofExpired.selector);
    vault.initiateExit(p, sig);
  }

  function testExitStaleEpochAfterFinalize() public {
    _fundReserveViaDeposit(1000e6);
    ILayerXVault.BalanceProof memory p = _proof(alice, 300e6, 1, 0);
    bytes memory sig = _signProof(p);
    vm.prank(alice);
    vault.initiateExit(p, sig);
    vm.warp(block.timestamp + EXIT_DELAY);
    vault.finalizeExit(alice);

    // Re-initiate now blocked because the account is terminally exited.
    ILayerXVault.BalanceProof memory p2 = _proof(alice, 100e6, 2, 0);
    bytes memory sig2 = _signProof(p2);
    vm.prank(alice);
    vm.expectRevert(LayerXVault.AccountExited.selector);
    vault.initiateExit(p2, sig2);
  }

  function testChallengeExitReducesAmount() public {
    _fundReserveViaDeposit(1000e6);
    ILayerXVault.BalanceProof memory p = _proof(alice, 300e6, 1, 0);
    bytes memory sig = _signProof(p);
    vm.prank(alice);
    vault.initiateExit(p, sig);

    // Operator proves a newer, lower balance (alice spent down).
    ILayerXVault.BalanceProof memory p2 = _proof(alice, 120e6, 2, 0);
    vault.challengeExit(p2, _signProof(p2));

    vm.warp(block.timestamp + EXIT_DELAY);
    vault.finalizeExit(alice);
    assertEq(usdl.balanceOf(alice), 120e6);
    assertEq(vault.claimedEpoch(alice), 2);
  }

  function testChallengeStaleEpochReverts() public {
    _fundReserveViaDeposit(1000e6);
    ILayerXVault.BalanceProof memory p = _proof(alice, 300e6, 5, 0);
    bytes memory sig = _signProof(p);
    vm.prank(alice);
    vault.initiateExit(p, sig);

    ILayerXVault.BalanceProof memory pOld = _proof(alice, 100e6, 5, 0);
    bytes memory sigOld = _signProof(pOld);
    vm.expectRevert(LayerXVault.StaleEpoch.selector);
    vault.challengeExit(pOld, sigOld);
  }

  function testSettleBarsExitedRecipient() public {
    _fundReserveViaDeposit(1000e6);
    ILayerXVault.BalanceProof memory p = _proof(bob, 100e6, 1, 0);
    bytes memory sig = _signProof(p);
    vm.prank(bob);
    vault.initiateExit(p, sig);
    vm.warp(block.timestamp + EXIT_DELAY);
    vault.finalizeExit(bob);

    ILayerXVault.Batch memory b = _batch(bytes32("w1"), bob, 10e6, carol, 5e6);
    vm.prank(operator);
    vm.expectRevert(LayerXVault.AccountExited.selector);
    vault.settle(b);
  }

  // ---- pause / governance / reentrancy ------------------------------------

  function testGuardianPauseBlocksDeposit() public {
    vm.prank(guardian);
    vault.pause();
    usdl.mint(alice, 100e6);
    vm.startPrank(alice);
    usdl.approve(address(vault), 100e6);
    vm.expectRevert(Pausable.EnforcedPause.selector);
    vault.depositUSDL(100e6, DID);
    vm.stopPrank();
  }

  function testOnlyGovernorUnpause() public {
    vm.prank(guardian);
    vault.pause();
    vm.prank(guardian);
    vm.expectRevert(Governed.NotGovernor.selector);
    vault.unpause();
    vm.prank(governor);
    vault.unpause();
    assertFalse(vault.paused());
  }

  function testNonGovernorSetterReverts() public {
    vm.prank(alice);
    vm.expectRevert(Governed.NotGovernor.selector);
    vault.setOperator(alice);
  }

  function testGovernanceTwoStep() public {
    address newGov = makeAddr("newGov");
    vm.prank(governor);
    vault.transferGovernance(newGov);
    assertEq(vault.governor(), governor);
    vm.prank(newGov);
    vault.acceptGovernance();
    assertEq(vault.governor(), newGov);
  }

  function testSetExitDelayBounds() public {
    vm.prank(governor);
    vm.expectRevert(LayerXVault.ExitDelayOutOfRange.selector);
    vault.setExitDelay(1 minutes);
  }

  function testReentrancyGuardBlocksReentrantDeposit() public {
    ReentrantToken evil = new ReentrantToken();
    vm.prank(governor);
    vault.setSwapAllowed(address(evil), true);
    router.setRate(address(evil), 1e18); // 1:1 so the outer swap completes

    evil.mint(alice, 100e6);
    // Arm the token to attempt re-entry into depositUSDL during its transferFrom.
    evil.arm(address(vault), abi.encodeWithSelector(vault.depositUSDL.selector, uint256(1), DID));

    vm.startPrank(alice);
    evil.approve(address(vault), 100e6);
    vault.depositSwap(address(evil), 100e6, 1, block.timestamp, DID);
    vm.stopPrank();

    // The re-entry was attempted and the guard rejected it; the outer deposit
    // still completed atomically.
    assertTrue(evil.reentryAttempted());
    assertFalse(evil.reentrySucceeded());
    assertEq(vault.reserveBalance(), 100e6);
  }

  function testConstructorRejectsBadExitDelay() public {
    vm.expectRevert(LayerXVault.ExitDelayOutOfRange.selector);
    new LayerXVault(
      address(usdl),
      governor,
      operator,
      guardian,
      address(anchor),
      address(router),
      address(wpax),
      1 minutes
    );
  }
}
