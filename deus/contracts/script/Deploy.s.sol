// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

import {Script} from "forge-std/Script.sol";
import {ServiceRegistry} from "../src/ServiceRegistry.sol";

/// @notice Broadcast deploy to chain 125. Requires explicit operator approval.
contract Deploy is Script {
  function run() external returns (address registry) {
    address governor = vm.envAddress("DEUS_REGISTRY_GOVERNOR");
    vm.startBroadcast();
    registry = address(new ServiceRegistry(governor));
    vm.stopBroadcast();
  }
}
