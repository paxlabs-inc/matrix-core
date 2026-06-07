package deployer

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/paxlabs-inc/tachyon-tools/internal/chains"
	"github.com/paxlabs-inc/tachyon-tools/internal/compiler"
	"github.com/paxlabs-inc/tachyon-tools/internal/evm"
	"github.com/paxlabs-inc/tachyon-tools/internal/registry"
	"github.com/paxlabs-inc/tachyon-tools/internal/wallet"
	"github.com/paxlabs-inc/tachyon-tools/pkg/types"
)

// Deployer plans and executes contract deployments.
type Deployer struct {
	Chains      *chains.Manager
	Reg         *registry.Registry
	Wallet      *wallet.Gate
	ProjectRoot string
}

// Deploy executes an intent-based deployment with idempotency.
func (d *Deployer) Deploy(ctx context.Context, req types.DeployRequest) (types.DeployResponse, *types.Error) {
	key := strings.TrimSpace(req.IdempotencyKey)
	chainKey := strings.TrimSpace(req.ChainID)
	contract := strings.TrimSpace(req.Contract)
	if key == "" || chainKey == "" || contract == "" {
		return types.DeployResponse{}, types.NewError(types.CodeInvalidRequest, "idempotency_key, chain_id, contract required", false, nil)
	}

	if rec, ok := d.Reg.GetDeployment(key, chainKey); ok && rec.Address != "" {
		if confirmed, _ := d.confirmOnChain(ctx, chainKey, rec.Address); confirmed {
			return types.DeployResponse{
				Address:        rec.Address,
				TxHash:         rec.TxHash,
				IdempotencyKey: key,
				ChainID:        chainKey,
				Contract:       rec.Contract,
				Existing:       true,
			}, nil
		}
	}

	if !d.Wallet.Configured() {
		return types.DeployResponse{}, types.NewError(types.CodeWalletNotConfigured, "no signer configured; set wallet mode in tachyon.config.kvx", false, nil)
	}

	projectID := strings.TrimSpace(req.ProjectID)
	root := d.ProjectRoot
	if projectID == "" {
		projectID = compiler.ProjectID(root)
	}
	artRec, ok := d.Reg.GetArtifact(projectID, contract)
	if !ok {
		return types.DeployResponse{}, types.NewError(types.CodeArtifactNotFound, "artifact not found: "+contract, false, nil)
	}

	profile, cerr := d.Chains.Resolve(chainKey, "", d.Reg.ActiveChainID())
	if cerr != nil {
		return types.DeployResponse{}, cerr
	}
	client, err := evm.Dial(profile.RPCURL, profile.ChainID)
	if err != nil {
		return types.DeployResponse{}, types.NewError(types.CodeChainRPCFailed, err.Error(), true, nil)
	}

	initCode, err := packConstructor(artRec, req.ConstructorArgs)
	if err != nil {
		return types.DeployResponse{}, types.NewError(types.CodeDeployFailed, err.Error(), false, nil)
	}

	// Build the signing intent. CREATE2 routes through a deterministic factory
	// (calldata = salt ++ initCode); plain deploys are a creation tx (to = nil).
	intent := wallet.TxIntent{From: strings.TrimSpace(req.From), Value: big.NewInt(0)}
	var create2Addr common.Address
	if req.Create2 != nil {
		create2Addr, err = computeCreate2(req.Create2.Deployer, req.Create2.Salt, initCode)
		if err != nil {
			return types.DeployResponse{}, types.NewError(types.CodeDeployFailed, err.Error(), false, nil)
		}
		if confirmed, _ := d.confirmOnChain(ctx, chainKey, create2Addr.Hex()); confirmed {
			return d.recordAndReturn(key, chainKey, contract, projectID, create2Addr.Hex(), "", true)
		}
		salt, _ := hex.DecodeString(strings.TrimPrefix(req.Create2.Salt, "0x"))
		intent.To = req.Create2.Deployer
		intent.Data = append(salt, initCode...)
	} else {
		intent.Data = initCode
	}

	spendCap, _ := wallet.ParseSpendCap(req.SpendCapWei)
	policy, perr := d.Wallet.Authorize(req.CapabilityToken, spendCap, chainKey)
	if perr != nil {
		return types.DeployResponse{}, perr
	}
	res, werr := d.Wallet.Sign(ctx, client, intent, policy)
	if werr != nil {
		return types.DeployResponse{}, werr
	}

	txHash := res.TxHash
	if len(res.RawTx) > 0 {
		txHash, err = client.SendRawTransaction(ctx, res.RawTx)
		if err != nil {
			return types.DeployResponse{}, types.NewError(types.CodeDeployFailed, err.Error(), true, nil)
		}
	}
	if txHash == "" {
		return types.DeployResponse{}, types.NewError(types.CodeDeployFailed, "signer produced neither raw tx nor tx hash", false, nil)
	}

	receipt, err := client.WaitReceipt(ctx, txHash)
	if err != nil {
		return types.DeployResponse{}, types.NewError(types.CodeDeployFailed, "receipt timeout: "+err.Error(), true, nil)
	}

	deployAddr := receipt.ContractAddress
	if req.Create2 != nil {
		deployAddr = create2Addr // factory call: address is the deterministic one
	}
	if deployAddr == (common.Address{}) {
		return types.DeployResponse{}, types.NewError(types.CodeDeployFailed, "no deployment address", false, nil)
	}

	return d.recordAndReturn(key, chainKey, contract, projectID, deployAddr.Hex(), txHash, false)
}

