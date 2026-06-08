// Code generated - DO NOT EDIT.
// This file is a generated binding and any manual changes will be lost.

package bindings

import (
	"errors"
	"math/big"
	"strings"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
)

// Reference imports to suppress errors if they are not otherwise used.
var (
	_ = errors.New
	_ = big.NewInt
	_ = strings.NewReader
	_ = ethereum.NotFound
	_ = bind.Bind
	_ = common.Big1
	_ = types.BloomLookup
	_ = event.NewSubscription
	_ = abi.ConvertType
)

// IServiceRegistryService is an auto generated low-level Go binding around an user-defined struct.
type IServiceRegistryService struct {
	Id           *big.Int
	Owner        common.Address
	Payout       common.Address
	ManifestHash [32]byte
	PricingHash  [32]byte
	Status       uint8
	Hosted       bool
	Confidential bool
	RegisteredAt uint64
	UpdatedAt    uint64
}

// ServiceRegistryMetaData contains all meta data concerning the ServiceRegistry contract.
var ServiceRegistryMetaData = &bind.MetaData{
	ABI: "[{\"type\":\"constructor\",\"inputs\":[{\"name\":\"governor_\",\"type\":\"address\",\"internalType\":\"address\"}],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"STATUS_ACTIVE\",\"inputs\":[],\"outputs\":[{\"name\":\"\",\"type\":\"uint8\",\"internalType\":\"uint8\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"STATUS_DELISTED\",\"inputs\":[],\"outputs\":[{\"name\":\"\",\"type\":\"uint8\",\"internalType\":\"uint8\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"STATUS_DRAFT\",\"inputs\":[],\"outputs\":[{\"name\":\"\",\"type\":\"uint8\",\"internalType\":\"uint8\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"STATUS_PAUSED\",\"inputs\":[],\"outputs\":[{\"name\":\"\",\"type\":\"uint8\",\"internalType\":\"uint8\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"getService\",\"inputs\":[{\"name\":\"id\",\"type\":\"uint256\",\"internalType\":\"uint256\"}],\"outputs\":[{\"name\":\"\",\"type\":\"tuple\",\"internalType\":\"structIServiceRegistry.Service\",\"components\":[{\"name\":\"id\",\"type\":\"uint256\",\"internalType\":\"uint256\"},{\"name\":\"owner\",\"type\":\"address\",\"internalType\":\"address\"},{\"name\":\"payout\",\"type\":\"address\",\"internalType\":\"address\"},{\"name\":\"manifestHash\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"pricingHash\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"status\",\"type\":\"uint8\",\"internalType\":\"uint8\"},{\"name\":\"hosted\",\"type\":\"bool\",\"internalType\":\"bool\"},{\"name\":\"confidential\",\"type\":\"bool\",\"internalType\":\"bool\"},{\"name\":\"registeredAt\",\"type\":\"uint64\",\"internalType\":\"uint64\"},{\"name\":\"updatedAt\",\"type\":\"uint64\",\"internalType\":\"uint64\"}]}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"governor\",\"inputs\":[],\"outputs\":[{\"name\":\"\",\"type\":\"address\",\"internalType\":\"address\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"isActive\",\"inputs\":[{\"name\":\"id\",\"type\":\"uint256\",\"internalType\":\"uint256\"}],\"outputs\":[{\"name\":\"\",\"type\":\"bool\",\"internalType\":\"bool\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"nextId\",\"inputs\":[],\"outputs\":[{\"name\":\"\",\"type\":\"uint256\",\"internalType\":\"uint256\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"ownerOf\",\"inputs\":[{\"name\":\"id\",\"type\":\"uint256\",\"internalType\":\"uint256\"}],\"outputs\":[{\"name\":\"\",\"type\":\"address\",\"internalType\":\"address\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"register\",\"inputs\":[{\"name\":\"payout\",\"type\":\"address\",\"internalType\":\"address\"},{\"name\":\"manifestHash\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"pricingHash\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"hosted\",\"type\":\"bool\",\"internalType\":\"bool\"},{\"name\":\"confidential\",\"type\":\"bool\",\"internalType\":\"bool\"}],\"outputs\":[{\"name\":\"id\",\"type\":\"uint256\",\"internalType\":\"uint256\"}],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"services\",\"inputs\":[{\"name\":\"\",\"type\":\"uint256\",\"internalType\":\"uint256\"}],\"outputs\":[{\"name\":\"id\",\"type\":\"uint256\",\"internalType\":\"uint256\"},{\"name\":\"owner\",\"type\":\"address\",\"internalType\":\"address\"},{\"name\":\"payout\",\"type\":\"address\",\"internalType\":\"address\"},{\"name\":\"manifestHash\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"pricingHash\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"status\",\"type\":\"uint8\",\"internalType\":\"uint8\"},{\"name\":\"hosted\",\"type\":\"bool\",\"internalType\":\"bool\"},{\"name\":\"confidential\",\"type\":\"bool\",\"internalType\":\"bool\"},{\"name\":\"registeredAt\",\"type\":\"uint64\",\"internalType\":\"uint64\"},{\"name\":\"updatedAt\",\"type\":\"uint64\",\"internalType\":\"uint64\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"servicesByOwner\",\"inputs\":[{\"name\":\"\",\"type\":\"address\",\"internalType\":\"address\"},{\"name\":\"\",\"type\":\"uint256\",\"internalType\":\"uint256\"}],\"outputs\":[{\"name\":\"\",\"type\":\"uint256\",\"internalType\":\"uint256\"}],\"stateMutability\":\"view\"},{\"type\":\"function\",\"name\":\"setPayout\",\"inputs\":[{\"name\":\"id\",\"type\":\"uint256\",\"internalType\":\"uint256\"},{\"name\":\"payout\",\"type\":\"address\",\"internalType\":\"address\"}],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"setStatus\",\"inputs\":[{\"name\":\"id\",\"type\":\"uint256\",\"internalType\":\"uint256\"},{\"name\":\"status\",\"type\":\"uint8\",\"internalType\":\"uint8\"}],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"transferOwner\",\"inputs\":[{\"name\":\"id\",\"type\":\"uint256\",\"internalType\":\"uint256\"},{\"name\":\"newOwner\",\"type\":\"address\",\"internalType\":\"address\"}],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"function\",\"name\":\"update\",\"inputs\":[{\"name\":\"id\",\"type\":\"uint256\",\"internalType\":\"uint256\"},{\"name\":\"manifestHash\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"},{\"name\":\"pricingHash\",\"type\":\"bytes32\",\"internalType\":\"bytes32\"}],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"type\":\"event\",\"name\":\"OwnerTransferred\",\"inputs\":[{\"name\":\"id\",\"type\":\"uint256\",\"indexed\":true,\"internalType\":\"uint256\"},{\"name\":\"from\",\"type\":\"address\",\"indexed\":true,\"internalType\":\"address\"},{\"name\":\"to\",\"type\":\"address\",\"indexed\":true,\"internalType\":\"address\"}],\"anonymous\":false},{\"type\":\"event\",\"name\":\"PayoutChanged\",\"inputs\":[{\"name\":\"id\",\"type\":\"uint256\",\"indexed\":true,\"internalType\":\"uint256\"},{\"name\":\"payout\",\"type\":\"address\",\"indexed\":false,\"internalType\":\"address\"}],\"anonymous\":false},{\"type\":\"event\",\"name\":\"ServiceRegistered\",\"inputs\":[{\"name\":\"id\",\"type\":\"uint256\",\"indexed\":true,\"internalType\":\"uint256\"},{\"name\":\"owner\",\"type\":\"address\",\"indexed\":true,\"internalType\":\"address\"},{\"name\":\"manifestHash\",\"type\":\"bytes32\",\"indexed\":false,\"internalType\":\"bytes32\"},{\"name\":\"pricingHash\",\"type\":\"bytes32\",\"indexed\":false,\"internalType\":\"bytes32\"},{\"name\":\"hosted\",\"type\":\"bool\",\"indexed\":false,\"internalType\":\"bool\"},{\"name\":\"confidential\",\"type\":\"bool\",\"indexed\":false,\"internalType\":\"bool\"}],\"anonymous\":false},{\"type\":\"event\",\"name\":\"ServiceStatusChanged\",\"inputs\":[{\"name\":\"id\",\"type\":\"uint256\",\"indexed\":true,\"internalType\":\"uint256\"},{\"name\":\"status\",\"type\":\"uint8\",\"indexed\":false,\"internalType\":\"uint8\"}],\"anonymous\":false},{\"type\":\"event\",\"name\":\"ServiceUpdated\",\"inputs\":[{\"name\":\"id\",\"type\":\"uint256\",\"indexed\":true,\"internalType\":\"uint256\"},{\"name\":\"manifestHash\",\"type\":\"bytes32\",\"indexed\":false,\"internalType\":\"bytes32\"},{\"name\":\"pricingHash\",\"type\":\"bytes32\",\"indexed\":false,\"internalType\":\"bytes32\"}],\"anonymous\":false},{\"type\":\"error\",\"name\":\"InvalidStatus\",\"inputs\":[{\"name\":\"status\",\"type\":\"uint8\",\"internalType\":\"uint8\"}]},{\"type\":\"error\",\"name\":\"NotOwner\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"NotOwnerOrGovernor\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"ServiceNotFound\",\"inputs\":[]},{\"type\":\"error\",\"name\":\"ZeroAddress\",\"inputs\":[]}]",
}

