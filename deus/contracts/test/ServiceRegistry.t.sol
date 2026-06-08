// SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
pragma solidity 0.8.27;

import {Test} from "forge-std/Test.sol";
import {ServiceRegistry} from "../src/ServiceRegistry.sol";
import {IServiceRegistry} from "../src/interfaces/IServiceRegistry.sol";

contract ServiceRegistryTest is Test {
  ServiceRegistry internal registry;
  address internal gov;
  address internal dev;
  address internal payout;

  bytes32 internal manifestHash = keccak256("manifest");
  bytes32 internal pricingHash = keccak256("pricing");

  function setUp() public {
    gov = makeAddr("governor");
    dev = makeAddr("developer");
    payout = makeAddr("payout");
    registry = new ServiceRegistry(gov);
  }

  function testRegisterEmitsEventAndStores() public {
    vm.prank(dev);
    vm.expectEmit(true, true, false, true);
    emit IServiceRegistry.ServiceRegistered(1, dev, manifestHash, pricingHash, false, false);
    uint256 id = registry.register(payout, manifestHash, pricingHash, false, false);
    assertEq(id, 1);
    IServiceRegistry.Service memory svc = registry.getService(1);
    assertEq(svc.owner, dev);
    assertEq(svc.payout, payout);
    assertEq(svc.manifestHash, manifestHash);
    assertEq(svc.status, registry.STATUS_ACTIVE());
    assertTrue(registry.isActive(1));
  }

  function testUpdateOwnerOnly() public {
    vm.prank(dev);
    uint256 id = registry.register(payout, manifestHash, pricingHash, true, false);
    bytes32 newManifest = keccak256("manifest2");
    vm.prank(dev);
    registry.update(id, newManifest, pricingHash);
    assertEq(registry.getService(id).manifestHash, newManifest);

    vm.prank(address(0xDEAD));
    vm.expectRevert(ServiceRegistry.NotOwner.selector);
    registry.update(id, manifestHash, pricingHash);
  }

  function testSetStatusOwnerAndGovernor() public {
    hoax(dev);
    uint256 id = registry.register(payout, manifestHash, pricingHash, false, false);
    hoax(dev);
    registry.setStatus(id, 2);
    assertEq(registry.getService(id).status, 2);

    address storedGov = registry.governor();
    hoax(storedGov);
    registry.setStatus(id, 1);
    assertTrue(registry.isActive(id));
  }

  function testTransferOwner() public {
    vm.prank(dev);
    uint256 id = registry.register(payout, manifestHash, pricingHash, false, false);
    address newOwner = address(0x1234);
    vm.prank(dev);
    registry.transferOwner(id, newOwner);
    assertEq(registry.ownerOf(id), newOwner);
  }
}
