use std::collections::{BTreeMap, BTreeSet};
use std::env;
use std::fs;
use std::path::{Path, PathBuf};
use std::process::Command;

use keith_release::{
    MANIFEST_FILE, decode_public_key, verify_packaged_build_reports, verify_release,
};
use serde::Deserialize;
use sha2::{Digest, Sha256};

const REQUIRED_ATTACKS: &[&str] = &[
    "archive_bomb",
    "attachment_handling",
    "authentication",
    "awareness_instruction",
    "browser_storage",
    "command_injection",
    "credential_exfiltration",
    "cross_channel",
    "cross_profile",
    "cross_session",
    "csrf",
    "deletion_isolation",
    "delivery_isolation",
    "destructive_action",
    "device_path",
    "dns_change",
    "duplicate_event",
    "environment_injection",
    "export_disclosure",
    "forged_route",
    "kernel_isolation",
    "log_disclosure",
    "malicious_markdown",
    "malicious_media",
    "malicious_repository_content",
    "malicious_web_content",
    "mcp_isolation",
    "origin",
    "output_flood",
    "packaged_daemon",
    "packaged_desktop",
    "path_traversal",
    "payload_bound",
    "plugin_isolation",
    "protected_path",
    "rate_limit",
    "real_process_boundary",
    "redirect",
    "refinement_instruction",
    "schedule_isolation",
    "self_evolution_authority_widening",
    "self_evolution_candidate_tamper",
    "self_evolution_crash_recovery",
    "self_evolution_ledger_tamper",
    "self_evolution_private_data",
    "self_evolution_protected_path",
    "self_evolution_absolute_path",
    "self_evolution_build_script",
    "self_evolution_credential_access",
    "self_evolution_device_path",
    "self_evolution_filesystem_escape",
    "self_evolution_generated_output",
    "self_evolution_network_escape",
    "self_evolution_output_limit",
    "self_evolution_process_escape",
    "self_evolution_proc_macro",
    "self_evolution_prompt_injection",
    "self_evolution_rename_escape",
    "self_evolution_resource_limit",
    "self_evolution_symlink_escape",
    "self_evolution_toolchain_override",
    "self_evolution_unsigned_worker",
    "self_evolution_workspace_manifest",
    "ssrf",
    "stale_lease",
    "symlink_race",
    "terminal_escape",
    "unauthenticated_access",
];

const PACKAGED_BINARIES: &[&str] = &[
    "agent-cli",
    "agent-desktop",
    "agent-tui",
    "agent-web",
    "agent-worker",
    "agentd",
    "browser-runner",
    "channel-gateway",
    "kernel-runner",
    "tool-runner",
];

const RELEASE_BLOCKING_CLASSES: &[&str] = &[
    "credential_exfiltration",
    "self_evolution_candidate_tamper",
    "self_evolution_credential_access",
    "self_evolution_filesystem_escape",
    "self_evolution_network_escape",
    "self_evolution_process_escape",
    "self_evolution_protected_path",
    "self_evolution_unsigned_worker",
    "unreversible_state",
];

struct Probe {
    package: &'static str,
    test: &'static str,
    attacks: &'static [&'static str],
}