// ServiceRegistryABI is the input ABI used to generate the binding from.
// Deprecated: Use ServiceRegistryMetaData.ABI instead.
var ServiceRegistryABI = ServiceRegistryMetaData.ABI

// ServiceRegistry is an auto generated Go binding around an Ethereum contract.
type ServiceRegistry struct {
	ServiceRegistryCaller     // Read-only binding to the contract
	ServiceRegistryTransactor // Write-only binding to the contract
	ServiceRegistryFilterer   // Log filterer for contract events
}

// ServiceRegistryCaller is an auto generated read-only Go binding around an Ethereum contract.
type ServiceRegistryCaller struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// ServiceRegistryTransactor is an auto generated write-only Go binding around an Ethereum contract.
type ServiceRegistryTransactor struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// ServiceRegistryFilterer is an auto generated log filtering Go binding around an Ethereum contract events.
type ServiceRegistryFilterer struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// ServiceRegistrySession is an auto generated Go binding around an Ethereum contract,
// with pre-set call and transact options.
type ServiceRegistrySession struct {
	Contract     *ServiceRegistry  // Generic contract binding to set the session for
	CallOpts     bind.CallOpts     // Call options to use throughout this session
	TransactOpts bind.TransactOpts // Transaction auth options to use throughout this session
}

// ServiceRegistryCallerSession is an auto generated read-only Go binding around an Ethereum contract,
// with pre-set call options.
type ServiceRegistryCallerSession struct {
	Contract *ServiceRegistryCaller // Generic contract caller binding to set the session for
	CallOpts bind.CallOpts          // Call options to use throughout this session
}

// ServiceRegistryTransactorSession is an auto generated write-only Go binding around an Ethereum contract,
// with pre-set transact options.
type ServiceRegistryTransactorSession struct {
	Contract     *ServiceRegistryTransactor // Generic contract transactor binding to set the session for
	TransactOpts bind.TransactOpts          // Transaction auth options to use throughout this session
}

// ServiceRegistryRaw is an auto generated low-level Go binding around an Ethereum contract.
type ServiceRegistryRaw struct {
	Contract *ServiceRegistry // Generic contract binding to access the raw methods on
}

// ServiceRegistryCallerRaw is an auto generated low-level read-only Go binding around an Ethereum contract.
type ServiceRegistryCallerRaw struct {
	Contract *ServiceRegistryCaller // Generic read-only contract binding to access the raw methods on
}

// ServiceRegistryTransactorRaw is an auto generated low-level write-only Go binding around an Ethereum contract.
type ServiceRegistryTransactorRaw struct {
	Contract *ServiceRegistryTransactor // Generic write-only contract binding to access the raw methods on
}

// NewServiceRegistry creates a new instance of ServiceRegistry, bound to a specific deployed contract.
func NewServiceRegistry(address common.Address, backend bind.ContractBackend) (*ServiceRegistry, error) {
	contract, err := bindServiceRegistry(address, backend, backend, backend)
	if err != nil {
		return nil, err
	}
	return &ServiceRegistry{ServiceRegistryCaller: ServiceRegistryCaller{contract: contract}, ServiceRegistryTransactor: ServiceRegistryTransactor{contract: contract}, ServiceRegistryFilterer: ServiceRegistryFilterer{contract: contract}}, nil
}

// NewServiceRegistryCaller creates a new read-only instance of ServiceRegistry, bound to a specific deployed contract.
func NewServiceRegistryCaller(address common.Address, caller bind.ContractCaller) (*ServiceRegistryCaller, error) {
	contract, err := bindServiceRegistry(address, caller, nil, nil)
	if err != nil {
		return nil, err
	}
	return &ServiceRegistryCaller{contract: contract}, nil
}

// NewServiceRegistryTransactor creates a new write-only instance of ServiceRegistry, bound to a specific deployed contract.
func NewServiceRegistryTransactor(address common.Address, transactor bind.ContractTransactor) (*ServiceRegistryTransactor, error) {
	contract, err := bindServiceRegistry(address, nil, transactor, nil)
	if err != nil {
		return nil, err
	}
	return &ServiceRegistryTransactor{contract: contract}, nil
}

// NewServiceRegistryFilterer creates a new log filterer instance of ServiceRegistry, bound to a specific deployed contract.
func NewServiceRegistryFilterer(address common.Address, filterer bind.ContractFilterer) (*ServiceRegistryFilterer, error) {
	contract, err := bindServiceRegistry(address, nil, nil, filterer)
	if err != nil {
		return nil, err
	}
	return &ServiceRegistryFilterer{contract: contract}, nil
}

// bindServiceRegistry binds a generic wrapper to an already deployed contract.
func bindServiceRegistry(address common.Address, caller bind.ContractCaller, transactor bind.ContractTransactor, filterer bind.ContractFilterer) (*bind.BoundContract, error) {
	parsed, err := ServiceRegistryMetaData.GetAbi()
	if err != nil {
		return nil, err
	}
	return bind.NewBoundContract(address, *parsed, caller, transactor, filterer), nil
}

// Call invokes the (constant) contract method with params as input values and
// sets the output to result. The result type might be a single field for simple
// returns, a slice of interfaces for anonymous returns and a struct for named
// returns.
func (_ServiceRegistry *ServiceRegistryRaw) Call(opts *bind.CallOpts, result *[]interface{}, method string, params ...interface{}) error {
	return _ServiceRegistry.Contract.ServiceRegistryCaller.contract.Call(opts, result, method, params...)
}

