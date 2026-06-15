// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

import { Script } from "forge-std/Script.sol";
import { SettlementAnchor } from "../src/SettlementAnchor.sol";
import { LayerXVault } from "../src/LayerXVault.sol";

/// @notice Broadcast deploy of the LayerX contract suite to Paxeer (chain 125).
///         Deploys the SettlementAnchor, then the LayerXVault, then wires the
///         anchor's writer to the vault. REQUIRES explicit operator approval and
///         mainnet funds (Andrew drives all on-chain/mainnet actions).
///
/// Env:
///   LAYERX_GOVERNOR        protocol root authority (two-step)
///   LAYERX_OPERATOR_ADDR   sequencer EVM key (settlement + balance-proof signer)
///   LAYERX_GUARDIAN        emergency pause authority
///   LAYERX_USDL_ADDR       USDL reserve token (0x7c69c84d... on chain 125)
///   LAYERX_DEX_ROUTER      PECOR V4 router (0x1D5f3ac9... on chain 125)
///   LAYERX_WRAPPED_NATIVE  WPAX9 (0xe5ccf339... on chain 125)
///   LAYERX_EXIT_DELAY      force-exit challenge window in seconds (e.g. 86400)
contract Deploy is Script {
  function run() external returns (address anchor, address vault) {
    address governor = vm.envAddress("LAYERX_GOVERNOR");
    address operator = vm.envAddress("LAYERX_OPERATOR_ADDR");
    address guardian = vm.envAddress("LAYERX_GUARDIAN");
    address usdl = vm.envAddress("LAYERX_USDL_ADDR");
    address dexRouter = vm.envAddress("LAYERX_DEX_ROUTER");
    address wrappedNative = vm.envAddress("LAYERX_WRAPPED_NATIVE");
    uint64 exitDelay = uint64(vm.envUint("LAYERX_EXIT_DELAY"));

    vm.startBroadcast();

    SettlementAnchor anchorC = new SettlementAnchor(governor, address(0));
    LayerXVault vaultC = new LayerXVault(
      usdl, governor, operator, guardian, address(anchorC), dexRouter, wrappedNative, exitDelay
    );
    // The deployer is the governor here only if the broadcasting key == governor;
    // otherwise the governor must call setWriter post-deploy. We attempt it and
    // leave it to the operator to confirm wiring.
    if (anchorC.governor() == msg.sender) {
      anchorC.setWriter(address(vaultC));
    }

    vm.stopBroadcast();

    anchor = address(anchorC);
    vault = address(vaultC);
  }
}
