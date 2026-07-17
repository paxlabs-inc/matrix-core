// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

type Provenance string

const (
	User       Provenance = "user"
	Repository Provenance = "repository"
	Framework  Provenance = "framework"
	Runtime    Provenance = "runtime"
	Inferred   Provenance = "inferred"
)

type Clause struct {
	ID         string     `json:"id"`
	Text       string     `json:"text"`
	Provenance Provenance `json:"provenance"`
	Source     string     `json:"source"`
}

type Criterion struct {
	ID        string   `json:"id"`
	Statement string   `json:"statement"`
	Evidence  []string `json:"evidence"`
	Closed    bool     `json:"closed"`
}

type Gate struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	RequiredWhen string `json:"required_when"`
	Satisfied    bool   `json:"satisfied"`
}

type Ambiguity struct {
	ID       string `json:"id"`
	Question string `json:"question"`
	Material bool   `json:"material"`
	Resolved bool   `json:"resolved"`
}

type TaskContract struct {
	ID                   string      `json:"id"`
	Version              uint64      `json:"version"`
	Objective            Clause      `json:"objective"`
	Deliverables         []Clause    `json:"deliverables"`
	Constraints          []Clause    `json:"constraints"`
	NonGoals             []Clause    `json:"non_goals"`
	RequiredCapabilities []string    `json:"required_capabilities"`
	Criteria             []Criterion `json:"acceptance_criteria"`
	Gates                []Gate      `json:"authorization_gates"`
	Ambiguities          []Ambiguity `json:"ambiguities"`
	CompletionPredicate  string      `json:"completion_predicate"`
	Revisions            []Revision  `json:"revisions"`
}

type Revision struct {
	Version    uint64   `json:"version"`
	Steering   Clause   `json:"steering"`
	PriorProof []string `json:"prior_proof"`
}

type CompileInput struct {
	Request         string
	RepositoryRules []string
	FrameworkRules  []string
}

func Compile(in CompileInput) TaskContract {
	request := strings.TrimSpace(in.Request)
	sum := sha256.Sum256([]byte(request))
	required := requiredCapabilities(strings.Join(append(append([]string{request}, in.RepositoryRules...), in.FrameworkRules...), "\n"))
	contract := TaskContract{
		ID:      "o1_" + hex.EncodeToString(sum[:8]),
		Version: 1,
		Objective: Clause{
			ID: "objective.user", Text: request, Provenance: User, Source: "initial_request",
		},
		Deliverables: []Clause{{
			ID: "deliverable.requested_result", Text: request, Provenance: User, Source: "initial_request",
		}},
		Criteria: []Criterion{
			{ID: "criterion.request", Statement: "The requested result is complete and matches the initial request.", Evidence: []string{"authoritative output", "requested semantics verifier"}},
			{ID: "criterion.rules", Statement: "All applicable repository and framework rules are satisfied.", Evidence: []string{"repository checks", "framework validation"}},
			{ID: "criterion.delivery", Statement: "The verified result is delivered to the authorized user surface.", Evidence: []string{"durable delivery record"}},
		},
		Gates: []Gate{
			{ID: "gate.destructive", Kind: "destructive_or_irreversible", RequiredWhen: "an operation is destructive or irreversible"},
			{ID: "gate.external", Kind: "external_effect", RequiredWhen: "an operation changes production, money, secrets, privacy, or an external system"},
		},
		RequiredCapabilities: required,
		CompletionPredicate:  "all acceptance_criteria closed AND all effects reconciled AND no unresolved preventable hazard",
	}
	contract.Criteria = append(contract.Criteria, criteriaForCapabilities(required)...)
	for i, rule := range cleanRules(in.RepositoryRules) {
		contract.Constraints = append(contract.Constraints, Clause{
			ID: "constraint.repository." + itoa(i+1), Text: rule, Provenance: Repository, Source: "repository_rules",
		})
	}
	for i, rule := range cleanRules(in.FrameworkRules) {
		contract.Constraints = append(contract.Constraints, Clause{
			ID: "constraint.framework." + itoa(i+1), Text: rule, Provenance: Framework, Source: "installed_framework",
		})
	}
	if request == "" || strings.Contains(request, "<TBD>") || strings.Contains(request, "{{") {
		contract.Ambiguities = append(contract.Ambiguities, Ambiguity{
			ID: "ambiguity.required_input", Question: "A required request value is missing.", Material: true,
		})
	}
	return contract
}