const PROBES: &[Probe] = &[
    Probe {
        package: "keith-tool-runner-core",
        test: "tests::traversal_device_paths_size_and_cancellation_are_rejected",
        attacks: &["path_traversal", "device_path", "payload_bound"],
    },
    Probe {
        package: "keith-tool-runner-core",
        test: "tests::symlinks_and_symlink_swap_races_cannot_escape_the_capability_root",
        attacks: &["symlink_race"],
    },
    Probe {
        package: "keith-tool-runner-core",
        test: "tests::argv_is_not_reparsed_and_environment_is_minimal",
        attacks: &["command_injection", "environment_injection"],
    },
    Probe {
        package: "keith-tool-runner-core",
        test: "tests::output_flood_and_timeout_kill_the_process_tree",
        attacks: &["output_flood", "real_process_boundary"],
    },
    Probe {
        package: "keith-web",
        test: "fetch::tests::rejects_ssrf_destinations_and_non_http_schemes",
        attacks: &["ssrf"],
    },
    Probe {
        package: "keith-web",
        test: "fetch::tests::repeated_validation_catches_dns_rebinding",
        attacks: &["dns_change"],
    },
    Probe {
        package: "keith-web",
        test: "fetch::tests::redirect_targets_are_revalidated_before_connection",
        attacks: &["redirect"],
    },
    Probe {
        package: "keith-web",
        test: "browser::tests::hostile_markup_instructions_and_popups_are_neutralized",
        attacks: &["malicious_web_content", "malicious_markdown"],
    },
    Probe {
        package: "keith-web",
        test: "browser::tests::profiles_cannot_read_or_mutate_each_others_private_state",
        attacks: &["browser_storage"],
    },
    Probe {
        package: "keith-data-control",
        test: "tests::compressed_archive_bombs_are_rejected_without_expansion",
        attacks: &["archive_bomb"],
    },
    Probe {
        package: "keith-data-control",
        test: "tests::complete_lifecycle_exports_restores_deletes_rebuilds_and_isolates",
        attacks: &["deletion_isolation"],
    },
    Probe {
        package: "keith-credentials",
        test: "tests::seeded_leak_suite_scans_persistence_browser_process_export_event_log_and_diagnostics",
        attacks: &[
            "credential_exfiltration",
            "export_disclosure",
            "log_disclosure",
        ],
    },
    Probe {
        package: "keith-plugin-host",
        test: "tests::malicious_import_timeout_memory_and_crash_are_isolated_and_quarantined",
        attacks: &["plugin_isolation"],
    },
    Probe {
        package: "keith-mcp",
        test: "tests::timeout_and_malicious_output_are_bounded_and_processes_are_reaped",
        attacks: &["mcp_isolation"],
    },
    Probe {
        package: "keith-kernel-broker",
        test: "tests::isolation_resource_profiles_and_idle_reclamation_fail_closed",
        attacks: &["kernel_isolation"],
    },
    Probe {
        package: "keith-artifacts",
        test: "tests::cross_tree_and_cross_profile_access_are_denied_before_content_lookup",
        attacks: &["cross_profile"],
    },
    Probe {
        package: "keith-artifacts",
        test: "tests::spill_has_bounded_preview_media_detection_and_oversize_rejection",
        attacks: &["malicious_media"],
    },
    Probe {
        package: "keith-routing",
        test: "tests::channel_session_policies_and_unavailable_profiles_are_isolated",
        attacks: &["cross_channel", "cross_session"],
    },
    Probe {
        package: "keith-routing",
        test: "tests::channel_routes_are_durable_deterministic_and_fail_closed",
        attacks: &["forged_route"],
    },
    Probe {
        package: "keith-worker-runtime",
        test: "tests::simultaneous_claims_have_one_winner_and_expiry_advances_generation",
        attacks: &["stale_lease"],
    },
    Probe {
        package: "keith-supervisor",
        test: "renewal_loss_stops_stale_worker_and_forced_replacement_advances_generation",
        attacks: &["stale_lease", "real_process_boundary"],
    },
    Probe {
        package: "keith-daemon-core",
        test: "events::tests::acknowledgements_detach_and_command_deduplication_are_explicit",
        attacks: &["duplicate_event"],
    },
    Probe {
        package: "keith-scheduler",
        test: "tests::duplicate_claim_race_enqueues_one_action",
        attacks: &["schedule_isolation"],
    },
    Probe {
        package: "keith-delivery",
        test: "tests::acknowledgement_crash_recovers_with_honest_duplicate_state_and_receipt",
        attacks: &["delivery_isolation"],
    },
    Probe {
        package: "keith-channel-adapters",
        test: "tests::discord_gateway_inbound_dedup_attachment_isolation_and_resume_are_real",
        attacks: &["attachment_handling"],
    },
    Probe {
        package: "keith-agent-web",
        test: "security::tests::authentication_origin_csrf_and_rate_are_independent_gates",
        attacks: &[
            "authentication",
            "csrf",
            "origin",
            "rate_limit",
            "unauthenticated_access",
        ],
    },
    Probe {
        package: "keith-agent-tui",
        test: "tests::terminal_control_sequences_are_neutralized_before_rendering",
        attacks: &["terminal_escape"],
    },
    Probe {
        package: "keith-awareness",
        test: "tests::hostile_repository_instructions_remain_bounded_observed_data",
        attacks: &["awareness_instruction", "malicious_repository_content"],
    },
    Probe {
        package: "keith-evolution",
        test: "refinement::tests::reviewer_is_read_only_and_confirmed_diff_is_durable_and_undoable",
        attacks: &["refinement_instruction"],
    },
    Probe {
        package: "keith-evolution",
        test: "refinement::tests::malformed_protected_validation_no_change_and_concurrent_edits_fail_closed",
        attacks: &["protected_path"],
    },
    Probe {
        package: "keith-web",
        test: "browser::tests::every_consequential_action_requires_confirmation",
        attacks: &["destructive_action"],
    },
    Probe {
        package: "keith-agentd",
        test: "daemon_process_is_lazy_contains_crashes_and_adopts_after_restart",
        attacks: &["packaged_daemon"],
    },
    Probe {
        package: "keith-agent-desktop",
        test: "startup_existing_daemon_crash_report_restart_and_graceful_stop_use_real_processes",
        attacks: &["packaged_desktop"],
    },
    Probe {
        package: "keith-self-evolution",
        test: "build::tests::unsigned_wrongly_signed_and_tampered_worker_images_are_rejected",
        attacks: &["self_evolution_candidate_tamper"],
    },
    Probe {
        package: "keith-self-evolution",
        test: "ledger::tests::tampering_quarantines_on_every_reopen",
        attacks: &["self_evolution_ledger_tamper"],
    },
    Probe {
        package: "keith-self-evolution",
        test: "ledger::tests::gate_output_is_redacted_bounded_and_private_categories_are_rejected",
        attacks: &["self_evolution_private_data"],
    },
    Probe {
        package: "keith-self-evolution",
        test: "guard::tests::guard_refuses_protected_artifacts_and_rename_targets",
        attacks: &["self_evolution_protected_path"],
    },
    Probe {
        package: "keith-self-evolution",
        test: "budget::tests::recursion_and_every_authority_widening_capability_are_refused_before_mutation",
        attacks: &["self_evolution_authority_widening"],
    },
    Probe {
        package: "keith-self-evolution",
        test: "promotion_crash_boundary_matrix_recovers_to_old_or_fully_committed_state",
        attacks: &["self_evolution_crash_recovery"],
    },
    Probe {
        package: "keith-self-evolution",
        test: "traversal_absolute_device_and_symlink_escape_attacks_are_rejected",
        attacks: &[
            "self_evolution_absolute_path",
            "self_evolution_device_path",
            "self_evolution_symlink_escape",
        ],
    },
    Probe {
        package: "keith-self-evolution",
        test: "rename_cannot_cross_into_protected_build_workspace_or_toolchain_surfaces",
        attacks: &["self_evolution_rename_escape"],
    },
    Probe {
        package: "keith-self-evolution",
        test: "build_scripts_proc_macro_manifests_generated_output_and_toolchain_are_fail_closed",
        attacks: &[
            "self_evolution_build_script",
            "self_evolution_generated_output",
            "self_evolution_proc_macro",
            "self_evolution_toolchain_override",
            "self_evolution_workspace_manifest",
        ],
    },
    Probe {
        package: "keith-self-evolution",
        test: "prompt_injection_cannot_obtain_shell_network_filesystem_or_credentials",
        attacks: &[
            "self_evolution_credential_access",
            "self_evolution_filesystem_escape",
            "self_evolution_network_escape",
            "self_evolution_process_escape",
            "self_evolution_prompt_injection",
        ],
    },
    Probe {
        package: "keith-self-evolution",
        test: "sandbox_configuration_refuses_missing_network_cpu_memory_output_or_wall_limits",
        attacks: &[
            "self_evolution_output_limit",
            "self_evolution_resource_limit",
        ],
    },
    Probe {
        package: "keith-self-evolution",
        test: "unsigned_wrong_signer_and_tampered_worker_images_are_rejected_at_decode",
        attacks: &["self_evolution_unsigned_worker"],
    },
];

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct FindingLedger {
    schema_version: u16,
    findings: Vec<Finding>,
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct Finding {
    id: String,
    severity: Severity,
    status: FindingStatus,
    class: String,
    summary: String,
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq)]
#[serde(rename_all = "snake_case")]
enum Severity {
    Low,
    Medium,
    High,
    Critical,
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq)]
#[serde(rename_all = "snake_case")]
enum FindingStatus {
    Open,
    Resolved,
}

