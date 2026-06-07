package simulate

import (
	"context"
	"encoding/hex"
	"strings"
	"time"

	"github.com/paxlabs-inc/tachyon-tools/internal/chains"
	"github.com/paxlabs-inc/tachyon-tools/internal/evm"
	"github.com/paxlabs-inc/tachyon-tools/pkg/types"
)

// Simulator performs eth_call dry runs.
type Simulator struct {
	Chains *chains.Manager
}

// Simulate executes eth_call (and optional trace).
func (s *Simulator) Simulate(ctx context.Context, req types.SimulateRequest, activeChain string) (types.SimulateResponse, *types.Error) {
	if strings.TrimSpace(req.To) == "" {
		return types.SimulateResponse{}, types.NewError(types.CodeInvalidRequest, "to required", false, nil)
	}
	profile, err := s.Chains.Resolve(req.ChainID, req.RPCURL, activeChain)
	if err != nil {
		return types.SimulateResponse{}, err
	}
	client, dialErr := evm.Dial(profile.RPCURL, profile.ChainID)
	if dialErr != nil {
		return types.SimulateResponse{}, types.NewError(types.CodeChainRPCFailed, dialErr.Error(), true, nil)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	result, callErr := client.CallMessage(cctx, req.From, req.To, req.Data, req.Value, req.Block)
	gas, _ := client.EstimateGas(cctx, req.From, req.To, req.Data, req.Value)

	resp := types.SimulateResponse{GasEstimate: gas}
	if callErr != nil {
		resp.Revert = callErr.Error()
		return resp, types.NewError(types.CodeSimulateFailed, callErr.Error(), false, nil)
	}
	resp.Result = "0x" + hex.EncodeToString(result)

	if req.Trace {
		if trace, terr := client.TraceCall(cctx, req.From, req.To, req.Data, req.Value); terr == nil {
			resp.Trace = trace
		}
	}
	return resp, nil
}
