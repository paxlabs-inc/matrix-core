package types

// TestRequest runs forge tests.
type TestRequest struct {
	ProjectRoot   string `json:"project_root,omitempty"`
	MatchPath     string `json:"match_path,omitempty"`
	MatchContract string `json:"match_contract,omitempty"`
	Filter        string `json:"filter,omitempty"`
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