pub fn run(root: &Path) -> Result<(), String> {
    validate_corpus()?;
    validate_findings(
        &fs::read(root.join("security/findings.json"))
            .map_err(|error| format!("security finding ledger is unavailable: {error}"))?,
    )?;
    let release = required_path("KEITH_SECURITY_RELEASE_PATH")?;
    let trusted_key = required_text("KEITH_SECURITY_TRUSTED_PUBLIC_KEY")?;
    verify_packaged_binaries(&release, &trusted_key)?;

    let mut packages = BTreeMap::<&str, Vec<&str>>::new();
    for probe in PROBES {
        packages.entry(probe.package).or_default().push(probe.test);
    }
    for (package, expected_tests) in packages {
        let listed = listed_tests(root, package)?;
        for expected in expected_tests {
            if !listed.contains(expected) {
                return Err(format!(
                    "security probe {package}::{expected} is missing from the release test binary"
                ));
            }
        }
        run_command(
            root,
            "cargo",
            &["test", "-p", package, "--release", "--locked"],
        )?;
    }
    let teammates_report = TeammatesSecurityGate::from_verified_package(&release)?.run()?;
    println!(
        "security gate passed: {} attacks, {} packaged binaries, {} real test probes, {} real teammates fault scenarios",
        REQUIRED_ATTACKS.len(),
        PACKAGED_BINARIES.len(),
        PROBES.len(),
        teammates_report.results.len()
    );
    Ok(())
}

fn validate_corpus() -> Result<(), String> {
    let required = REQUIRED_ATTACKS.iter().copied().collect::<BTreeSet<_>>();
    let covered = PROBES
        .iter()
        .flat_map(|probe| probe.attacks.iter().copied())
        .collect::<BTreeSet<_>>();
    if required != covered {
        let missing = required.difference(&covered).copied().collect::<Vec<_>>();
        let unexpected = covered.difference(&required).copied().collect::<Vec<_>>();
        return Err(format!(
            "security corpus mismatch; missing {missing:?}, unexpected {unexpected:?}"
        ));
    }
    let unique = PROBES
        .iter()
        .map(|probe| (probe.package, probe.test))
        .collect::<BTreeSet<_>>();
    if unique.len() != PROBES.len() {
        return Err("security corpus contains a duplicate test probe".into());
    }
    Ok(())
}

