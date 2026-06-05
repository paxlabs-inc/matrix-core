// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// gideon_ops_policy.go — autonomy guardrails for the Gideon ops agent
// (Gideon Phase 2, plan todo: ops-tools).
//
// Mirrors the structure of forge_policy.go / forge_git_policy.go: a
// pure, table-driven policy object with deterministic, testable
// decision functions. NONE of the two hard guardrails below are ever
// left to the model — they are enforced in code on every tool-call
// step before dispatch (see daemon_gideon_pipeline.go).
//
// Gideon is otherwise AUTONOMOUS: every action that is neither a
// chain-state-loss risk nor a validator-cluster write is allowed and
// executed without a gate. The policy returns exactly one of three
// verdicts per tool call:
//
//	OpsAllow — run autonomously (the common case).
//	OpsGate  — HARD RULE 1: chain-state-loss risk. Force a mandatory
//	           human gate through the existing httpGateHandler. Never
//	           auto-approved; silence/timeout == deny.
//	OpsDeny  — HARD RULE 2: a write/restart/exec/ssh action targeting
//	           the validator-cluster host (id "validator-cluster" or
//	           addr 147.93.139.18). Denied outright (no gate) UNLESS the
//	           intent prose explicitly names that host/IP. Read-only RPC
//	           observation against the validators is always fine.
//
// TOOL-CALL CONTRACT (agreed with the tools worker): every gideon-infra
// MCP tool takes a `host` arg whose value matches a deploy/gideon/
// hosts.json id; ssh_exec carries a `command` arg, service_restart a
// `service` arg. The policy inspects {tool_name, host, command} from
// the walker step's tool invocation.

import (
	"fmt"
	"regexp"
	"strings"
)

// OpsDecision is the closed enum of GideonOpsPolicy verdicts.
type OpsDecision int

const (
	// OpsAllow runs the tool call autonomously.
	OpsAllow OpsDecision = iota
	// OpsGate forces a mandatory human approval gate (chain-state-loss).
	OpsGate
	// OpsDeny blocks the tool call outright (validator hard-deny).
	OpsDeny
)

// String renders the decision for transcript events + error messages.
func (d OpsDecision) String() string {
	switch d {
	case OpsAllow:
		return "allow"
	case OpsGate:
		return "gate"
	case OpsDeny:
		return "deny"
	default:
		return "unknown"
	}
}

// Stable rule identifiers stamped on transcript events + gate prompts.
const (
	GideonRuleChainStateLoss    = "chain_state_loss"
	GideonRuleValidatorHardDeny = "validator_hard_deny"
)

// OpsEvaluation is the full result of evaluating one tool call. Rule +
// Pattern are populated only for the non-Allow verdicts so the gate
// prompt + audit log can explain exactly why the guardrail fired.
type OpsEvaluation struct {
	Decision OpsDecision
	Rule     string // "" | GideonRuleChainStateLoss | GideonRuleValidatorHardDeny
	Reason   string // human-readable explanation for the audit log / gate prompt
	Tool     string // resolved tool name (e.g. "ssh_exec")
	Host     string // the `host` arg value
	Command  string // the `command` arg value (ssh_exec)
	Pattern  string // matched chain-state-loss pattern name (OpsGate only)
}

// namedPattern is a labelled regex so the matched rule is reportable.
type namedPattern struct {
	Name string
	Re   *regexp.Regexp
}

// GideonOpsPolicy encodes the two hard guardrails. Built once
// (DefaultGideonOpsPolicy) and held on daemonState; immutable at
// runtime so it is safe to share across the scheduler + HTTP goroutines.
type GideonOpsPolicy struct {
	// ChainStateLossPatterns is the allowlist of command-string regexes
	// that classify a tool call as a chain-state-loss risk → forced
	// gate. Matched case-insensitively against the ssh_exec `command`.
	ChainStateLossPatterns []namedPattern

	// ValidatorHostIDs are hosts.json ids treated as the untouchable
	// validator cluster. Matched case-insensitively against `host`.
	ValidatorHostIDs []string

	// ValidatorAddrs are raw addresses/IPs that also resolve to the
	// validator cluster (a `host` arg may carry an IP instead of an id).
	ValidatorAddrs []string

	// MutatingTools is the set of tool names that perform a write /
	// restart / exec / ssh action. Only these trigger the validator
	// hard-deny; read-only tools (node_status, metrics_read, log_tail,
	// RPC reads) are always permitted, including against the validators.
	MutatingTools map[string]bool
}