// Transfer initiates a plain transaction to move funds to the contract, calling
// its default method if one is available.
func (_ServiceRegistry *ServiceRegistryRaw) Transfer(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _ServiceRegistry.Contract.ServiceRegistryTransactor.contract.Transfer(opts)
}

// Transact invokes the (paid) contract method with params as input values.
func (_ServiceRegistry *ServiceRegistryRaw) Transact(opts *bind.TransactOpts, method string, params ...interface{}) (*types.Transaction, error) {
	return _ServiceRegistry.Contract.ServiceRegistryTransactor.contract.Transact(opts, method, params...)
}

// Call invokes the (constant) contract method with params as input values and
// sets the output to result. The result type might be a single field for simple
// returns, a slice of interfaces for anonymous returns and a struct for named
// returns.
func (_ServiceRegistry *ServiceRegistryCallerRaw) Call(opts *bind.CallOpts, result *[]interface{}, method string, params ...interface{}) error {
	return _ServiceRegistry.Contract.contract.Call(opts, result, method, params...)
}

// Transfer initiates a plain transaction to move funds to the contract, calling
// its default method if one is available.
func (_ServiceRegistry *ServiceRegistryTransactorRaw) Transfer(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _ServiceRegistry.Contract.contract.Transfer(opts)
}

// Transact invokes the (paid) contract method with params as input values.
func (_ServiceRegistry *ServiceRegistryTransactorRaw) Transact(opts *bind.TransactOpts, method string, params ...interface{}) (*types.Transaction, error) {
	return _ServiceRegistry.Contract.contract.Transact(opts, method, params...)
}

// STATUSACTIVE is a free data retrieval call binding the contract method 0x4389cc22.
//
// Solidity: function STATUS_ACTIVE() view returns(uint8)
func (_ServiceRegistry *ServiceRegistryCaller) STATUSACTIVE(opts *bind.CallOpts) (uint8, error) {
	var out []interface{}
	err := _ServiceRegistry.contract.Call(opts, &out, "STATUS_ACTIVE")

	if err != nil {
		return *new(uint8), err
	}

	out0 := *abi.ConvertType(out[0], new(uint8)).(*uint8)

	return out0, err

}

// STATUSACTIVE is a free data retrieval call binding the contract method 0x4389cc22.
//
// Solidity: function STATUS_ACTIVE() view returns(uint8)
func (_ServiceRegistry *ServiceRegistrySession) STATUSACTIVE() (uint8, error) {
	return _ServiceRegistry.Contract.STATUSACTIVE(&_ServiceRegistry.CallOpts)
}

// STATUSACTIVE is a free data retrieval call binding the contract method 0x4389cc22.
//
// Solidity: function STATUS_ACTIVE() view returns(uint8)
func (_ServiceRegistry *ServiceRegistryCallerSession) STATUSACTIVE() (uint8, error) {
	return _ServiceRegistry.Contract.STATUSACTIVE(&_ServiceRegistry.CallOpts)
}

// STATUSDELISTED is a free data retrieval call binding the contract method 0xb4334df2.
//
// Solidity: function STATUS_DELISTED() view returns(uint8)
func (_ServiceRegistry *ServiceRegistryCaller) STATUSDELISTED(opts *bind.CallOpts) (uint8, error) {
	var out []interface{}
	err := _ServiceRegistry.contract.Call(opts, &out, "STATUS_DELISTED")

	if err != nil {
		return *new(uint8), err
	}

	out0 := *abi.ConvertType(out[0], new(uint8)).(*uint8)

	return out0, err

}

// STATUSDELISTED is a free data retrieval call binding the contract method 0xb4334df2.
//
// Solidity: function STATUS_DELISTED() view returns(uint8)
func (_ServiceRegistry *ServiceRegistrySession) STATUSDELISTED() (uint8, error) {
	return _ServiceRegistry.Contract.STATUSDELISTED(&_ServiceRegistry.CallOpts)
}

// STATUSDELISTED is a free data retrieval call binding the contract method 0xb4334df2.
//
// Solidity: function STATUS_DELISTED() view returns(uint8)
func (_ServiceRegistry *ServiceRegistryCallerSession) STATUSDELISTED() (uint8, error) {
	return _ServiceRegistry.Contract.STATUSDELISTED(&_ServiceRegistry.CallOpts)
}

// STATUSDRAFT is a free data retrieval call binding the contract method 0x4dd70788.
//
// Solidity: function STATUS_DRAFT() view returns(uint8)
func (_ServiceRegistry *ServiceRegistryCaller) STATUSDRAFT(opts *bind.CallOpts) (uint8, error) {
	var out []interface{}
	err := _ServiceRegistry.contract.Call(opts, &out, "STATUS_DRAFT")

	if err != nil {
		return *new(uint8), err
	}

	out0 := *abi.ConvertType(out[0], new(uint8)).(*uint8)

	return out0, err

}

// STATUSDRAFT is a free data retrieval call binding the contract method 0x4dd70788.
//
// Solidity: function STATUS_DRAFT() view returns(uint8)
func (_ServiceRegistry *ServiceRegistrySession) STATUSDRAFT() (uint8, error) {
	return _ServiceRegistry.Contract.STATUSDRAFT(&_ServiceRegistry.CallOpts)
}

// STATUSDRAFT is a free data retrieval call binding the contract method 0x4dd70788.
//
// Solidity: function STATUS_DRAFT() view returns(uint8)
func (_ServiceRegistry *ServiceRegistryCallerSession) STATUSDRAFT() (uint8, error) {
	return _ServiceRegistry.Contract.STATUSDRAFT(&_ServiceRegistry.CallOpts)
}

// STATUSPAUSED is a free data retrieval call binding the contract method 0x934b57d6.
//
// Solidity: function STATUS_PAUSED() view returns(uint8)
func (_ServiceRegistry *ServiceRegistryCaller) STATUSPAUSED(opts *bind.CallOpts) (uint8, error) {
	var out []interface{}
	err := _ServiceRegistry.contract.Call(opts, &out, "STATUS_PAUSED")

	if err != nil {
		return *new(uint8), err
	}

	out0 := *abi.ConvertType(out[0], new(uint8)).(*uint8)

	return out0, err

}

// STATUSPAUSED is a free data retrieval call binding the contract method 0x934b57d6.
//
// Solidity: function STATUS_PAUSED() view returns(uint8)
func (_ServiceRegistry *ServiceRegistrySession) STATUSPAUSED() (uint8, error) {
	return _ServiceRegistry.Contract.STATUSPAUSED(&_ServiceRegistry.CallOpts)
}

// STATUSPAUSED is a free data retrieval call binding the contract method 0x934b57d6.
//
// Solidity: function STATUS_PAUSED() view returns(uint8)
func (_ServiceRegistry *ServiceRegistryCallerSession) STATUSPAUSED() (uint8, error) {
	return _ServiceRegistry.Contract.STATUSPAUSED(&_ServiceRegistry.CallOpts)
}

// GetService is a free data retrieval call binding the contract method 0xef0e239b.
//
// Solidity: function getService(uint256 id) view returns((uint256,address,address,bytes32,bytes32,uint8,bool,bool,uint64,uint64))
func (_ServiceRegistry *ServiceRegistryCaller) GetService(opts *bind.CallOpts, id *big.Int) (IServiceRegistryService, error) {
	var out []interface{}
	err := _ServiceRegistry.contract.Call(opts, &out, "getService", id)

	if err != nil {
		return *new(IServiceRegistryService), err
	}

	out0 := *abi.ConvertType(out[0], new(IServiceRegistryService)).(*IServiceRegistryService)

	return out0, err

}

