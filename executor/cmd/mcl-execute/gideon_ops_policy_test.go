// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import "testing"

// TestGideonChainStateLossGuardrail exercises HARD RULE 1: chain-state-
// loss risk → forced gate. Destructive command strings must gate;
// benign ops must run autonomously.
func TestGideonChainStateLossGuardrail(t *testing.T) {
	p := DefaultGideonOpsPolicy()

	gateCases := []struct {
		name    string
		command string
	}{
		{"rm_rf_data_dir", "rm -rf /root/paxeer/data"},
		{"rm_rf_datadir_flag", "sudo rm -rf /var/lib/paxeerd/datadir"},
		{"rm_rf_node_data", "rm -fr ~/node-data"},
		{"rm_rf_root", "rm -rf /"},
		{"docker_volume_rm", "docker volume rm paxeer_chaindata"},
		{"docker_volume_prune", "docker volume prune -f"},
		{"unsafe_reset_all", "paxeerd tendermint unsafe-reset-all"},
		{"unsafe_reset_underscore", "paxeerd unsafe_reset_all --home /root/.paxeer"},
		{"reset_all", "paxeerd comet reset-all"},
		{"force_reset", "paxeerd force-reset"},
		{"delete_genesis", "rm /root/.paxeer/config/genesis.json"},
		{"delete_priv_validator", "rm -f /root/.paxeer/config/priv_validator_key.json"},
		{"delete_snapshots", "rm -rf /root/.paxeer/snapshots"},
	}
	for _, tc := range gateCases {
		t.Run("gate/"+tc.name, func(t *testing.T) {
			ev := p.Evaluate("ssh_exec", "full-node-01", tc.command, "", "sweep the fleet")
			if ev.Decision != OpsGate {
				t.Fatalf("command %q: got decision %s, want gate", tc.command, ev.Decision)
			}
			if ev.Rule != GideonRuleChainStateLoss {
				t.Fatalf("command %q: got rule %q, want %q", tc.command, ev.Rule, GideonRuleChainStateLoss)
			}
			if ev.Pattern == "" {
				t.Fatalf("command %q: expected a matched pattern name", tc.command)
			}
		})
	}

	allowCases := []struct {
		name    string
		command string
	}{
		{"disk_usage", "df -h"},
		{"list_dir", "ls -la /root/.paxeer"},
		{"tail_log", "tail -n 100 /var/log/paxeerd.log"},
		{"systemctl_status", "systemctl status paxeerd"},
		{"remove_logfile", "rm /tmp/old.log"},
		{"restart_service", "systemctl restart paxeerd"},
	}
	for _, tc := range allowCases {
		t.Run("allow/"+tc.name, func(t *testing.T) {
			ev := p.Evaluate("ssh_exec", "full-node-01", tc.command, "", "sweep the fleet")
			if ev.Decision != OpsAllow {
				t.Fatalf("command %q: got decision %s (rule=%s, pattern=%s), want allow",
					tc.command, ev.Decision, ev.Rule, ev.Pattern)
			}
		})
	}
}

// TestGideonValidatorHardDenyGuardrail exercises HARD RULE 2: the
// validator-cluster host is untouchable for write/restart/exec unless
// the prose explicitly names it. Read-only observation is always fine.
func TestGideonValidatorHardDenyGuardrail(t *testing.T) {
	p := DefaultGideonOpsPolicy()

	// Mutating action against validator id, prose does NOT name it → deny.
	t.Run("deny/ssh_by_id", func(t *testing.T) {
		ev := p.Evaluate("ssh_exec", "validator-cluster", "uptime", "", "check the fleet health")
		if ev.Decision != OpsDeny {
			t.Fatalf("got %s, want deny", ev.Decision)
		}
		if ev.Rule != GideonRuleValidatorHardDeny {
			t.Fatalf("got rule %q, want %q", ev.Rule, GideonRuleValidatorHardDeny)
		}
	})

	// Mutating action against validator IP, prose does NOT name it → deny.
	t.Run("deny/restart_by_ip", func(t *testing.T) {
		ev := p.Evaluate("service_restart", "147.93.139.18", "", "paxeerd", "restart unhealthy nodes")
		if ev.Decision != OpsDeny {
			t.Fatalf("got %s, want deny", ev.Decision)
		}
	})

	// Read-only observation against the validators is always allowed.
	t.Run("allow/node_status_readonly", func(t *testing.T) {
		ev := p.Evaluate("node_status", "validator-cluster", "", "", "check the fleet health")
		if ev.Decision != OpsAllow {
			t.Fatalf("got %s, want allow (read-only observation)", ev.Decision)
		}
	})
	t.Run("allow/metrics_read_readonly", func(t *testing.T) {
		ev := p.Evaluate("metrics_read", "147.93.139.18", "", "", "sweep the fleet")
		if ev.Decision != OpsAllow {
			t.Fatalf("got %s, want allow (read-only observation)", ev.Decision)
		}
	})

	// Prose explicitly names the validator-cluster id → opt-in; a benign
	// command is then allowed.
	t.Run("allow/prose_names_id", func(t *testing.T) {
		ev := p.Evaluate("ssh_exec", "validator-cluster", "uptime", "",
			"ssh into validator-cluster and report uptime")
		if ev.Decision != OpsAllow {
			t.Fatalf("got %s, want allow (prose named the host)", ev.Decision)
		}
	})

	// Prose names the validator IP → opt-in.
	t.Run("allow/prose_names_ip", func(t *testing.T) {
		ev := p.Evaluate("service_restart", "147.93.139.18", "", "paxeerd",
			"restart paxeerd on 147.93.139.18 per Andrew")
		if ev.Decision != OpsAllow {
			t.Fatalf("got %s, want allow (prose named the IP)", ev.Decision)
		}
	})

	// Even when prose names the validators, a chain-state-loss command
	// still escalates to a gate (RULE 1 layered on top of the opt-in).
	t.Run("gate/named_but_destructive", func(t *testing.T) {
		ev := p.Evaluate("ssh_exec", "validator-cluster", "rm -rf /root/.paxeer/data", "",
			"on validator-cluster, reclaim space")
		if ev.Decision != OpsGate {
			t.Fatalf("got %s, want gate (destructive even when named)", ev.Decision)
		}
		if ev.Rule != GideonRuleChainStateLoss {
			t.Fatalf("got rule %q, want %q", ev.Rule, GideonRuleChainStateLoss)
		}
	})

	// Deny takes precedence over gate when prose does NOT name the host,
	// even with a destructive command (blocked before any approval path).
	t.Run("deny/destructive_not_named", func(t *testing.T) {
		ev := p.Evaluate("ssh_exec", "validator-cluster", "rm -rf /root/.paxeer/data", "",
			"wipe everything")
		if ev.Decision != OpsDeny {
			t.Fatalf("got %s, want deny (validator not named in prose)", ev.Decision)
		}
	})

	// A non-validator host with a mutating tool + benign command runs
	// autonomously (the autonomy default).
	t.Run("allow/non_validator_autonomous", func(t *testing.T) {
		ev := p.Evaluate("ssh_exec", "full-node-07", "systemctl restart watchdog", "",
			"restart the watchdog sidecar")
		if ev.Decision != OpsAllow {
			t.Fatalf("got %s, want allow", ev.Decision)
		}
	})
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