func criteriaForCapabilities(capabilities []string) []Criterion {
	has := map[string]bool{}
	for _, capability := range capabilities {
		has[capability] = true
	}
	var criteria []Criterion
	if has["filesystem"] {
		criteria = append(criteria, Criterion{
			ID: "criterion.artifact", Statement: "The requested artifact output has verified structure and integrity.",
			Evidence: []string{"successful bounded mutation", "artifact integrity verifier"},
		})
	}
	if has["execution"] {
		criteria = append(criteria, Criterion{
			ID: "criterion.execution", Statement: "The requested build, test, lint, or validation command succeeds.",
			Evidence: []string{"normalized successful process outcome"},
		})
	}
	if has["web_evidence"] {
		criteria = append(criteria, Criterion{
			ID: "criterion.sources", Statement: "Current factual claims use relevant fetched source evidence.",
			Evidence: []string{"successful search discovery", "successful source fetch"},
		})
	}
	if has["memory"] || has["scheduling"] || has["chain_or_wallet"] {
		criteria = append(criteria, Criterion{
			ID: "criterion.effects", Statement: "Every requested state effect is verified and reconciled.",
			Evidence: []string{"typed successful effect", "postcondition reconciliation"},
		})
	}
	return criteria
}

func requiredCapabilities(request string) []string {
	lower := strings.ToLower(request)
	groups := []struct {
		name  string
		terms []string
	}{
		{"filesystem", []string{"file", "code", "repo", "project", "document", "artifact", "wire", "bridge", "implement", "build", "fix", "write", ".go", ".ts", ".tsx", ".js", ".html", ".css"}},
		{"web_evidence", []string{"web", "search", "fetch", "news", "source", "current", "latest", "data", "endpoint"}},
		{"browser", []string{"browser", "page", "website", "click", "form"}},
		{"media", []string{"image", "video", "audio", "speech", "transcribe"}},
		{"memory", []string{"memory", "remember", "recall", "forget", "personalization"}},
		{"scheduling", []string{"schedule", "alarm", "remind", "calendar"}},
		{"chain_or_wallet", []string{"chain", "token", "wallet", "transfer", "pay", "deposit", "swap"}},
		{"workspace_preview", []string{"preview", "application", "app", "frontend", "ui"}},
		{"execution", []string{"run tests", "run the test", "write a test", "test that", "build", "compile", "command", "shell", "execute", "verify", "prove", "lint", "vet"}},
	}
	var out []string
	for _, group := range groups {
		for _, term := range group.terms {
			if requestContainsTerm(lower, term) {
				out = append(out, group.name)
				break
			}
		}
	}
	hasFilesystem, hasExecution := false, false
	for _, capability := range out {
		hasFilesystem = hasFilesystem || capability == "filesystem"
		hasExecution = hasExecution || capability == "execution"
	}
	if hasFilesystem && !hasExecution {
		out = append(out, "execution")
	}
	return out
}

func requestContainsTerm(request, term string) bool {
	if strings.HasPrefix(term, ".") {
		return strings.Contains(request, term)
	}
	if strings.Contains(term, " ") {
		return strings.Contains(request, term)
	}
	isWord := func(r byte) bool {
		return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_'
	}
	for start := 0; ; {
		index := strings.Index(request[start:], term)
		if index < 0 {
			return false
		}
		index += start
		beforeOK := index == 0 || !isWord(request[index-1])
		after := index + len(term)
		afterOK := after == len(request) || !isWord(request[after])
		if beforeOK && afterOK {
			return true
		}
		start = index + 1
	}
}

func (c TaskContract) ReadyForMutation() bool {
	if strings.TrimSpace(c.Objective.Text) == "" || len(c.Criteria) == 0 {
		return false
	}
	for _, ambiguity := range c.Ambiguities {
		if ambiguity.Material && !ambiguity.Resolved {
			return false
		}
	}
	return true
}

func (c TaskContract) Revise(steering string) TaskContract {
	steering = strings.TrimSpace(steering)
	if steering == "" {
		return c
	}
	c.Version++
	var proof []string
	for _, criterion := range c.Criteria {
		if criterion.Closed {
			proof = append(proof, criterion.ID)
		}
	}
	c.Revisions = append(c.Revisions, Revision{
		Version:    c.Version,
		Steering:   Clause{ID: "steering." + itoa(int(c.Version)), Text: steering, Provenance: User, Source: "inbox_steering"},
		PriorProof: proof,
	})
	c.Constraints = append(c.Constraints, Clause{
		ID: "constraint.steering." + itoa(int(c.Version)), Text: steering, Provenance: User, Source: "inbox_steering",
	})
	return c
}

func (c TaskContract) Render() string {
	b, _ := json.Marshal(c)
	return "\n\n<o1_task_contract>\n" + string(b) + "\n</o1_task_contract>"
}

func cleanRules(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, rule := range in {
		rule = strings.TrimSpace(rule)
		if rule != "" && !seen[rule] {
			seen[rule] = true
			out = append(out, rule)
		}
	}
	sort.Strings(out)
	return out
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