// GetService is a free data retrieval call binding the contract method 0xef0e239b.
//
// Solidity: function getService(uint256 id) view returns((uint256,address,address,bytes32,bytes32,uint8,bool,bool,uint64,uint64))
func (_ServiceRegistry *ServiceRegistrySession) GetService(id *big.Int) (IServiceRegistryService, error) {
	return _ServiceRegistry.Contract.GetService(&_ServiceRegistry.CallOpts, id)
}

// GetService is a free data retrieval call binding the contract method 0xef0e239b.
//
// Solidity: function getService(uint256 id) view returns((uint256,address,address,bytes32,bytes32,uint8,bool,bool,uint64,uint64))
func (_ServiceRegistry *ServiceRegistryCallerSession) GetService(id *big.Int) (IServiceRegistryService, error) {
	return _ServiceRegistry.Contract.GetService(&_ServiceRegistry.CallOpts, id)
}

// Governor is a free data retrieval call binding the contract method 0x0c340a24.
//
// Solidity: function governor() view returns(address)
func (_ServiceRegistry *ServiceRegistryCaller) Governor(opts *bind.CallOpts) (common.Address, error) {
	var out []interface{}
	err := _ServiceRegistry.contract.Call(opts, &out, "governor")

	if err != nil {
		return *new(common.Address), err
	}

	out0 := *abi.ConvertType(out[0], new(common.Address)).(*common.Address)

	return out0, err

}

// Governor is a free data retrieval call binding the contract method 0x0c340a24.
//
// Solidity: function governor() view returns(address)
func (_ServiceRegistry *ServiceRegistrySession) Governor() (common.Address, error) {
	return _ServiceRegistry.Contract.Governor(&_ServiceRegistry.CallOpts)
}

// Governor is a free data retrieval call binding the contract method 0x0c340a24.
//
// Solidity: function governor() view returns(address)
func (_ServiceRegistry *ServiceRegistryCallerSession) Governor() (common.Address, error) {
	return _ServiceRegistry.Contract.Governor(&_ServiceRegistry.CallOpts)
}

// IsActive is a free data retrieval call binding the contract method 0x82afd23b.
//
// Solidity: function isActive(uint256 id) view returns(bool)
func (_ServiceRegistry *ServiceRegistryCaller) IsActive(opts *bind.CallOpts, id *big.Int) (bool, error) {
	var out []interface{}
	err := _ServiceRegistry.contract.Call(opts, &out, "isActive", id)

	if err != nil {
		return *new(bool), err
	}

	out0 := *abi.ConvertType(out[0], new(bool)).(*bool)

	return out0, err

}

// IsActive is a free data retrieval call binding the contract method 0x82afd23b.
//
// Solidity: function isActive(uint256 id) view returns(bool)
func (_ServiceRegistry *ServiceRegistrySession) IsActive(id *big.Int) (bool, error) {
	return _ServiceRegistry.Contract.IsActive(&_ServiceRegistry.CallOpts, id)
}

// IsActive is a free data retrieval call binding the contract method 0x82afd23b.
//
// Solidity: function isActive(uint256 id) view returns(bool)
func (_ServiceRegistry *ServiceRegistryCallerSession) IsActive(id *big.Int) (bool, error) {
	return _ServiceRegistry.Contract.IsActive(&_ServiceRegistry.CallOpts, id)
}

// NextId is a free data retrieval call binding the contract method 0x61b8ce8c.
//
// Solidity: function nextId() view returns(uint256)
func (_ServiceRegistry *ServiceRegistryCaller) NextId(opts *bind.CallOpts) (*big.Int, error) {
	var out []interface{}
	err := _ServiceRegistry.contract.Call(opts, &out, "nextId")

	if err != nil {
		return *new(*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)

	return out0, err

}

// NextId is a free data retrieval call binding the contract method 0x61b8ce8c.
//
// Solidity: function nextId() view returns(uint256)
func (_ServiceRegistry *ServiceRegistrySession) NextId() (*big.Int, error) {
	return _ServiceRegistry.Contract.NextId(&_ServiceRegistry.CallOpts)
}

// NextId is a free data retrieval call binding the contract method 0x61b8ce8c.
//
// Solidity: function nextId() view returns(uint256)
func (_ServiceRegistry *ServiceRegistryCallerSession) NextId() (*big.Int, error) {
	return _ServiceRegistry.Contract.NextId(&_ServiceRegistry.CallOpts)
}

// OwnerOf is a free data retrieval call binding the contract method 0x6352211e.
//
// Solidity: function ownerOf(uint256 id) view returns(address)
func (_ServiceRegistry *ServiceRegistryCaller) OwnerOf(opts *bind.CallOpts, id *big.Int) (common.Address, error) {
	var out []interface{}
	err := _ServiceRegistry.contract.Call(opts, &out, "ownerOf", id)

	if err != nil {
		return *new(common.Address), err
	}

	out0 := *abi.ConvertType(out[0], new(common.Address)).(*common.Address)

	return out0, err

}

// OwnerOf is a free data retrieval call binding the contract method 0x6352211e.
//
// Solidity: function ownerOf(uint256 id) view returns(address)
func (_ServiceRegistry *ServiceRegistrySession) OwnerOf(id *big.Int) (common.Address, error) {
	return _ServiceRegistry.Contract.OwnerOf(&_ServiceRegistry.CallOpts, id)
}

// OwnerOf is a free data retrieval call binding the contract method 0x6352211e.
//
// Solidity: function ownerOf(uint256 id) view returns(address)
func (_ServiceRegistry *ServiceRegistryCallerSession) OwnerOf(id *big.Int) (common.Address, error) {
	return _ServiceRegistry.Contract.OwnerOf(&_ServiceRegistry.CallOpts, id)
}

// Services is a free data retrieval call binding the contract method 0xc22c4f43.
//
// Solidity: function services(uint256 ) view returns(uint256 id, address owner, address payout, bytes32 manifestHash, bytes32 pricingHash, uint8 status, bool hosted, bool confidential, uint64 registeredAt, uint64 updatedAt)
func (_ServiceRegistry *ServiceRegistryCaller) Services(opts *bind.CallOpts, arg0 *big.Int) (struct {
	Id           *big.Int
	Owner        common.Address
	Payout       common.Address
	ManifestHash [32]byte
	PricingHash  [32]byte
	Status       uint8
	Hosted       bool
	Confidential bool
	RegisteredAt uint64
	UpdatedAt    uint64
}, error) {
	var out []interface{}
	err := _ServiceRegistry.contract.Call(opts, &out, "services", arg0)

	outstruct := new(struct {
		Id           *big.Int
		Owner        common.Address
		Payout       common.Address
		ManifestHash [32]byte
		PricingHash  [32]byte
		Status       uint8
		Hosted       bool
		Confidential bool
		RegisteredAt uint64
		UpdatedAt    uint64
	})
	if err != nil {
		return *outstruct, err
	}

	outstruct.Id = *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)
	outstruct.Owner = *abi.ConvertType(out[1], new(common.Address)).(*common.Address)
	outstruct.Payout = *abi.ConvertType(out[2], new(common.Address)).(*common.Address)
	outstruct.ManifestHash = *abi.ConvertType(out[3], new([32]byte)).(*[32]byte)
	outstruct.PricingHash = *abi.ConvertType(out[4], new([32]byte)).(*[32]byte)
	outstruct.Status = *abi.ConvertType(out[5], new(uint8)).(*uint8)
	outstruct.Hosted = *abi.ConvertType(out[6], new(bool)).(*bool)
	outstruct.Confidential = *abi.ConvertType(out[7], new(bool)).(*bool)
	outstruct.RegisteredAt = *abi.ConvertType(out[8], new(uint64)).(*uint64)
	outstruct.UpdatedAt = *abi.ConvertType(out[9], new(uint64)).(*uint64)

	return *outstruct, err

}