func (d *Deployer) recordAndReturn(key, chainKey, contract, projectID, address, txHash string, existing bool) (types.DeployResponse, *types.Error) {
	_ = d.Reg.PutDeployment(registry.DeploymentRecord{
		IdempotencyKey: key,
		ChainID:        chainKey,
		Contract:       contract,
		Address:        address,
		TxHash:         txHash,
		Confirmed:      txHash != "" || existing,
		ProjectID:      projectID,
	})
	return types.DeployResponse{
		Address:        address,
		TxHash:         txHash,
		IdempotencyKey: key,
		ChainID:        chainKey,
		Contract:       contract,
		Existing:       existing,
	}, nil
}

func (d *Deployer) confirmOnChain(ctx context.Context, chainID, address string) (bool, error) {
	profile, chainErr := d.Chains.Resolve(chainID, "", "")
	if chainErr != nil {
		return false, fmt.Errorf("%s", chainErr.Message)
	}
	client, dialErr := evm.Dial(profile.RPCURL, profile.ChainID)
	if dialErr != nil {
		return false, dialErr
	}
	code, codeErr := client.CodeAt(ctx, address)
	if codeErr != nil {
		return false, codeErr
	}
	return len(code) > 0, nil
}

func packConstructor(art registry.ArtifactRecord, args json.RawMessage) ([]byte, error) {
	bytecode, err := hex.DecodeString(strings.TrimPrefix(art.Bytecode, "0x"))
	if err != nil {
		return nil, fmt.Errorf("decode bytecode: %w", err)
	}
	if len(args) == 0 || string(args) == "null" {
		return bytecode, nil
	}
	parsed, err := abi.JSON(bytes.NewReader(art.ABI))
	if err != nil {
		return nil, err
	}
	var rawArgs []interface{}
	if err := json.Unmarshal(args, &rawArgs); err != nil {
		return nil, err
	}
	packed, err := parsed.Constructor.Inputs.Pack(rawArgs...)
	if err != nil {
		return nil, fmt.Errorf("pack constructor: %w", err)
	}
	return append(bytecode, packed...), nil
}

func computeCreate2(deployerHex, saltHex string, initCode []byte) (common.Address, error) {
	salt, err := hex.DecodeString(strings.TrimPrefix(saltHex, "0x"))
	if err != nil || len(salt) != 32 {
		return common.Address{}, fmt.Errorf("create2 salt must be 32 bytes")
	}
	if strings.TrimSpace(deployerHex) == "" {
		return common.Address{}, fmt.Errorf("create2 deployer (factory) address required")
	}
	var salt32 [32]byte
	copy(salt32[:], salt)
	// CreateAddress2 expects the keccak256 hash of the init code, not the code.
	codeHash := crypto.Keccak256(initCode)
	return crypto.CreateAddress2(common.HexToAddress(deployerHex), salt32, codeHash), nil
}
