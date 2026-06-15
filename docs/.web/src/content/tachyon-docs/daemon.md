# Daemon

**Source file:** `cmd/tachyond/main.go`

The daemon entry point wires configuration, engine, and transport layers. It supports three modes: HTTP API server, MCP stdio server, and selftest.

---

## Design decisions

### Flag-based mode selection

Three mutually exclusive modes, selected by flags:

```sh
./bin/tachyond              # HTTP API server (default)
./bin/tachyond --mcp        # MCP stdio server
./bin/tachyond --selftest   # MCP tool registry selftest, then exit
```

The `--mcp` flag redirects logs to stderr so stdout is reserved for NDJSON-RPC. The `--selftest` flag verifies tool registry consistency and exits with code 0 or 1.

### Signal handling

The HTTP server runs in a goroutine; the main thread blocks on `signal.Notify` for `SIGINT`/`SIGTERM`. On signal, it initiates a graceful shutdown with a 10-second timeout.

```go
sig := make(chan os.Signal, 1)
signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
<-sig

ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
srv.Shutdown(ctx)
```

### Fatal error handling

Config load failures and engine init failures are fatal (`os.Exit(1)`). Server failures in the HTTP goroutine are also fatal. This is a daemon, not a library — crashing on misconfiguration is preferred over limping with undefined behavior.

### No daemonization

The daemon does not fork, write PID files, or implement systemd notify. It is designed to run under a process supervisor (systemd, Docker, etc.) that handles these concerns.

---

## Startup flow

```
Parse flags (--mcp, --selftest)
    │
    ▼
Configure logger (stdout for HTTP, stderr for MCP/selftest)
    │
    ▼
Selftest? → verify tool registry → exit 0/1
    │
    ▼
Load config (tachyon.config.kvx + environment)
    │
    ▼
Initialize engine (registry, chains, compiler, tester, simulator, deployer, wallet)
    │
    ▼
MCP mode? → RunStdio(eng) → block on stdin
    │
    ▼
HTTP mode? → api.New(eng, logger) → ListenAndServe → block on signal
```

---

## Modifying the daemon

| What to change | Where |
|---|---|
| Add flag | `cmd/tachyond/main.go` — `flag` declarations |
| Add startup mode | `cmd/tachyond/main.go` — switch after flag parse |
| Change shutdown timeout | `cmd/tachyond/main.go` — `context.WithTimeout` |
| Add health check | `pkg/api/server.go` — `handleHealthz` |