// Services is a free data retrieval call binding the contract method 0xc22c4f43.
//
// Solidity: function services(uint256 ) view returns(uint256 id, address owner, address payout, bytes32 manifestHash, bytes32 pricingHash, uint8 status, bool hosted, bool confidential, uint64 registeredAt, uint64 updatedAt)
func (_ServiceRegistry *ServiceRegistrySession) Services(arg0 *big.Int) (struct {
	Id           *big.Int
	Owner        common.Address
	Payout       common.Address
	ManifestHash [32]byte
	PricingHash  [32]byte
	Status       uint8
	Hosted       bool
	Confidential bool
	RegisteredAt uint64
	UpdatedAt    uint64
}, error) {
	return _ServiceRegistry.Contract.Services(&_ServiceRegistry.CallOpts, arg0)
}

// Services is a free data retrieval call binding the contract method 0xc22c4f43.
//
// Solidity: function services(uint256 ) view returns(uint256 id, address owner, address payout, bytes32 manifestHash, bytes32 pricingHash, uint8 status, bool hosted, bool confidential, uint64 registeredAt, uint64 updatedAt)
func (_ServiceRegistry *ServiceRegistryCallerSession) Services(arg0 *big.Int) (struct {
	Id           *big.Int
	Owner        common.Address
	Payout       common.Address
	ManifestHash [32]byte
	PricingHash  [32]byte
	Status       uint8
	Hosted       bool
	Confidential bool
	RegisteredAt uint64
	UpdatedAt    uint64
}, error) {
	return _ServiceRegistry.Contract.Services(&_ServiceRegistry.CallOpts, arg0)
}