// DefaultGideonOpsPolicy returns the production guardrail set.
//
//	ValidatorHostIDs = [validator-cluster]
//	ValidatorAddrs   = [147.93.139.18]
//	MutatingTools    = {ssh_exec, service_restart, disk_reclaim}
//	ChainStateLossPatterns — see inline (data-dir wipes, rm -rf of
//	  node/home/data paths, docker volume rm, *unsafe-reset* /
//	  tendermint unsafe_reset_all, deleting genesis/priv_validator/
//	  snapshot, force/reset-all).
func DefaultGideonOpsPolicy() *GideonOpsPolicy {
	return &GideonOpsPolicy{
		ValidatorHostIDs: []string{"validator-cluster"},
		ValidatorAddrs:   []string{"147.93.139.18"},
		MutatingTools: map[string]bool{
			"ssh_exec":        true,
			"service_restart": true,
			"disk_reclaim":    true,
		},
		ChainStateLossPatterns: []namedPattern{
			// `rm -rf` (any -r/-f flag ordering) of a node/home/data path.
			{
				Name: "rm_rf_data_path",
				Re: regexp.MustCompile(
					`(?i)\brm\s+-[a-z]*[rf][a-z]*\b[^|;&]*\b(datadir|chaindata|node[-_ ]?data|data|\.tendermint|\.gaia|\.paxeer[a-z]*|home|/root/[^ ]*node)\b`),
			},
			// `rm -rf /` (root or near-root wipe).
			{
				Name: "rm_rf_root",
				Re:   regexp.MustCompile(`(?i)\brm\s+-[a-z]*[rf][a-z]*\s+/(\s|$)`),
			},
			// `docker volume rm` — destroys a node's persisted volume.
			{
				Name: "docker_volume_rm",
				Re:   regexp.MustCompile(`(?i)\bdocker\s+volume\s+(rm|prune)\b`),
			},
			// any *unsafe-reset* / unsafe_reset_all (tendermint/cosmos).
			{
				Name: "unsafe_reset",
				Re:   regexp.MustCompile(`(?i)unsafe[-_]reset(_all)?`),
			},
			// generic reset-all / reset_all subcommand.
			{
				Name: "reset_all",
				Re:   regexp.MustCompile(`(?i)\breset[-_]all\b`),
			},
			// explicit force-reset.
			{
				Name: "force_reset",
				Re:   regexp.MustCompile(`(?i)\bforce[-_]?reset\b`),
			},
			// deleting genesis / priv_validator / snapshot state files.
			{
				Name: "delete_chain_state_file",
				Re: regexp.MustCompile(
					`(?i)\brm\b[^|;&]*\b(genesis\.json|priv_validator(_key|_state)?(\.json)?|snapshots?)\b`),
			},
		},
	}
}

// Evaluate applies the two hard guardrails to one tool call and returns
// the verdict. Order matters: the validator hard-deny is checked first
// (host-based) so a destructive command against the validators that
// does NOT name them in prose is denied outright rather than gated.
//
// Pure: no I/O, no state mutation — safe to call from any goroutine and
// trivially unit-testable.
func (p *GideonOpsPolicy) Evaluate(toolName, host, command, service, prose string) OpsEvaluation {
	ev := OpsEvaluation{
		Decision: OpsAllow,
		Tool:     toolName,
		Host:     host,
		Command:  command,
	}
	if p == nil {
		return ev
	}

	mutating := p.MutatingTools[toolName]

	// HARD RULE 2 — validator hard-deny. A write/restart/exec/ssh action
	// against the validator-cluster host is denied UNLESS the intent
	// prose explicitly names that host/IP. Read-only observation (any
	// non-mutating tool) is always allowed, so it falls through.
	if mutating && p.isValidatorHost(host) {
		if !p.proseNamesValidator(prose) {
			ev.Decision = OpsDeny
			ev.Rule = GideonRuleValidatorHardDeny
			ev.Reason = fmt.Sprintf(
				"validator-cluster host %q is hard-denied for %q (write/restart/exec): "+
					"the intent prose did not explicitly name the validator host/IP",
				host, toolName)
			return ev
		}
		// Prose explicitly named the validators → operator opted in for
		// this intent. Still subject to the chain-state-loss gate below.
	}

	// HARD RULE 1 — chain-state-loss risk → forced human gate. Evaluated
	// against the command string (ssh_exec is the carrier per the
	// tool-call contract). Never auto-approved.
	if name, ok := p.matchChainStateLoss(command); ok {
		ev.Decision = OpsGate
		ev.Rule = GideonRuleChainStateLoss
		ev.Pattern = name
		ev.Reason = fmt.Sprintf(
			"command on host %q matched chain-state-loss pattern %q; "+
				"requires explicit human approval before dispatch",
			host, name)
		return ev
	}

	return ev
}

// matchChainStateLoss reports the first chain-state-loss pattern that
// matches the command, or ("", false) when the command is benign.
func (p *GideonOpsPolicy) matchChainStateLoss(command string) (string, bool) {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return "", false
	}
	for _, np := range p.ChainStateLossPatterns {
		if np.Re.MatchString(cmd) {
			return np.Name, true
		}
	}
	return "", false
}

// isValidatorHost reports whether the `host` arg resolves to the
// untouchable validator cluster (by hosts.json id OR raw addr/IP).
func (p *GideonOpsPolicy) isValidatorHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}
	for _, id := range p.ValidatorHostIDs {
		if h == strings.ToLower(id) {
			return true
		}
	}
	for _, addr := range p.ValidatorAddrs {
		if h == strings.ToLower(addr) {
			return true
		}
	}
	return false
}

// proseNamesValidator reports whether the intent prose explicitly names
// the validator host id or IP — the only override for HARD RULE 2. The
// match is a case-insensitive substring on the exact id/addr so a vague
// mention of "validators" does NOT unlock the guardrail.
func (p *GideonOpsPolicy) proseNamesValidator(prose string) bool {
	lp := strings.ToLower(prose)
	if lp == "" {
		return false
	}
	for _, id := range p.ValidatorHostIDs {
		if strings.Contains(lp, strings.ToLower(id)) {
			return true
		}
	}
	for _, addr := range p.ValidatorAddrs {
		if strings.Contains(lp, strings.ToLower(addr)) {
			return true
		}
	}
	return false
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
