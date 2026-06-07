package engine

import (
	"context"
	"encoding/hex"
	"math/big"
	"path/filepath"
	"strings"

	"github.com/paxlabs-inc/tachyon-tools/internal/abienc"
	"github.com/paxlabs-inc/tachyon-tools/internal/chains"
	"github.com/paxlabs-inc/tachyon-tools/internal/compiler"
	"github.com/paxlabs-inc/tachyon-tools/internal/config"
	"github.com/paxlabs-inc/tachyon-tools/internal/deployer"
	"github.com/paxlabs-inc/tachyon-tools/internal/evm"
	"github.com/paxlabs-inc/tachyon-tools/internal/registry"
	"github.com/paxlabs-inc/tachyon-tools/internal/simulate"
	"github.com/paxlabs-inc/tachyon-tools/internal/tester"
	"github.com/paxlabs-inc/tachyon-tools/internal/wallet"
	"github.com/paxlabs-inc/tachyon-tools/pkg/types"
)

const Version = "0.1.0"

// Engine is the single implementation of all tachyon verbs.
type Engine struct {
	Cfg       config.Config
	Reg       *registry.Registry
	Chains    *chains.Manager
	Compiler  *compiler.Compiler
	Tester    *tester.Tester
	Simulator *simulate.Simulator
	Deployer  *deployer.Deployer
	Wallet    *wallet.Gate
}

// New wires the engine from config.
func New(cfg config.Config) (*Engine, error) {
	regPath := cfg.RegistryPath
	if !filepath.IsAbs(regPath) {
		regPath = filepath.Join(cfg.ProjectRoot, regPath)
	}
	reg, err := registry.Open(regPath)
	if err != nil {
		return nil, err
	}
	presets := chains.DefaultPresetsPath(cfg.ProjectRoot)
	cm, err := chains.New(presets)
	if err != nil {
		return nil, err
	}
	cm.SetProjectRoot(cfg.ProjectRoot)

	// Register operator-defined chains from tachyon.config.kvx.
	for _, c := range cfg.Chains {
		if c.ID == "" || c.RPCURL == "" {
			continue
		}
		_, _ = cm.Register(types.ChainRegisterRequest{
			ID:       c.ID,
			Name:     c.Name,
			RPCURL:   c.RPCURL,
			ChainID:  c.ChainID,
			Explorer: c.Explorer,
		})
	}

	gate, err := wallet.NewGate(cfg)
	if err != nil {
		return nil, err
	}

	artifactsDir := cfg.ArtifactsDir
	if !filepath.IsAbs(artifactsDir) {
		artifactsDir = filepath.Join(cfg.ProjectRoot, artifactsDir)
	}

	e := &Engine{
		Cfg:    cfg,
		Reg:    reg,
		Chains: cm,
		Compiler: &compiler.Compiler{
			ForgePath:    cfg.ForgePath,
			ArtifactsDir: artifactsDir,
		},
		Tester:    &tester.Tester{ForgePath: cfg.ForgePath},
		Simulator: &simulate.Simulator{Chains: cm},
		Deployer: &deployer.Deployer{
			Chains:      cm,
			Reg:         reg,
			Wallet:      gate,
			ProjectRoot: cfg.ProjectRoot,
		},
		Wallet: gate,
	}
	return e, nil
}

func (e *Engine) defaultRoot(reqRoot string) string {
	if strings.TrimSpace(reqRoot) != "" {
		return reqRoot
	}
	return e.Cfg.ProjectRoot
}

// Compile builds contracts. When req.Sources is non-empty the request is
// self-contained: the source set is materialized into an ephemeral Foundry
// project (the box's forge-std + @openzeppelin corpus linked in) and the
// ProjectID is derived deterministically from the sources so a later
// deploy/call resolves the same artifacts.
func (e *Engine) Compile(ctx context.Context, req types.CompileRequest) types.Envelope[types.CompileResponse] {
	_ = ctx
	if len(req.Sources) > 0 {
		dir, cleanup, perr := e.prepareSourceWorkdir(req.Sources)
		if perr != nil {
			return types.Fail[types.CompileResponse](perr)
		}
		defer cleanup()
		req.ProjectRoot = dir
		if strings.TrimSpace(req.ProjectID) == "" {
			req.ProjectID = sourcesProjectID(req.Sources)
		}
	} else {
		req.ProjectRoot = e.defaultRoot(req.ProjectRoot)
	}
	data, err := e.Compiler.Compile(req, e.Reg)
	if err != nil {
		return types.Fail[types.CompileResponse](err)
	}
	return types.OK(data)
}

