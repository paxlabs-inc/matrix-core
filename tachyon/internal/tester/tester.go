package tester

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/paxlabs-inc/tachyon-tools/internal/forgeutil"
	"github.com/paxlabs-inc/tachyon-tools/pkg/types"
)

// Tester wraps forge test.
type Tester struct {
	ForgePath string
}

// Test runs forge test and parses JSON output.
func (t *Tester) Test(req types.TestRequest) (types.TestResponse, *types.Error) {
	root := strings.TrimSpace(req.ProjectRoot)
	if root == "" {
		return types.TestResponse{}, types.NewError(types.CodeInvalidRequest, "project_root required", false, nil)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return types.TestResponse{}, types.NewError(types.CodeInvalidRequest, err.Error(), false, nil)
	}

	args := []string{"test", "--json"}
	if p := strings.TrimSpace(req.MatchPath); p != "" {
		args = append(args, "--match-path", p)
	}
	if c := strings.TrimSpace(req.MatchContract); c != "" {
		args = append(args, "--match-contract", c)
	}
	if f := strings.TrimSpace(req.Filter); f != "" {
		args = append(args, "--match-test", f)
	}

	stdout, stderr, runErr := forgeutil.RunWithTimeout(t.ForgePath, abs, 30*time.Minute, args...)
	resp, parseErr := parseForgeTestJSON(stdout)
	if parseErr != nil && stdout == "" {
		return types.TestResponse{}, types.NewError(types.CodeTestForgeFailed, forgeutil.FormatForgeError(stdout, stderr, runErr), true, nil)
	}
	if runErr != nil && resp.Failed == 0 && resp.Passed == 0 {
		return types.TestResponse{}, types.NewError(types.CodeTestForgeFailed, forgeutil.FormatForgeError(stdout, stderr, runErr), true, nil)
	}
	if resp.Failed > 0 {
		return resp, types.NewError(types.CodeTestAssertionFailed, "one or more tests failed", false, map[string]int{
			"failed": resp.Failed,
			"passed": resp.Passed,
		})
	}
	return resp, nil
}

type forgeSuite struct {
	Duration    string                       `json:"duration"`
	TestResults map[string]forgeTestResult   `json:"test_results"`
}

type forgeTestResult struct {
	Status   string `json:"status"`
	Reason   any    `json:"reason"`
	Duration string `json:"duration"`
	Kind     struct {
		Fuzz struct {
			MeanGas uint64 `json:"mean_gas"`
		} `json:"Fuzz"`
		Unit struct {
			Gas uint64 `json:"gas"`
		} `json:"Unit"`
	} `json:"kind"`
}

func parseForgeTestJSON(stdout string) (types.TestResponse, error) {
	var resp types.TestResponse
	line := strings.TrimSpace(stdout)
	if line == "" {
		return resp, nil
	}
	var suites map[string]forgeSuite
	if err := json.Unmarshal([]byte(line), &suites); err != nil {
		// NDJSON fallback
		for _, l := range strings.Split(stdout, "\n") {
			l = strings.TrimSpace(l)
			if l == "" {
				continue
			}
			var one map[string]forgeSuite
			if err := json.Unmarshal([]byte(l), &one); err != nil {
				continue
			}
			for k, v := range one {
				suites[k] = v
			}
		}
	}
	if suites == nil {
		return resp, nil
	}
	for file, suite := range suites {
		sr := types.TestSuiteResult{File: file}
		for name, tr := range suite.TestResults {
			status := tr.Status
			gas := tr.Kind.Unit.Gas
			if gas == 0 {
				gas = tr.Kind.Fuzz.MeanGas
			}
			reason := ""
			if tr.Reason != nil {
				reason = fmtAny(tr.Reason)
			}
			cr := types.TestCaseResult{
				Name:     name,
				Status:   status,
				Reason:   reason,
				Gas:      gas,
				Duration: tr.Duration,
			}
			sr.Cases = append(sr.Cases, cr)
			switch status {
			case "Success":
				sr.Passed++
				resp.Passed++
			case "Failure":
				sr.Failed++
				resp.Failed++
			default:
				sr.Skipped++
			}
		}
		resp.Suites = append(resp.Suites, sr)
	}
	return resp, nil
}

func fmtAny(v any) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}
