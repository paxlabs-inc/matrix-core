package rpc

import (
	"context"
	"encoding/json"

	"github.com/paxlabs-inc/tachyon-tools/internal/engine"
)

// DispatchForMCP routes MCP tool names through JSON-RPC dispatch.
func DispatchForMCP(ctx context.Context, eng *engine.Engine, method string, params json.RawMessage) (any, *rpcError) {
	return Dispatch(ctx, eng, method, params)
}
