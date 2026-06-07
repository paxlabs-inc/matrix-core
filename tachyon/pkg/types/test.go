package types

// TestRequest runs forge tests.
//
// Sources mirrors CompileRequest.Sources: a non-empty map (workdir-relative
// path -> content) makes the request self-contained so a shared tachyond can
// test a caller's uploaded contracts in an ephemeral Foundry project (tests
// under test/, contracts under src/; forge-std + @openzeppelin available).
type TestRequest struct {
	ProjectRoot   string            `json:"project_root,omitempty"`
	Sources       map[string]string `json:"sources,omitempty"`
	MatchPath     string            `json:"match_path,omitempty"`
	MatchContract string            `json:"match_contract,omitempty"`
	Filter        string            `json:"filter,omitempty"`
	// EVMVersion pins the solc/EVM target for an uploaded-source test build
	// (e.g. "shanghai", "paris", "cancun"). Empty uses the engine's
	// conservative default (shanghai), matching the compile/deploy target so
	// the suite exercises the same opcode set that will run on-chain.
	EVMVersion string `json:"evm_version,omitempty"`
}

// TestCaseResult is one forge test outcome.
type TestCaseResult struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Gas      uint64 `json:"gas,omitempty"`
	Duration string `json:"duration,omitempty"`
}

// TestSuiteResult groups cases from one .t.sol file.
type TestSuiteResult struct {
	File    string           `json:"file"`
	Passed  int              `json:"passed"`
	Failed  int              `json:"failed"`
	Skipped int              `json:"skipped"`
	Cases   []TestCaseResult `json:"cases"`
}

// TestResponse aggregates forge test output.
type TestResponse struct {
	Suites []TestSuiteResult `json:"suites"`
	Passed int               `json:"passed"`
	Failed int               `json:"failed"`
}