fn validate_findings(bytes: &[u8]) -> Result<(), String> {
    let ledger: FindingLedger = serde_json::from_slice(bytes)
        .map_err(|error| format!("security finding ledger is invalid: {error}"))?;
    if ledger.schema_version != 1 {
        return Err(format!(
            "security finding ledger schema {} is unsupported",
            ledger.schema_version
        ));
    }
    let mut identifiers = BTreeSet::new();
    for finding in &ledger.findings {
        if finding.id.trim().is_empty()
            || finding.class.trim().is_empty()
            || finding.summary.trim().is_empty()
            || !identifiers.insert(finding.id.as_str())
        {
            return Err("security finding ledger contains an invalid finding".into());
        }
        if finding.status == FindingStatus::Open
            && (matches!(finding.severity, Severity::High | Severity::Critical)
                || RELEASE_BLOCKING_CLASSES.contains(&finding.class.as_str()))
        {
            return Err(format!(
                "release blocked by open {:?} security finding {} ({})",
                finding.severity, finding.id, finding.class
            ));
        }
    }
    Ok(())
}

fn listed_tests(root: &Path, package: &str) -> Result<BTreeSet<String>, String> {
    let output = Command::new("cargo")
        .args([
            "test",
            "-q",
            "-p",
            package,
            "--release",
            "--locked",
            "--",
            "--list",
        ])
        .current_dir(root)
        .output()
        .map_err(|error| format!("failed to enumerate {package} security probes: {error}"))?;
    if !output.status.success() {
        return Err(format!(
            "failed to enumerate {package} security probes: {}",
            String::from_utf8_lossy(&output.stderr)
        ));
    }
    Ok(String::from_utf8_lossy(&output.stdout)
        .lines()
        .filter_map(|line| line.strip_suffix(": test"))
        .map(str::to_owned)
        .collect())
}

fn verify_packaged_binaries(release: &Path, trusted_key: &str) -> Result<(), String> {
    if !release.is_absolute() {
        return Err("KEITH_SECURITY_RELEASE_PATH must be absolute".into());
    }
    let trusted_key = decode_public_key(trusted_key).map_err(|error| error.to_string())?;
    let verified = verify_release(release, &trusted_key).map_err(|error| error.to_string())?;
    let host_target = format!("{}-{}", env::consts::ARCH, env::consts::OS);
    if verified.manifest.target != host_target {
        return Err(format!(
            "signed release target {} does not match this host {host_target}",
            verified.manifest.target
        ));
    }
    if verified.manifest.build_id.trim().is_empty()
        || verified.manifest.build_id.ends_with("+development")
    {
        return Err("security gate requires a freshly assembled non-development release".into());
    }
    for binary in PACKAGED_BINARIES {
        let path = release
            .join("bin")
            .join(format!("{binary}{}", env::consts::EXE_SUFFIX));
        if !path.is_file() {
            return Err(format!(
                "signed release binary is missing: {}",
                path.display()
            ));
        }
    }
    verify_packaged_build_reports(release, &verified.manifest)
        .map_err(|error| error.to_string())?;
    Ok(())
}

fn required_path(name: &str) -> Result<PathBuf, String> {
    env::var_os(name)
        .filter(|value| !value.is_empty())
        .map(PathBuf::from)
        .ok_or_else(|| format!("{name} must name a freshly assembled signed release directory"))
}

fn required_text(name: &str) -> Result<String, String> {
    env::var(name)
        .ok()
        .filter(|value| !value.trim().is_empty())
        .ok_or_else(|| format!("{name} must contain an independently supplied trusted public key"))
}

