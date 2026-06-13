# Tester

**Source file:** `internal/tester/tester.go`

The tester wraps `forge test` and parses the JSON output into a structured, agent-friendly format. It supports both in-place test runs and self-contained source uploads (same ephemeral workdir logic as the compiler).

---

## Design decisions

### JSON output parsing

Foundry's `forge test --json` emits either a single JSON object (one suite) or NDJSON (multiple suites). The tester handles both:

1. Attempts to parse the entire stdout as a single `map[string]forgeSuite`
2. If that fails, splits by newline and parses each line individually
3. Aggregates all suites into a unified `TestResponse`

This robustness is necessary because Foundry's JSON output format varies by version and test configuration.

### Partial failure handling

If `forge test` exits non-zero but some tests passed, the tester returns a partial result:

```go
env := types.Fail[types.TestResponse](err)
env.Data = data  // partial suite results
return env
```

This lets agents see which tests passed and which failed without re-running.

### Test filtering

`TestRequest` supports three filter dimensions:

- `MatchPath`: `--match-path` (file path pattern)
- `MatchContract`: `--match-contract` (contract name pattern)
- `Filter`: `--match-test` (test function name pattern)

These are passed directly to forge as CLI args.

### Gas reporting

Each test case reports gas consumed:
- Unit tests: `kind.Unit.Gas`
- Fuzz tests: `kind.Fuzz.MeanGas`

This is useful for agents optimizing contract gas usage.

### Source upload support

Like `Compile`, `Test` supports self-contained source uploads. The ephemeral workdir is prepared with the same dependency linking and `foundry.toml` generation. Tests are expected under `test/` in the uploaded source set.

---

## Request/response types

```go
type TestRequest struct {
    ProjectRoot   string            `json:"project_root,omitempty"`
    Sources       map[string]string `json:"sources,omitempty"`
    MatchPath     string            `json:"match_path,omitempty"`
    MatchContract string            `json:"match_contract,omitempty"`
    Filter        string            `json:"filter,omitempty"`
    EVMVersion    string            `json:"evm_version,omitempty"`
}

type TestCaseResult struct {
    Name     string `json:"name"`
    Status   string `json:"status"`
    Reason   string `json:"reason,omitempty"`
    Gas      uint64 `json:"gas,omitempty"`
    Duration string `json:"duration,omitempty"`
}

type TestSuiteResult struct {
    File    string           `json:"file"`
    Passed  int              `json:"passed"`
    Failed  int              `json:"failed"`
    Skipped int              `json:"skipped"`
    Cases   []TestCaseResult `json:"cases"`
}

type TestResponse struct {
    Suites []TestSuiteResult `json:"suites"`
    Passed int               `json:"passed"`
    Failed int               `json:"failed"`
}
```

---

## Error codes

| Code | Retry | Meaning |
|---|---|---|
| `TEST_FORGE_FAILED` | yes | `forge test` subprocess failed |
| `TEST_ASSERTION_FAILED` | no | Tests ran but assertions failed |
| `INVALID_REQUEST` | no | Missing project_root |

---

## Modifying the tester

| What to change | Where |
|---|---|
| Add test coverage | `internal/tester/tester.go` — parse coverage output from forge |
| Add fuzz test details | `internal/tester/tester.go` — expand `forgeTestResult.Kind.Fuzz` |
| Change timeout | `internal/tester/tester.go` — `RunWithTimeout` call |
| Add test profiling | `internal/tester/tester.go` — parse gas profiling output |