// Test runs forge tests. Like Compile, a non-empty req.Sources runs the suite
// in an ephemeral uploaded-source workdir.
func (e *Engine) Test(ctx context.Context, req types.TestRequest) types.Envelope[types.TestResponse] {
	_ = ctx
	if len(req.Sources) > 0 {
		dir, cleanup, perr := e.prepareSourceWorkdir(req.Sources)
		if perr != nil {
			return types.Fail[types.TestResponse](perr)
		}
		defer cleanup()
		req.ProjectRoot = dir
	} else {
		req.ProjectRoot = e.defaultRoot(req.ProjectRoot)
	}
	data, err := e.Tester.Test(req)
	if err != nil {
		// Return partial results with failure envelope
		if data.Passed > 0 || data.Failed > 0 {
			env := types.Fail[types.TestResponse](err)
			env.Data = data
			return env
		}
		return types.Fail[types.TestResponse](err)
	}
	return types.OK(data)
}

// Simulate dry-runs a call.
func (e *Engine) Simulate(ctx context.Context, req types.SimulateRequest) types.Envelope[types.SimulateResponse] {
	data, err := e.Simulator.Simulate(ctx, req, e.Reg.ActiveChainID())
	if err != nil {
		if data.Revert != "" {
			env := types.Fail[types.SimulateResponse](err)
			env.Data = data
			return env
		}
		return types.Fail[types.SimulateResponse](err)
	}
	return types.OK(data)
}

// Deploy deploys a contract.
func (e *Engine) Deploy(ctx context.Context, req types.DeployRequest) types.Envelope[types.DeployResponse] {
	data, err := e.Deployer.Deploy(ctx, req)
	if err != nil {
		return types.Fail[types.DeployResponse](err)
	}
	return types.OK(data)
}

// Call invokes or simulates a contract call. Calldata is either ABI-encoded
// from Method+Args (ABI resolved inline or from the registry) or taken verbatim
// from the pre-encoded hex Data field.
func (e *Engine) Call(ctx context.Context, req types.CallRequest) types.Envelope[types.CallResponse] {
	if strings.TrimSpace(req.To) == "" {
		return types.Fail[types.CallResponse](types.NewError(types.CodeInvalidRequest, "to required", false, nil))
	}

	data, derr := e.callData(req)
	if derr != nil {
		return types.Fail[types.CallResponse](derr)
	}

	if req.SimulateOnly {
		sim, err := e.Simulator.Simulate(ctx, types.SimulateRequest{
			ChainID: req.ChainID,
			RPCURL:  req.RPCURL,
			From:    req.From,
			To:      req.To,
			Data:    "0x" + hex.EncodeToString(data),
			Value:   req.Value,
		}, e.Reg.ActiveChainID())
		if err != nil {
			return types.Fail[types.CallResponse](err)
		}
		return types.OK(types.CallResponse{Result: sim.Result, Revert: sim.Revert})
	}

	if !e.Wallet.Configured() {
		return types.Fail[types.CallResponse](types.NewError(types.CodeWalletNotConfigured, "broadcast call requires a configured signer; set simulate_only=true or configure a wallet", false, nil))
	}

	profile, cerr := e.Chains.Resolve(req.ChainID, req.RPCURL, e.Reg.ActiveChainID())
	if cerr != nil {
		return types.Fail[types.CallResponse](cerr)
	}
	client, dialErr := evm.Dial(profile.RPCURL, profile.ChainID)
	if dialErr != nil {
		return types.Fail[types.CallResponse](types.NewError(types.CodeChainRPCFailed, dialErr.Error(), true, nil))
	}

	value, ok := parseWei(req.Value)
	if !ok {
		return types.Fail[types.CallResponse](types.NewError(types.CodeInvalidRequest, "invalid value", false, nil))
	}

	chainKey := req.ChainID
	if chainKey == "" {
		chainKey = e.Reg.ActiveChainID()
	}
	spendCap, _ := wallet.ParseSpendCap(req.SpendCapWei)
	policy, perr := e.Wallet.Authorize(req.CapabilityToken, spendCap, chainKey)
	if perr != nil {
		return types.Fail[types.CallResponse](perr)
	}
	res, werr := e.Wallet.Sign(ctx, client, wallet.TxIntent{
		From:      req.From,
		To:        req.To,
		Data:      data,
		Value:     value,
		AuthToken: strings.TrimSpace(req.WalletToken),
	}, policy)
	if werr != nil {
		return types.Fail[types.CallResponse](werr)
	}

	txHash := res.TxHash
	if len(res.RawTx) > 0 {
		var err error
		txHash, err = client.SendRawTransaction(ctx, res.RawTx)
		if err != nil {
			return types.Fail[types.CallResponse](types.NewError(types.CodeCallFailed, err.Error(), true, nil))
		}
	}
	if txHash == "" {
		return types.Fail[types.CallResponse](types.NewError(types.CodeCallFailed, "signer produced neither raw tx nor tx hash", false, nil))
	}
	return types.OK(types.CallResponse{TxHash: txHash})
}