fn run_command(root: &Path, program: &str, args: &[&str]) -> Result<(), String> {
    let status = Command::new(program)
        .args(args)
        .current_dir(root)
        .status()
        .map_err(|error| format!("failed to run {program}: {error}"))?;
    if status.success() {
        Ok(())
    } else {
        Err(format!("{program} {} failed with {status}", args.join(" ")))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn corpus_is_complete_and_has_stable_unique_probes() {
        validate_corpus().unwrap();
    }

    #[test]
    fn serious_open_findings_block_but_resolved_findings_do_not() {
        let open = br#"{
            "schema_version": 1,
            "findings": [{
                "id": "SEC-1",
                "severity": "high",
                "status": "open",
                "class": "cross_scope",
                "summary": "cross-profile read"
            }]
        }"#;
        assert!(validate_findings(open).is_err());

        let resolved = open
            .windows(b"\"open\"".len())
            .position(|window| window == b"\"open\"")
            .map(|offset| {
                let mut bytes = open.to_vec();
                bytes.splice(
                    offset..offset + b"\"open\"".len(),
                    b"\"resolved\"".iter().copied(),
                );
                bytes
            })
            .unwrap();
        validate_findings(&resolved).unwrap();
    }

    #[test]
    fn protected_release_findings_block_at_every_severity() {
        for class in RELEASE_BLOCKING_CLASSES {
            let ledger = format!(
                r#"{{
                    "schema_version": 1,
                    "findings": [{{
                        "id": "SEC-PROTECTED",
                        "severity": "low",
                        "status": "open",
                        "class": "{class}",
                        "summary": "release invariant failure"
                    }}]
                }}"#
            );
            assert!(validate_findings(ledger.as_bytes()).is_err(), "{class}");
        }
    }
}
#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TeammatesKillPoint {
    AfterEnqueue,
    AfterClaim,
    AfterExpiredClaim,
    AfterFinalizeBeforeAck,
    AfterPublicationIntent,
    AfterPublicationClaimBeforeAppend,
    AfterAppendBeforePublished,
    AfterAssignmentClaim,
    AfterOwnershipTransfer,
    AfterRoundTrigger,
    MidRoundBranch,
    BeforeMigrationWrite,
    AfterMigrationWriteBeforeCommit,
    BrowserTaskActive,
    DisplayActive,
    StreamActive,
    TakeoverActive,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TeammatesAttack {
    None,
    ForgedAuthorship,
    ForgedWebSocketSubject,
    StaleRevision,
    StaleLeaseToken,
    DeliveryReplay,
    AttachmentProvenance,
    PromptInjection,
    CrossProfileMemory,
    CrossProfileSearch,
    RevokedGrant,
    SecretExfiltration,
    ToolEscalation,
    CredentialEscalation,
    BrowserCrossProfileControl,
    SelfEvolutionEscalation,
    ConversationBudget,
    DeliveryBudget,
    BrowserBudget,
    ProcessBudget,
    DiskBudget,
    CpuBudget,
    MemoryBudget,
    NetworkBudget,
    ModelBudget,
    CostBudget,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammatesFaultCase {
    pub id: String,
    pub kill_point: Option<TeammatesKillPoint>,
    pub attack: TeammatesAttack,
    pub required_processes: std::collections::BTreeSet<String>,
    pub minimum_concurrency: usize,
    pub required_invariants: std::collections::BTreeSet<String>,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammatesFaultMatrix {
    pub version: u32,
    pub cases: Vec<TeammatesFaultCase>,
}

impl TeammatesFaultMatrix {
    #[allow(clippy::too_many_lines)]
    pub fn release_gate() -> Self {
        let coordination = [
            ("delivery-after-enqueue", TeammatesKillPoint::AfterEnqueue),
            ("delivery-after-claim", TeammatesKillPoint::AfterClaim),
            (
                "delivery-after-expired-claim",
                TeammatesKillPoint::AfterExpiredClaim,
            ),
            (
                "delivery-after-finalize-before-ack",
                TeammatesKillPoint::AfterFinalizeBeforeAck,
            ),
            (
                "publication-after-intent",
                TeammatesKillPoint::AfterPublicationIntent,
            ),
            (
                "publication-after-claim-before-append",
                TeammatesKillPoint::AfterPublicationClaimBeforeAppend,
            ),
            (
                "publication-after-append-before-published",
                TeammatesKillPoint::AfterAppendBeforePublished,
            ),
            (
                "assignment-after-claim",
                TeammatesKillPoint::AfterAssignmentClaim,
            ),
            (
                "assignment-after-transfer",
                TeammatesKillPoint::AfterOwnershipTransfer,
            ),
            ("round-after-trigger", TeammatesKillPoint::AfterRoundTrigger),
            ("round-mid-branch", TeammatesKillPoint::MidRoundBranch),
        ];
        let mut cases = coordination
            .into_iter()
            .map(|(id, kill_point)| TeammatesFaultCase {
                id: id.into(),
                kill_point: Some(kill_point),
                attack: TeammatesAttack::None,
                required_processes: process_set(&["agentd", "agent-worker"]),
                minimum_concurrency: 5,
                required_invariants: invariant_set(&[
                    "stable_key_exact_or_conflict",
                    "monotonic_revision_and_fence",
                    "stale_claim_rejected",
                    "exactly_one_visible_publication",
                    "terminal_work_not_reclaimed",
                    "atomic_handoff_bundle",
                    "bounded_fair_scheduling",
                    "dead_letter_clears_claim",
                ]),
            })
            .collect::<Vec<_>>();
        for (id, kill_point, processes) in [
            (
                "migration-before-write",
                TeammatesKillPoint::BeforeMigrationWrite,
                &[][..],
            ),
            (
                "migration-after-write-before-commit",
                TeammatesKillPoint::AfterMigrationWriteBeforeCommit,
                &[][..],
            ),
            (
                "computer-browser-crash",
                TeammatesKillPoint::BrowserTaskActive,
                &["agentd", "browser-runner", "chromium", "xvfb"][..],
            ),
            (
                "computer-display-crash",
                TeammatesKillPoint::DisplayActive,
                &["agentd", "browser-runner", "chromium", "xvfb"][..],
            ),
            (
                "computer-stream-crash",
                TeammatesKillPoint::StreamActive,
                &["agentd", "agent-web", "browser-runner", "chromium", "xvfb"][..],
            ),
            (
                "computer-takeover-crash",
                TeammatesKillPoint::TakeoverActive,
                &["agentd", "agent-web", "browser-runner", "chromium", "xvfb"][..],
            ),
        ] {
            cases.push(TeammatesFaultCase {
                id: id.into(),
                kill_point: Some(kill_point),
                attack: TeammatesAttack::None,
                required_processes: process_set(processes),
                minimum_concurrency: 5,
                required_invariants: invariant_set(&[
                    "fresh_root",
                    "restart_reconciled",
                    "cross_profile_denied",
                    "leases_fenced",
                    "no_secret_in_evidence",
                ]),
            });
        }
        for attack in [
            TeammatesAttack::ForgedAuthorship,
            TeammatesAttack::ForgedWebSocketSubject,
            TeammatesAttack::StaleRevision,
            TeammatesAttack::StaleLeaseToken,
            TeammatesAttack::DeliveryReplay,
            TeammatesAttack::AttachmentProvenance,
            TeammatesAttack::PromptInjection,
            TeammatesAttack::CrossProfileMemory,
            TeammatesAttack::CrossProfileSearch,
            TeammatesAttack::RevokedGrant,
            TeammatesAttack::SecretExfiltration,
            TeammatesAttack::ToolEscalation,
            TeammatesAttack::CredentialEscalation,
            TeammatesAttack::BrowserCrossProfileControl,
            TeammatesAttack::SelfEvolutionEscalation,
            TeammatesAttack::ConversationBudget,
            TeammatesAttack::DeliveryBudget,
            TeammatesAttack::BrowserBudget,
            TeammatesAttack::ProcessBudget,
            TeammatesAttack::DiskBudget,
            TeammatesAttack::CpuBudget,
            TeammatesAttack::MemoryBudget,
            TeammatesAttack::NetworkBudget,
            TeammatesAttack::ModelBudget,
            TeammatesAttack::CostBudget,
        ] {
            cases.push(TeammatesFaultCase {
                id: format!("attack-{attack:?}").to_ascii_lowercase(),
                kill_point: None,
                attack,
                required_processes: process_set(&[
                    "agentd",
                    "agent-worker",
                    "agent-web",
                    "kernel-runner",
                    "tool-runner",
                    "browser-runner",
                    "chromium",
                    "xvfb",
                ]),
                minimum_concurrency: 5,
                required_invariants: invariant_set(&[
                    "fresh_root",
                    "real_provider_boundary",
                    "real_sandbox_boundary",
                    "cross_profile_denied",
                    "resource_limit_enforced",
                    "owner_visible_audit",
                    "no_secret_in_evidence",
                ]),
            });
        }
        Self { version: 1, cases }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ProviderBoundaryEvidence {
    Hosted,
    LocalModel,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammatesInvariantEvidence {
    pub name: String,
    pub passed: bool,
    pub safe_observation: String,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammatesScenarioEvidence {
    pub matrix_version: u32,
    pub case_id: String,
    pub fresh_data_root: String,
    pub package_manifest_sha256: String,
    pub provider_boundary: ProviderBoundaryEvidence,
    pub real_processes: std::collections::BTreeSet<String>,
    pub reopened_store_count: u32,
    pub observed_concurrency: usize,
    pub invariants: Vec<TeammatesInvariantEvidence>,
    pub secret_scan_matches: usize,
    pub used_mock_or_fake: bool,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammatesGateCaseResult {
    pub case_id: String,
    pub passed: bool,
    pub exit_code: Option<i32>,
    pub evidence_path: String,
    pub safe_failure: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammatesSecurityGateReport {
    pub matrix_version: u32,
    pub package_root: String,
    pub evidence_root: String,
    pub results: Vec<TeammatesGateCaseResult>,
}

pub struct TeammatesSecurityGate {
    pub matrix: TeammatesFaultMatrix,
    pub package_root: std::path::PathBuf,
    pub evidence_root: std::path::PathBuf,
    pub scenario_driver: std::path::PathBuf,
    pub provider_command: String,
    pub provider_boundary: ProviderBoundaryEvidence,
    pub package_manifest_sha256: String,
    pub timeout: std::time::Duration,
}

impl TeammatesSecurityGate {
    pub fn from_environment() -> Result<Self, String> {
        let package_root = required_path("KEITH_SECURITY_RELEASE_PATH")?;
        let trusted_key = required_text("KEITH_SECURITY_TRUSTED_PUBLIC_KEY")?;
        verify_packaged_binaries(&package_root, &trusted_key)?;
        Self::from_verified_package(&package_root)
    }

    pub fn from_verified_package(package_root: &std::path::Path) -> Result<Self, String> {
        let evidence_root = required_environment_path("KEITH_TEAMMATES_EVIDENCE_ROOT")?;
        let scenario_driver = std::env::var_os("KEITH_TEAMMATES_SECURITY_DRIVER")
            .filter(|value| !value.is_empty())
            .map(std::path::PathBuf::from)
            .unwrap_or_else(|| {
                package_root
                    .join("bin")
                    .join(format!("agentd{}", std::env::consts::EXE_SUFFIX))
            });
        let provider_command = std::env::var("KEITH_TEAMMATES_PROVIDER_COMMAND")
            .map_err(|_| "KEITH_TEAMMATES_PROVIDER_COMMAND is required".to_owned())?;
        let provider_boundary = match std::env::var("KEITH_TEAMMATES_PROVIDER_BOUNDARY")
            .map_err(|_| "KEITH_TEAMMATES_PROVIDER_BOUNDARY is required".to_owned())?
            .as_str()
        {
            "hosted" => ProviderBoundaryEvidence::Hosted,
            "local_model" => ProviderBoundaryEvidence::LocalModel,
            _ => {
                return Err(
                    "KEITH_TEAMMATES_PROVIDER_BOUNDARY must be hosted or local_model".into(),
                );
            }
        };
        let timeout_seconds = std::env::var("KEITH_TEAMMATES_CASE_TIMEOUT_SECONDS")
            .ok()
            .and_then(|value| value.parse::<u64>().ok())
            .unwrap_or(300);
        let package_root = std::fs::canonicalize(package_root)
            .map_err(|error| format!("package root is unavailable: {error}"))?;
        let scenario_driver = std::fs::canonicalize(scenario_driver)
            .map_err(|error| format!("scenario driver is unavailable: {error}"))?;
        if !scenario_driver.starts_with(&package_root) {
            return Err("scenario driver must be a verified packaged binary".into());
        }
        if provider_command.trim().is_empty() {
            return Err("real provider command is empty".into());
        }
        let package_manifest_sha256 = hex_sha256_file(&package_root.join(MANIFEST_FILE))?;
        std::fs::create_dir_all(&evidence_root)
            .map_err(|error| format!("cannot create evidence root: {error}"))?;
        Ok(Self {
            matrix: TeammatesFaultMatrix::release_gate(),
            package_root,
            evidence_root,
            scenario_driver,
            provider_command,
            provider_boundary,
            package_manifest_sha256,
            timeout: std::time::Duration::from_secs(timeout_seconds),
        })
    }

    pub fn run(&self) -> Result<TeammatesSecurityGateReport, String> {
        validate_packaged_processes(&self.package_root)?;
        let mut results = Vec::with_capacity(self.matrix.cases.len());
        for (index, case) in self.matrix.cases.iter().enumerate() {
            let case_root = fresh_case_root(&self.evidence_root, index, &case.id)?;
            let evidence_path = case_root.join("evidence.json");
            let stdout_path = case_root.join("stdout.log");
            let stderr_path = case_root.join("stderr.log");
            let stdout = std::fs::File::create(&stdout_path)
                .map_err(|error| format!("cannot create scenario stdout: {error}"))?;
            let stderr = std::fs::File::create(&stderr_path)
                .map_err(|error| format!("cannot create scenario stderr: {error}"))?;
            let matrix_case_path = case_root.join("case.json");
            let case_bytes = serde_json::to_vec_pretty(case)
                .map_err(|error| format!("cannot encode matrix case: {error}"))?;
            std::fs::write(&matrix_case_path, case_bytes)
                .map_err(|error| format!("cannot write matrix case: {error}"))?;
            let mut child = std::process::Command::new(&self.scenario_driver)
                .arg("teammates-security-scenario")
                .arg("--package-root")
                .arg(&self.package_root)
                .arg("--fresh-data-root")
                .arg(case_root.join("data"))
                .arg("--case")
                .arg(&matrix_case_path)
                .arg("--evidence")
                .arg(&evidence_path)
                .arg("--minimum-concurrency")
                .arg(case.minimum_concurrency.to_string())
                .env("KEITH_REAL_PROVIDER_COMMAND", &self.provider_command)
                .env(
                    "KEITH_REAL_PROVIDER_BOUNDARY",
                    match self.provider_boundary {
                        ProviderBoundaryEvidence::Hosted => "hosted",
                        ProviderBoundaryEvidence::LocalModel => "local_model",
                    },
                )
                .env(
                    "KEITH_VERIFIED_PACKAGE_MANIFEST_SHA256",
                    &self.package_manifest_sha256,
                )
                .stdin(std::process::Stdio::null())
                .stdout(std::process::Stdio::from(stdout))
                .stderr(std::process::Stdio::from(stderr))
                .spawn()
                .map_err(|error| format!("cannot start real security scenario: {error}"))?;
            let started = std::time::Instant::now();
            let status = loop {
                if let Some(status) = child
                    .try_wait()
                    .map_err(|error| format!("cannot observe security scenario: {error}"))?
                {
                    break status;
                }
                if started.elapsed() >= self.timeout {
                    let _ = child.kill();
                    let status = child
                        .wait()
                        .map_err(|error| format!("cannot reap timed-out scenario: {error}"))?;
                    break status;
                }
                std::thread::sleep(std::time::Duration::from_millis(50));
            };
            let result = validate_scenario(
                case,
                &evidence_path,
                &case_root.join("data"),
                &self.package_manifest_sha256,
                &self.provider_boundary,
                status.code(),
            )
            .unwrap_or_else(|safe_failure| TeammatesGateCaseResult {
                case_id: case.id.clone(),
                passed: false,
                exit_code: status.code(),
                evidence_path: evidence_path.display().to_string(),
                safe_failure: Some(safe_failure),
            });
            results.push(result);
        }
        let report = TeammatesSecurityGateReport {
            matrix_version: self.matrix.version,
            package_root: self.package_root.display().to_string(),
            evidence_root: self.evidence_root.display().to_string(),
            results,
        };
        let report_bytes = serde_json::to_vec_pretty(&report)
            .map_err(|error| format!("cannot encode teammates gate report: {error}"))?;
        std::fs::write(
            self.evidence_root.join("teammates-security-gate.json"),
            report_bytes,
        )
        .map_err(|error| format!("cannot write teammates gate report: {error}"))?;
        if report.results.iter().any(|result| !result.passed) {
            return Err("teammates security gate found load-bearing failures".into());
        }
        Ok(report)
    }
}

pub fn run_teammates(_root: &std::path::Path) -> Result<(), String> {
    let gate = TeammatesSecurityGate::from_environment()?;
    let report = gate.run()?;
    println!(
        "teammates security gate passed {} real packaged scenarios",
        report.results.len()
    );
    Ok(())
}

fn validate_scenario(
    case: &TeammatesFaultCase,
    evidence_path: &std::path::Path,
    expected_data_root: &std::path::Path,
    expected_package_manifest_sha256: &str,
    expected_provider_boundary: &ProviderBoundaryEvidence,
    exit_code: Option<i32>,
) -> Result<TeammatesGateCaseResult, String> {
    if exit_code != Some(0) {
        return Err(format!("scenario exited with {exit_code:?}"));
    }
    let bytes = std::fs::read(evidence_path)
        .map_err(|error| format!("scenario evidence missing: {error}"))?;
    let evidence: TeammatesScenarioEvidence = serde_json::from_slice(&bytes)
        .map_err(|error| format!("scenario evidence malformed: {error}"))?;
    if evidence.matrix_version != 1 || evidence.case_id != case.id {
        return Err("scenario evidence identity mismatch".into());
    }
    let observed_data_root = std::fs::canonicalize(&evidence.fresh_data_root)
        .map_err(|error| format!("scenario data root is unavailable: {error}"))?;
    let expected_data_root = std::fs::canonicalize(expected_data_root)
        .map_err(|error| format!("fresh data root is unavailable: {error}"))?;
    if observed_data_root != expected_data_root {
        return Err("scenario evidence was produced from a different data root".into());
    }
    if evidence.package_manifest_sha256 != expected_package_manifest_sha256 {
        return Err("scenario evidence is not bound to the verified package manifest".into());
    }
    if &evidence.provider_boundary != expected_provider_boundary {
        return Err("scenario exercised a different provider boundary".into());
    }
    if evidence.used_mock_or_fake {
        return Err("scenario used a mock or fake".into());
    }
    if evidence.secret_scan_matches != 0 {
        return Err("scenario evidence contains secret material".into());
    }
    if evidence.reopened_store_count == 0
        || evidence.observed_concurrency < case.minimum_concurrency
    {
        return Err("scenario did not exercise restart and required concurrency".into());
    }
    if !case.required_processes.is_subset(&evidence.real_processes) {
        return Err("scenario did not run every required real process".into());
    }
    let observations = evidence
        .invariants
        .iter()
        .map(|invariant| (invariant.name.as_str(), invariant.passed))
        .collect::<std::collections::BTreeMap<_, _>>();
    for invariant in &case.required_invariants {
        if observations.get(invariant.as_str()) != Some(&true) {
            return Err(format!("required invariant failed: {invariant}"));
        }
    }
    Ok(TeammatesGateCaseResult {
        case_id: case.id.clone(),
        passed: true,
        exit_code,
        evidence_path: evidence_path.display().to_string(),
        safe_failure: None,
    })
}

fn required_environment_path(name: &str) -> Result<std::path::PathBuf, String> {
    std::env::var_os(name)
        .filter(|value| !value.is_empty())
        .map(std::path::PathBuf::from)
        .ok_or_else(|| format!("{name} is required"))
}

fn fresh_case_root(
    evidence_root: &std::path::Path,
    index: usize,
    case_id: &str,
) -> Result<std::path::PathBuf, String> {
    let nonce = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_err(|error| format!("clock error: {error}"))?
        .as_nanos();
    let root = evidence_root.join(format!(
        "case-{index}-{}-{}-{nonce}",
        std::process::id(),
        sanitize_case_id(case_id)
    ));
    std::fs::create_dir(&root)
        .map_err(|error| format!("cannot create fresh case root: {error}"))?;
    std::fs::create_dir(root.join("data"))
        .map_err(|error| format!("cannot create fresh data root: {error}"))?;
    Ok(root)
}

fn validate_packaged_processes(package_root: &std::path::Path) -> Result<(), String> {
    for logical_name in PACKAGED_BINARIES {
        let path = package_root
            .join("bin")
            .join(format!("{logical_name}{}", std::env::consts::EXE_SUFFIX));
        if !path.is_file() {
            return Err(format!(
                "required packaged process is missing: {logical_name}"
            ));
        }
    }
    Ok(())
}

fn hex_sha256_file(path: &std::path::Path) -> Result<String, String> {
    let bytes = std::fs::read(path)
        .map_err(|error| format!("verified package manifest is unavailable: {error}"))?;
    Ok(Sha256::digest(bytes)
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect::<Vec<_>>()
        .join(""))
}

fn process_set(values: &[&str]) -> std::collections::BTreeSet<String> {
    values.iter().map(|value| (*value).to_owned()).collect()
}

fn invariant_set(values: &[&str]) -> std::collections::BTreeSet<String> {
    process_set(values)
}

fn sanitize_case_id(value: &str) -> String {
    value
        .chars()
        .map(|character| {
            if character.is_ascii_alphanumeric() || character == '-' {
                character
            } else {
                '-'
            }
        })
        .collect()
}
