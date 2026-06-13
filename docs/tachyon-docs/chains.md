# Chains

**Source file:** `internal/chains/chains.go`

The chain manager loads preset chain profiles from a JSON file and maintains a map of custom chains registered at runtime. It resolves chain references by profile ID, numeric chain ID, or inline RPC URL.

---

## Design decisions

### Presets + custom registration

Chain profiles are loaded from `chains/presets.json` at startup. Operators can add custom chains via the API (`chain.register`) or config (`tachyon.config.kvx` `[chains.*]` sections). Custom chains are stored in-memory only (not persisted to disk).

### Numeric chain-id fallback

Callers often pass the numeric EVM chain ID (`"125"`) rather than the profile ID (`"paxeer-mainnet"`). The manager maps numeric IDs to profiles:

```go
if n, err := strconv.ParseUint(id, 10, 64); err == nil && n != 0 {
    for _, p := range m.presets {
        if p.ChainID == n { return p, true }
    }
}
```

This dual lookup makes the API more ergonomic for agents that know the chain number but not the profile name.

### Environment-variable RPC URLs

Presets can specify `rpc_url_env` instead of `rpc_url`. The manager resolves the env var at lookup time:

```json
{
  "id": "paxeer-mainnet",
  "rpc_url_env": "PAXEER_RPC_URL",
  "chain_id": 125
}
```

This keeps secrets out of the presets file and lets operators set RPC URLs via environment.

### Inline RPC override

Any verb that takes `rpc_url` directly (simulate, call, deploy) can bypass the chain manager entirely. The `Resolve` method returns an inline profile when `rpc_url` is set:

```go
if rpcURL != "" {
    return types.ChainProfile{ID: "inline", Name: "inline", RPCURL: rpcURL, ChainID: 0}, nil
}
```

This is useful for one-off calls to arbitrary RPCs without registering a chain profile.

### Active chain tracking

The active chain ID is stored in the registry (not the chain manager). The engine calls `Reg.SetActiveChain` on `chain.use` and reads `Reg.ActiveChainID` as the default for chain-facing verbs.

---

## Preset file format

```json
{
  "chains": [
    {
      "id": "paxeer-mainnet",
      "name": "Paxeer Mainnet",
      "rpc_url_env": "PAXEER_RPC_URL",
      "chain_id": 125,
      "preset": "paxeer",
      "explorer": "https://paxscan.paxeer.app",
      "features": ["debug_trace"]
    }
  ]
}
```

---

## Chain profile type

```go
type ChainProfile struct {
    ID         string   `json:"id"`
    Name       string   `json:"name"`
    RPCURL     string   `json:"rpc_url,omitempty"`
    RPCURLEnv  string   `json:"rpc_url_env,omitempty"`
    ChainID    uint64   `json:"chain_id"`
    Preset     string   `json:"preset,omitempty"`
    Explorer   string   `json:"explorer,omitempty"`
    Features   []string `json:"features,omitempty"`
    Active     bool     `json:"active,omitempty"`
}
```

---

## Modifying the chains manager

| What to change | Where |
|---|---|
| Add preset | `chains/presets.json` |
| Add feature flag | `internal/chains/chains.go` — `ChainProfile.Features` |
| Persist custom chains | `internal/chains/chains.go` — save to file on register |
| Add chain health check | `internal/chains/chains.go` — `HealthCheck` method |