// callData resolves the calldata for a call: ABI-encoded from Method+Args when
// Method is set, otherwise the pre-encoded hex Data field.
func (e *Engine) callData(req types.CallRequest) ([]byte, *types.Error) {
	if strings.TrimSpace(req.Method) != "" {
		abiJSON, aerr := e.resolveABI(req)
		if aerr != nil {
			return nil, aerr
		}
		data, err := abienc.Pack(abiJSON, req.Method, req.Args)
		if err != nil {
			return nil, types.NewError(types.CodeInvalidRequest, "encode call: "+err.Error(), false, nil)
		}
		return data, nil
	}
	data, err := hex.DecodeString(strings.TrimPrefix(req.Data, "0x"))
	if err != nil {
		return nil, types.NewError(types.CodeInvalidRequest, "data must be hex: "+err.Error(), false, nil)
	}
	return data, nil
}

// resolveABI returns the ABI JSON to encode against: an inline ABI if provided,
// otherwise the ABI of the named contract artifact from the registry.
func (e *Engine) resolveABI(req types.CallRequest) ([]byte, *types.Error) {
	if len(req.ABI) > 0 && strings.TrimSpace(string(req.ABI)) != "null" {
		return req.ABI, nil
	}
	if strings.TrimSpace(req.Contract) == "" {
		return nil, types.NewError(types.CodeInvalidRequest, "method encoding requires an inline abi or a contract name to resolve the abi from the registry", false, nil)
	}
	projectID := strings.TrimSpace(req.ProjectID)
	if projectID == "" {
		projectID = compiler.ProjectID(e.Cfg.ProjectRoot)
	}
	rec, ok := e.Reg.GetArtifact(projectID, req.Contract)
	if !ok {
		return nil, types.NewError(types.CodeArtifactNotFound, "artifact not found for abi: "+req.Contract, false, nil)
	}
	return rec.ABI, nil
}

// parseWei parses a decimal or 0x-hex wei amount; empty => 0.
func parseWei(s string) (*big.Int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return big.NewInt(0), true
	}
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		v, ok := new(big.Int).SetString(s[2:], 16)
		return v, ok
	}
	v, ok := new(big.Int).SetString(s, 10)
	return v, ok
}

// ChainList lists chain profiles.
func (e *Engine) ChainList() types.Envelope[types.ChainListResponse] {
	return types.OK(e.Chains.List(e.Reg.ActiveChainID()))
}

// ChainRegister adds a custom chain.
func (e *Engine) ChainRegister(req types.ChainRegisterRequest) types.Envelope[types.ChainProfile] {
	p, err := e.Chains.Register(req)
	if err != nil {
		return types.Fail[types.ChainProfile](err)
	}
	return types.OK(p)
}

// ChainUse sets active chain.
func (e *Engine) ChainUse(req types.ChainUseRequest) types.Envelope[types.ChainListResponse] {
	if strings.TrimSpace(req.ChainID) == "" {
		return types.Fail[types.ChainListResponse](types.NewError(types.CodeInvalidRequest, "chain_id required", false, nil))
	}
	if _, ok := e.Chains.Get(req.ChainID); !ok {
		return types.Fail[types.ChainListResponse](types.NewError(types.CodeChainNotFound, req.ChainID, false, nil))
	}
	_ = e.Reg.SetActiveChain(req.ChainID)
	return types.OK(e.Chains.List(req.ChainID))
}

// ArtifactGet returns a cached artifact.
func (e *Engine) ArtifactGet(req types.ArtifactGetRequest) types.Envelope[types.ArtifactGetResponse] {
	projectID := req.ProjectID
	if projectID == "" {
		projectID = compiler.ProjectID(e.Cfg.ProjectRoot)
	}
	rec, ok := e.Reg.GetArtifact(projectID, req.Name)
	if !ok {
		return types.Fail[types.ArtifactGetResponse](types.NewError(types.CodeArtifactNotFound, req.Name, false, nil))
	}
	return types.OK(types.ArtifactGetResponse{Artifact: registry.ToTypesArtifact(rec)})
}

// RegistryLookup finds a deployment by idempotency key.
func (e *Engine) RegistryLookup(req types.RegistryLookupRequest) types.Envelope[types.RegistryLookupResponse] {
	rec, ok := e.Reg.GetDeployment(req.IdempotencyKey, req.ChainID)
	if !ok {
		return types.OK(types.RegistryLookupResponse{Found: false})
	}
	return types.OK(types.RegistryLookupResponse{
		Found:     true,
		Address:   rec.Address,
		TxHash:    rec.TxHash,
		Contract:  rec.Contract,
		Confirmed: rec.Confirmed,
	})
}

// Health returns daemon health payload.
func (e *Engine) Health(forgeVersion string) types.HealthData {
	return types.HealthData{
		Version: Version,
		Forge:   forgeVersion,
		Chains:  e.Chains.AvailableIDs(),
		Project: e.Cfg.ProjectRoot,
	}
}

// EncodeContractCall packs ABI method + args into calldata (helper for agents).
func EncodeContractCall(artifactABI []byte, method string, args []interface{}) (string, error) {
	data, err := abienc.Pack(artifactABI, method, args)
	if err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(data), nil
}