// ServicesByOwner is a free data retrieval call binding the contract method 0x4f6753be.
//
// Solidity: function servicesByOwner(address , uint256 ) view returns(uint256)
func (_ServiceRegistry *ServiceRegistryCaller) ServicesByOwner(opts *bind.CallOpts, arg0 common.Address, arg1 *big.Int) (*big.Int, error) {
	var out []interface{}
	err := _ServiceRegistry.contract.Call(opts, &out, "servicesByOwner", arg0, arg1)

	if err != nil {
		return *new(*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)

	return out0, err

}

// ServicesByOwner is a free data retrieval call binding the contract method 0x4f6753be.
//
// Solidity: function servicesByOwner(address , uint256 ) view returns(uint256)
func (_ServiceRegistry *ServiceRegistrySession) ServicesByOwner(arg0 common.Address, arg1 *big.Int) (*big.Int, error) {
	return _ServiceRegistry.Contract.ServicesByOwner(&_ServiceRegistry.CallOpts, arg0, arg1)
}

// ServicesByOwner is a free data retrieval call binding the contract method 0x4f6753be.
//
// Solidity: function servicesByOwner(address , uint256 ) view returns(uint256)
func (_ServiceRegistry *ServiceRegistryCallerSession) ServicesByOwner(arg0 common.Address, arg1 *big.Int) (*big.Int, error) {
	return _ServiceRegistry.Contract.ServicesByOwner(&_ServiceRegistry.CallOpts, arg0, arg1)
}

// Register is a paid mutator transaction binding the contract method 0x88fbc623.
//
// Solidity: function register(address payout, bytes32 manifestHash, bytes32 pricingHash, bool hosted, bool confidential) returns(uint256 id)
func (_ServiceRegistry *ServiceRegistryTransactor) Register(opts *bind.TransactOpts, payout common.Address, manifestHash [32]byte, pricingHash [32]byte, hosted bool, confidential bool) (*types.Transaction, error) {
	return _ServiceRegistry.contract.Transact(opts, "register", payout, manifestHash, pricingHash, hosted, confidential)
}

// Register is a paid mutator transaction binding the contract method 0x88fbc623.
//
// Solidity: function register(address payout, bytes32 manifestHash, bytes32 pricingHash, bool hosted, bool confidential) returns(uint256 id)
func (_ServiceRegistry *ServiceRegistrySession) Register(payout common.Address, manifestHash [32]byte, pricingHash [32]byte, hosted bool, confidential bool) (*types.Transaction, error) {
	return _ServiceRegistry.Contract.Register(&_ServiceRegistry.TransactOpts, payout, manifestHash, pricingHash, hosted, confidential)
}

// Register is a paid mutator transaction binding the contract method 0x88fbc623.
//
// Solidity: function register(address payout, bytes32 manifestHash, bytes32 pricingHash, bool hosted, bool confidential) returns(uint256 id)
func (_ServiceRegistry *ServiceRegistryTransactorSession) Register(payout common.Address, manifestHash [32]byte, pricingHash [32]byte, hosted bool, confidential bool) (*types.Transaction, error) {
	return _ServiceRegistry.Contract.Register(&_ServiceRegistry.TransactOpts, payout, manifestHash, pricingHash, hosted, confidential)
}

// SetPayout is a paid mutator transaction binding the contract method 0xe775c165.
//
// Solidity: function setPayout(uint256 id, address payout) returns()
func (_ServiceRegistry *ServiceRegistryTransactor) SetPayout(opts *bind.TransactOpts, id *big.Int, payout common.Address) (*types.Transaction, error) {
	return _ServiceRegistry.contract.Transact(opts, "setPayout", id, payout)
}

// SetPayout is a paid mutator transaction binding the contract method 0xe775c165.
//
// Solidity: function setPayout(uint256 id, address payout) returns()
func (_ServiceRegistry *ServiceRegistrySession) SetPayout(id *big.Int, payout common.Address) (*types.Transaction, error) {
	return _ServiceRegistry.Contract.SetPayout(&_ServiceRegistry.TransactOpts, id, payout)
}

// SetPayout is a paid mutator transaction binding the contract method 0xe775c165.
//
// Solidity: function setPayout(uint256 id, address payout) returns()
func (_ServiceRegistry *ServiceRegistryTransactorSession) SetPayout(id *big.Int, payout common.Address) (*types.Transaction, error) {
	return _ServiceRegistry.Contract.SetPayout(&_ServiceRegistry.TransactOpts, id, payout)
}

// SetStatus is a paid mutator transaction binding the contract method 0xd896dd64.
//
// Solidity: function setStatus(uint256 id, uint8 status) returns()
func (_ServiceRegistry *ServiceRegistryTransactor) SetStatus(opts *bind.TransactOpts, id *big.Int, status uint8) (*types.Transaction, error) {
	return _ServiceRegistry.contract.Transact(opts, "setStatus", id, status)
}

// SetStatus is a paid mutator transaction binding the contract method 0xd896dd64.
//
// Solidity: function setStatus(uint256 id, uint8 status) returns()
func (_ServiceRegistry *ServiceRegistrySession) SetStatus(id *big.Int, status uint8) (*types.Transaction, error) {
	return _ServiceRegistry.Contract.SetStatus(&_ServiceRegistry.TransactOpts, id, status)
}

// SetStatus is a paid mutator transaction binding the contract method 0xd896dd64.
//
// Solidity: function setStatus(uint256 id, uint8 status) returns()
func (_ServiceRegistry *ServiceRegistryTransactorSession) SetStatus(id *big.Int, status uint8) (*types.Transaction, error) {
	return _ServiceRegistry.Contract.SetStatus(&_ServiceRegistry.TransactOpts, id, status)
}

// TransferOwner is a paid mutator transaction binding the contract method 0x1ebe85ba.
//
// Solidity: function transferOwner(uint256 id, address newOwner) returns()
func (_ServiceRegistry *ServiceRegistryTransactor) TransferOwner(opts *bind.TransactOpts, id *big.Int, newOwner common.Address) (*types.Transaction, error) {
	return _ServiceRegistry.contract.Transact(opts, "transferOwner", id, newOwner)
}

// TransferOwner is a paid mutator transaction binding the contract method 0x1ebe85ba.
//
// Solidity: function transferOwner(uint256 id, address newOwner) returns()
func (_ServiceRegistry *ServiceRegistrySession) TransferOwner(id *big.Int, newOwner common.Address) (*types.Transaction, error) {
	return _ServiceRegistry.Contract.TransferOwner(&_ServiceRegistry.TransactOpts, id, newOwner)
}

// TransferOwner is a paid mutator transaction binding the contract method 0x1ebe85ba.
//
// Solidity: function transferOwner(uint256 id, address newOwner) returns()
func (_ServiceRegistry *ServiceRegistryTransactorSession) TransferOwner(id *big.Int, newOwner common.Address) (*types.Transaction, error) {
	return _ServiceRegistry.Contract.TransferOwner(&_ServiceRegistry.TransactOpts, id, newOwner)
}

// Update is a paid mutator transaction binding the contract method 0xb9dccd55.
//
// Solidity: function update(uint256 id, bytes32 manifestHash, bytes32 pricingHash) returns()
func (_ServiceRegistry *ServiceRegistryTransactor) Update(opts *bind.TransactOpts, id *big.Int, manifestHash [32]byte, pricingHash [32]byte) (*types.Transaction, error) {
	return _ServiceRegistry.contract.Transact(opts, "update", id, manifestHash, pricingHash)
}

// Update is a paid mutator transaction binding the contract method 0xb9dccd55.
//
// Solidity: function update(uint256 id, bytes32 manifestHash, bytes32 pricingHash) returns()
func (_ServiceRegistry *ServiceRegistrySession) Update(id *big.Int, manifestHash [32]byte, pricingHash [32]byte) (*types.Transaction, error) {
	return _ServiceRegistry.Contract.Update(&_ServiceRegistry.TransactOpts, id, manifestHash, pricingHash)
}

// Update is a paid mutator transaction binding the contract method 0xb9dccd55.
//
// Solidity: function update(uint256 id, bytes32 manifestHash, bytes32 pricingHash) returns()
func (_ServiceRegistry *ServiceRegistryTransactorSession) Update(id *big.Int, manifestHash [32]byte, pricingHash [32]byte) (*types.Transaction, error) {
	return _ServiceRegistry.Contract.Update(&_ServiceRegistry.TransactOpts, id, manifestHash, pricingHash)
}

// ServiceRegistryOwnerTransferredIterator is returned from FilterOwnerTransferred and is used to iterate over the raw logs and unpacked data for OwnerTransferred events raised by the ServiceRegistry contract.
type ServiceRegistryOwnerTransferredIterator struct {
	Event *ServiceRegistryOwnerTransferred // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *ServiceRegistryOwnerTransferredIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(ServiceRegistryOwnerTransferred)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(ServiceRegistryOwnerTransferred)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *ServiceRegistryOwnerTransferredIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *ServiceRegistryOwnerTransferredIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// ServiceRegistryOwnerTransferred represents a OwnerTransferred event raised by the ServiceRegistry contract.
type ServiceRegistryOwnerTransferred struct {
	Id   *big.Int
	From common.Address
	To   common.Address
	Raw  types.Log // Blockchain specific contextual infos
}

// FilterOwnerTransferred is a free log retrieval operation binding the contract event 0x6ec74f357a0767aee452257973b2576cda349fa0d24adc05664f5ae0ec076030.
//
// Solidity: event OwnerTransferred(uint256 indexed id, address indexed from, address indexed to)
func (_ServiceRegistry *ServiceRegistryFilterer) FilterOwnerTransferred(opts *bind.FilterOpts, id []*big.Int, from []common.Address, to []common.Address) (*ServiceRegistryOwnerTransferredIterator, error) {

	var idRule []interface{}
	for _, idItem := range id {
		idRule = append(idRule, idItem)
	}
	var fromRule []interface{}
	for _, fromItem := range from {
		fromRule = append(fromRule, fromItem)
	}
	var toRule []interface{}
	for _, toItem := range to {
		toRule = append(toRule, toItem)
	}

	logs, sub, err := _ServiceRegistry.contract.FilterLogs(opts, "OwnerTransferred", idRule, fromRule, toRule)
	if err != nil {
		return nil, err
	}
	return &ServiceRegistryOwnerTransferredIterator{contract: _ServiceRegistry.contract, event: "OwnerTransferred", logs: logs, sub: sub}, nil
}

// WatchOwnerTransferred is a free log subscription operation binding the contract event 0x6ec74f357a0767aee452257973b2576cda349fa0d24adc05664f5ae0ec076030.
//
// Solidity: event OwnerTransferred(uint256 indexed id, address indexed from, address indexed to)
func (_ServiceRegistry *ServiceRegistryFilterer) WatchOwnerTransferred(opts *bind.WatchOpts, sink chan<- *ServiceRegistryOwnerTransferred, id []*big.Int, from []common.Address, to []common.Address) (event.Subscription, error) {

	var idRule []interface{}
	for _, idItem := range id {
		idRule = append(idRule, idItem)
	}
	var fromRule []interface{}
	for _, fromItem := range from {
		fromRule = append(fromRule, fromItem)
	}
	var toRule []interface{}
	for _, toItem := range to {
		toRule = append(toRule, toItem)
	}

	logs, sub, err := _ServiceRegistry.contract.WatchLogs(opts, "OwnerTransferred", idRule, fromRule, toRule)
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(ServiceRegistryOwnerTransferred)
				if err := _ServiceRegistry.contract.UnpackLog(event, "OwnerTransferred", log); err != nil {
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseOwnerTransferred is a log parse operation binding the contract event 0x6ec74f357a0767aee452257973b2576cda349fa0d24adc05664f5ae0ec076030.
//
// Solidity: event OwnerTransferred(uint256 indexed id, address indexed from, address indexed to)
func (_ServiceRegistry *ServiceRegistryFilterer) ParseOwnerTransferred(log types.Log) (*ServiceRegistryOwnerTransferred, error) {
	event := new(ServiceRegistryOwnerTransferred)
	if err := _ServiceRegistry.contract.UnpackLog(event, "OwnerTransferred", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}

// ServiceRegistryPayoutChangedIterator is returned from FilterPayoutChanged and is used to iterate over the raw logs and unpacked data for PayoutChanged events raised by the ServiceRegistry contract.
type ServiceRegistryPayoutChangedIterator struct {
	Event *ServiceRegistryPayoutChanged // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *ServiceRegistryPayoutChangedIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(ServiceRegistryPayoutChanged)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(ServiceRegistryPayoutChanged)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *ServiceRegistryPayoutChangedIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *ServiceRegistryPayoutChangedIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// ServiceRegistryPayoutChanged represents a PayoutChanged event raised by the ServiceRegistry contract.
type ServiceRegistryPayoutChanged struct {
	Id     *big.Int
	Payout common.Address
	Raw    types.Log // Blockchain specific contextual infos
}

// FilterPayoutChanged is a free log retrieval operation binding the contract event 0x8a966629c89db229e6eb666b33110dcaf36d6441c7fe1fe3c1aadec0e9a839f4.
//
// Solidity: event PayoutChanged(uint256 indexed id, address payout)
func (_ServiceRegistry *ServiceRegistryFilterer) FilterPayoutChanged(opts *bind.FilterOpts, id []*big.Int) (*ServiceRegistryPayoutChangedIterator, error) {

	var idRule []interface{}
	for _, idItem := range id {
		idRule = append(idRule, idItem)
	}

	logs, sub, err := _ServiceRegistry.contract.FilterLogs(opts, "PayoutChanged", idRule)
	if err != nil {
		return nil, err
	}
	return &ServiceRegistryPayoutChangedIterator{contract: _ServiceRegistry.contract, event: "PayoutChanged", logs: logs, sub: sub}, nil
}

// WatchPayoutChanged is a free log subscription operation binding the contract event 0x8a966629c89db229e6eb666b33110dcaf36d6441c7fe1fe3c1aadec0e9a839f4.
//
// Solidity: event PayoutChanged(uint256 indexed id, address payout)
func (_ServiceRegistry *ServiceRegistryFilterer) WatchPayoutChanged(opts *bind.WatchOpts, sink chan<- *ServiceRegistryPayoutChanged, id []*big.Int) (event.Subscription, error) {

	var idRule []interface{}
	for _, idItem := range id {
		idRule = append(idRule, idItem)
	}

	logs, sub, err := _ServiceRegistry.contract.WatchLogs(opts, "PayoutChanged", idRule)
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(ServiceRegistryPayoutChanged)
				if err := _ServiceRegistry.contract.UnpackLog(event, "PayoutChanged", log); err != nil {
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParsePayoutChanged is a log parse operation binding the contract event 0x8a966629c89db229e6eb666b33110dcaf36d6441c7fe1fe3c1aadec0e9a839f4.
//
// Solidity: event PayoutChanged(uint256 indexed id, address payout)
func (_ServiceRegistry *ServiceRegistryFilterer) ParsePayoutChanged(log types.Log) (*ServiceRegistryPayoutChanged, error) {
	event := new(ServiceRegistryPayoutChanged)
	if err := _ServiceRegistry.contract.UnpackLog(event, "PayoutChanged", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}

// ServiceRegistryServiceRegisteredIterator is returned from FilterServiceRegistered and is used to iterate over the raw logs and unpacked data for ServiceRegistered events raised by the ServiceRegistry contract.
type ServiceRegistryServiceRegisteredIterator struct {
	Event *ServiceRegistryServiceRegistered // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *ServiceRegistryServiceRegisteredIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(ServiceRegistryServiceRegistered)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(ServiceRegistryServiceRegistered)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *ServiceRegistryServiceRegisteredIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *ServiceRegistryServiceRegisteredIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// ServiceRegistryServiceRegistered represents a ServiceRegistered event raised by the ServiceRegistry contract.
type ServiceRegistryServiceRegistered struct {
	Id           *big.Int
	Owner        common.Address
	ManifestHash [32]byte
	PricingHash  [32]byte
	Hosted       bool
	Confidential bool
	Raw          types.Log // Blockchain specific contextual infos
}

// FilterServiceRegistered is a free log retrieval operation binding the contract event 0x499edad45400fdd1a7b009418a05f70bed574f978ed8bc184522937142adc311.
//
// Solidity: event ServiceRegistered(uint256 indexed id, address indexed owner, bytes32 manifestHash, bytes32 pricingHash, bool hosted, bool confidential)
func (_ServiceRegistry *ServiceRegistryFilterer) FilterServiceRegistered(opts *bind.FilterOpts, id []*big.Int, owner []common.Address) (*ServiceRegistryServiceRegisteredIterator, error) {

	var idRule []interface{}
	for _, idItem := range id {
		idRule = append(idRule, idItem)
	}
	var ownerRule []interface{}
	for _, ownerItem := range owner {
		ownerRule = append(ownerRule, ownerItem)
	}

	logs, sub, err := _ServiceRegistry.contract.FilterLogs(opts, "ServiceRegistered", idRule, ownerRule)
	if err != nil {
		return nil, err
	}
	return &ServiceRegistryServiceRegisteredIterator{contract: _ServiceRegistry.contract, event: "ServiceRegistered", logs: logs, sub: sub}, nil
}

// WatchServiceRegistered is a free log subscription operation binding the contract event 0x499edad45400fdd1a7b009418a05f70bed574f978ed8bc184522937142adc311.
//
// Solidity: event ServiceRegistered(uint256 indexed id, address indexed owner, bytes32 manifestHash, bytes32 pricingHash, bool hosted, bool confidential)
func (_ServiceRegistry *ServiceRegistryFilterer) WatchServiceRegistered(opts *bind.WatchOpts, sink chan<- *ServiceRegistryServiceRegistered, id []*big.Int, owner []common.Address) (event.Subscription, error) {

	var idRule []interface{}
	for _, idItem := range id {
		idRule = append(idRule, idItem)
	}
	var ownerRule []interface{}
	for _, ownerItem := range owner {
		ownerRule = append(ownerRule, ownerItem)
	}

	logs, sub, err := _ServiceRegistry.contract.WatchLogs(opts, "ServiceRegistered", idRule, ownerRule)
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(ServiceRegistryServiceRegistered)
				if err := _ServiceRegistry.contract.UnpackLog(event, "ServiceRegistered", log); err != nil {
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseServiceRegistered is a log parse operation binding the contract event 0x499edad45400fdd1a7b009418a05f70bed574f978ed8bc184522937142adc311.
//
// Solidity: event ServiceRegistered(uint256 indexed id, address indexed owner, bytes32 manifestHash, bytes32 pricingHash, bool hosted, bool confidential)
func (_ServiceRegistry *ServiceRegistryFilterer) ParseServiceRegistered(log types.Log) (*ServiceRegistryServiceRegistered, error) {
	event := new(ServiceRegistryServiceRegistered)
	if err := _ServiceRegistry.contract.UnpackLog(event, "ServiceRegistered", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}

// ServiceRegistryServiceStatusChangedIterator is returned from FilterServiceStatusChanged and is used to iterate over the raw logs and unpacked data for ServiceStatusChanged events raised by the ServiceRegistry contract.
type ServiceRegistryServiceStatusChangedIterator struct {
	Event *ServiceRegistryServiceStatusChanged // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *ServiceRegistryServiceStatusChangedIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(ServiceRegistryServiceStatusChanged)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(ServiceRegistryServiceStatusChanged)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *ServiceRegistryServiceStatusChangedIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *ServiceRegistryServiceStatusChangedIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// ServiceRegistryServiceStatusChanged represents a ServiceStatusChanged event raised by the ServiceRegistry contract.
type ServiceRegistryServiceStatusChanged struct {
	Id     *big.Int
	Status uint8
	Raw    types.Log // Blockchain specific contextual infos
}

// FilterServiceStatusChanged is a free log retrieval operation binding the contract event 0x73470480f13514e35624ac2c306df51074c12280b3281f2813826c7783e1153e.
//
// Solidity: event ServiceStatusChanged(uint256 indexed id, uint8 status)
func (_ServiceRegistry *ServiceRegistryFilterer) FilterServiceStatusChanged(opts *bind.FilterOpts, id []*big.Int) (*ServiceRegistryServiceStatusChangedIterator, error) {

	var idRule []interface{}
	for _, idItem := range id {
		idRule = append(idRule, idItem)
	}

	logs, sub, err := _ServiceRegistry.contract.FilterLogs(opts, "ServiceStatusChanged", idRule)
	if err != nil {
		return nil, err
	}
	return &ServiceRegistryServiceStatusChangedIterator{contract: _ServiceRegistry.contract, event: "ServiceStatusChanged", logs: logs, sub: sub}, nil
}

// WatchServiceStatusChanged is a free log subscription operation binding the contract event 0x73470480f13514e35624ac2c306df51074c12280b3281f2813826c7783e1153e.
//
// Solidity: event ServiceStatusChanged(uint256 indexed id, uint8 status)
func (_ServiceRegistry *ServiceRegistryFilterer) WatchServiceStatusChanged(opts *bind.WatchOpts, sink chan<- *ServiceRegistryServiceStatusChanged, id []*big.Int) (event.Subscription, error) {

	var idRule []interface{}
	for _, idItem := range id {
		idRule = append(idRule, idItem)
	}

	logs, sub, err := _ServiceRegistry.contract.WatchLogs(opts, "ServiceStatusChanged", idRule)
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(ServiceRegistryServiceStatusChanged)
				if err := _ServiceRegistry.contract.UnpackLog(event, "ServiceStatusChanged", log); err != nil {
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseServiceStatusChanged is a log parse operation binding the contract event 0x73470480f13514e35624ac2c306df51074c12280b3281f2813826c7783e1153e.
//
// Solidity: event ServiceStatusChanged(uint256 indexed id, uint8 status)
func (_ServiceRegistry *ServiceRegistryFilterer) ParseServiceStatusChanged(log types.Log) (*ServiceRegistryServiceStatusChanged, error) {
	event := new(ServiceRegistryServiceStatusChanged)
	if err := _ServiceRegistry.contract.UnpackLog(event, "ServiceStatusChanged", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}

// ServiceRegistryServiceUpdatedIterator is returned from FilterServiceUpdated and is used to iterate over the raw logs and unpacked data for ServiceUpdated events raised by the ServiceRegistry contract.
type ServiceRegistryServiceUpdatedIterator struct {
	Event *ServiceRegistryServiceUpdated // Event containing the contract specifics and raw log

	contract *bind.BoundContract // Generic contract to use for unpacking event data
	event    string              // Event name to use for unpacking event data

	logs chan types.Log        // Log channel receiving the found contract events
	sub  ethereum.Subscription // Subscription for errors, completion and termination
	done bool                  // Whether the subscription completed delivering logs
	fail error                 // Occurred error to stop iteration
}

// Next advances the iterator to the subsequent event, returning whether there
// are any more events found. In case of a retrieval or parsing error, false is
// returned and Error() can be queried for the exact failure.
func (it *ServiceRegistryServiceUpdatedIterator) Next() bool {
	// If the iterator failed, stop iterating
	if it.fail != nil {
		return false
	}
	// If the iterator completed, deliver directly whatever's available
	if it.done {
		select {
		case log := <-it.logs:
			it.Event = new(ServiceRegistryServiceUpdated)
			if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
				it.fail = err
				return false
			}
			it.Event.Raw = log
			return true

		default:
			return false
		}
	}
	// Iterator still in progress, wait for either a data or an error event
	select {
	case log := <-it.logs:
		it.Event = new(ServiceRegistryServiceUpdated)
		if err := it.contract.UnpackLog(it.Event, it.event, log); err != nil {
			it.fail = err
			return false
		}
		it.Event.Raw = log
		return true

	case err := <-it.sub.Err():
		it.done = true
		it.fail = err
		return it.Next()
	}
}

// Error returns any retrieval or parsing error occurred during filtering.
func (it *ServiceRegistryServiceUpdatedIterator) Error() error {
	return it.fail
}

// Close terminates the iteration process, releasing any pending underlying
// resources.
func (it *ServiceRegistryServiceUpdatedIterator) Close() error {
	it.sub.Unsubscribe()
	return nil
}

// ServiceRegistryServiceUpdated represents a ServiceUpdated event raised by the ServiceRegistry contract.
type ServiceRegistryServiceUpdated struct {
	Id           *big.Int
	ManifestHash [32]byte
	PricingHash  [32]byte
	Raw          types.Log // Blockchain specific contextual infos
}

// FilterServiceUpdated is a free log retrieval operation binding the contract event 0x1a816da97405c32faedafefd131368f926c0909622957bb12592f30367c28671.
//
// Solidity: event ServiceUpdated(uint256 indexed id, bytes32 manifestHash, bytes32 pricingHash)
func (_ServiceRegistry *ServiceRegistryFilterer) FilterServiceUpdated(opts *bind.FilterOpts, id []*big.Int) (*ServiceRegistryServiceUpdatedIterator, error) {

	var idRule []interface{}
	for _, idItem := range id {
		idRule = append(idRule, idItem)
	}

	logs, sub, err := _ServiceRegistry.contract.FilterLogs(opts, "ServiceUpdated", idRule)
	if err != nil {
		return nil, err
	}
	return &ServiceRegistryServiceUpdatedIterator{contract: _ServiceRegistry.contract, event: "ServiceUpdated", logs: logs, sub: sub}, nil
}

// WatchServiceUpdated is a free log subscription operation binding the contract event 0x1a816da97405c32faedafefd131368f926c0909622957bb12592f30367c28671.
//
// Solidity: event ServiceUpdated(uint256 indexed id, bytes32 manifestHash, bytes32 pricingHash)
func (_ServiceRegistry *ServiceRegistryFilterer) WatchServiceUpdated(opts *bind.WatchOpts, sink chan<- *ServiceRegistryServiceUpdated, id []*big.Int) (event.Subscription, error) {

	var idRule []interface{}
	for _, idItem := range id {
		idRule = append(idRule, idItem)
	}

	logs, sub, err := _ServiceRegistry.contract.WatchLogs(opts, "ServiceUpdated", idRule)
	if err != nil {
		return nil, err
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case log := <-logs:
				// New log arrived, parse the event and forward to the user
				event := new(ServiceRegistryServiceUpdated)
				if err := _ServiceRegistry.contract.UnpackLog(event, "ServiceUpdated", log); err != nil {
					return err
				}
				event.Raw = log

				select {
				case sink <- event:
				case err := <-sub.Err():
					return err
				case <-quit:
					return nil
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	}), nil
}

// ParseServiceUpdated is a log parse operation binding the contract event 0x1a816da97405c32faedafefd131368f926c0909622957bb12592f30367c28671.
//
// Solidity: event ServiceUpdated(uint256 indexed id, bytes32 manifestHash, bytes32 pricingHash)
func (_ServiceRegistry *ServiceRegistryFilterer) ParseServiceUpdated(log types.Log) (*ServiceRegistryServiceUpdated, error) {
	event := new(ServiceRegistryServiceUpdated)
	if err := _ServiceRegistry.contract.UnpackLog(event, "ServiceUpdated", log); err != nil {
		return nil, err
	}
	event.Raw = log
	return event, nil
}
