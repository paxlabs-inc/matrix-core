package rpc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/paxlabs-inc/tachyon-tools/internal/engine"
	"github.com/paxlabs-inc/tachyon-tools/pkg/types"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ServeHTTP handles JSON-RPC 2.0 tachyon_* methods.
func ServeHTTP(w http.ResponseWriter, r *http.Request, eng *engine.Engine) {
	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		write(w, response{JSONRPC: "2.0", ID: nil, Error: &rpcError{Code: -32700, Message: err.Error()}})
		return
	}
	result, err := Dispatch(r.Context(), eng, req.Method, req.Params)
	if err != nil {
		write(w, response{JSONRPC: "2.0", ID: req.ID, Error: err})
		return
	}
	write(w, response{JSONRPC: "2.0", ID: req.ID, Result: result})
}

// Dispatch routes tachyon_* JSON-RPC methods to the engine.
func Dispatch(ctx context.Context, eng *engine.Engine, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "tachyon_compile":
		var p types.CompileRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		return eng.Compile(ctx, p), nil
	case "tachyon_test":
		var p types.TestRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		return eng.Test(ctx, p), nil
	case "tachyon_simulate":
		var p types.SimulateRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		return eng.Simulate(ctx, p), nil
	case "tachyon_deploy":
		var p types.DeployRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		return eng.Deploy(ctx, p), nil
	case "tachyon_call":
		var p types.CallRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		return eng.Call(ctx, p), nil
	case "tachyon_chain_list":
		return eng.ChainList(), nil
	case "tachyon_chain_register":
		var p types.ChainRegisterRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		return eng.ChainRegister(p), nil
	case "tachyon_chain_use":
		var p types.ChainUseRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		return eng.ChainUse(p), nil
	case "tachyon_artifact_get":
		var p types.ArtifactGetRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		return eng.ArtifactGet(p), nil
	case "tachyon_registry_lookup":
		var p types.RegistryLookupRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		return eng.RegistryLookup(p), nil
	case "tachyon_health":
		return types.OK(eng.Health("")), nil
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + method}
	}
}

func write(w http.ResponseWriter, resp response) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// DecodeParams helper for tests.
func DecodeParams(params json.RawMessage, v any) error {
	if len(params) == 0 {
		return io.EOF
	}
	return json.Unmarshal(params, v)
}
