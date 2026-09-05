use std::collections::{BTreeMap, BTreeSet};
use std::env;
use std::fs;
use std::path::{Path, PathBuf};
use std::process::Command;

use keith_release::{decode_public_key, verify_packaged_build_reports, verify_release};
use serde::Deserialize;

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
    "injection",
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
    "plugin_escape",
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
    "durable_boundary_crash",
    "evaluator_tampering",
    "oauth_substitution",
    "protocol_confusion",
    "recording_leakage",
    "ssrf",
    "stale_lease",
    "stale_input",
    "stream_hijack",
    "symlink_race",
    "terminal_escape",
    "unauthenticated_access",
    "webhook_forgery",
];

const KEITH_EVERYWHERE_SURFACES: &[&str] = &[
    "channel",
    "acp",
    "plugin",
    "composio",
    "computer",
    "teaching",
    "meta_harness",
    "credentials",
    "profiles",
    "approvals",
    "data_control",
];

const KEITH_EVERYWHERE_ATTACKS: &[&str] = &[
    "injection",
    "ssrf",
    "webhook_forgery",
    "protocol_confusion",
    "plugin_escape",
    "oauth_substitution",
    "stream_hijack",
    "stale_input",
    "recording_leakage",
    "evaluator_tampering",
    "cross_profile",
    "durable_boundary_crash",
];

const PRIVILEGED_TRANSITIONS: &[&str] = &[
    "self_evolution",
    "plugin_install",
    "account_connection",
    "grant_widening",
    "computer_control",
    "action_approval",
    "credential_access",
    "profile_selection",
];

const DATA_CONTROL_CLASSES: &[&str] = &[
    "channel_accounts",
    "channel_events",
    "acp_metadata",
    "plugins",
    "connected_accounts",
    "computer_state",
    "recordings",
    "recipes",
    "traces",
    "candidates",
    "derived_indexes",
];

const FORBIDDEN_AUDIT_FIELDS: &[&str] = &[
    "raw_credentials",
    "secrets",
    "full_private_content",
    "reusable_stream_urls",
    "private_reasoning",
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
    "authority_widening",
    "credential_leak",
    "credential_exfiltration",
    "cross_profile_access",
    "data_loss",
    "fabricated_success",
    "self_evolution_candidate_tamper",
    "self_evolution_credential_access",
    "self_evolution_filesystem_escape",
    "self_evolution_network_escape",
    "self_evolution_process_escape",
    "self_evolution_protected_path",
    "self_evolution_unsigned_worker",
    "unrecoverable_state",
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
    Probe {
        package: "keith-channel-adapters",
        test: "slack_signed_webhook_uses_official_hmac_vector_and_rejects_before_parse",
        attacks: &["webhook_forgery", "injection"],
    },
    Probe {
        package: "keith-agent-acp",
        test: "real_process_refuses_unsupported_protocol_versions",
        attacks: &["protocol_confusion"],
    },
    Probe {
        package: "keith-agent-acp",
        test: "managed_http_sse_authenticates_replays_and_closes_a_real_connection",
        attacks: &[],
    },
    Probe {
        package: "keith-plugin-host",
        test: "abi_ambient_wasi_import_is_rejected_instead_of_inherited",
        attacks: &["plugin_escape"],
    },
    Probe {
        package: "keith-composio",
        test: "tests::durable_state_refuses_profile_or_provider_identity_substitution",
        attacks: &["oauth_substitution", "cross_profile"],
    },
    Probe {
        package: "keith-composio",
        test: "control_plane_and_mcp_endpoint_policy_blocks_credential_ssrf",
        attacks: &["ssrf"],
    },
    Probe {
        package: "keith-cua-runner",
        test: "real_runner_process_enforces_stream_and_exclusive_control_across_restart",
        attacks: &["stream_hijack"],
    },
    Probe {
        package: "keith-cua",
        test: "controller::tests::stale_coordinate_is_refused_and_exact_semantic_action_is_audited",
        attacks: &["stale_input"],
    },
    Probe {
        package: "keith-task-recipe",
        test: "synchronized_capture_substitutes_credentials_and_never_serializes_raw_secrets",
        attacks: &["recording_leakage"],
    },
    Probe {
        package: "keith-meta-harness",
        test: "meta_harness_evaluator_leakage_injection_crash_and_protected_surface_attacks_fail_closed",
        attacks: &["evaluator_tampering", "injection"],
    },
    Probe {
        package: "keith-agentd",
        test: "daemon_process_integration_lifecycle_survives_crash_and_quarantines_corrupt_service",
        attacks: &["durable_boundary_crash", "cross_profile"],
    },
    Probe {
        package: "keith-platform-contracts",
        test: "tests::consequential_action_requires_exact_unexpired_approval",
        attacks: &[],
    },
    Probe {
        package: "keith-acp",
        test: "permission::tests::client_response_cannot_substitute_target_or_unoffered_option",
        attacks: &[],
    },
    Probe {
        package: "keith-plugin-host",
        test: "authority_lifecycle_provenance_grants_updates_and_uninstall_are_durable",
        attacks: &[],
    },
    Probe {
        package: "keith-plugin-host",
        test: "authority_crash_loop_and_corruption_enter_safe_mode_without_blocking_uninstall",
        attacks: &[],
    },
    Probe {
        package: "keith-composio",
        test: "tests::real_http_journey_proves_sessions_accounts_policy_mcp_recovery_and_isolation",
        attacks: &[],
    },
    Probe {
        package: "keith-cua-runner",
        test: "named_credential_is_origin_scoped_and_only_fills_a_protected_field",
        attacks: &[],
    },
    Probe {
        package: "keith-cua-runner",
        test: "real_runner_process_crash_reconciles_without_cross_profile_access",
        attacks: &[],
    },
    Probe {
        package: "keith-task-recipe",
        test: "filesystem_store_exports_sanitized_data_and_cascades_complete_deletion",
        attacks: &[],
    },
    Probe {
        package: "keith-meta-harness",
        test: "meta_harness_real_failure_diagnosis_candidates_held_out_pareto_and_history",
        attacks: &[],
    },
    Probe {
        package: "keith-state-store",
        test: "tests::external_service_collections_round_trip_restart_and_exact_deletion",
        attacks: &[],
    },
];

#[derive(Clone, Debug, Deserialize, Eq, Ord, PartialEq, PartialOrd)]
#[serde(deny_unknown_fields)]
struct ProbeReference {
    package: String,
    test: String,
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct KeithEverywhereSecurityManifest {
    schema_version: u16,
    surfaces: Vec<String>,
    privileged_transitions: Vec<String>,
    attacks: Vec<AttackCoverage>,
    authority_boundaries: Vec<AuthorityBoundaryCoverage>,
    durable_boundaries: Vec<DurableBoundaryCoverage>,
    data_control: Vec<DataControlCoverage>,
    forbidden_audit_fields: Vec<String>,
    audit_records: Vec<SafeAuditRecord>,
    unavailable_credentialed_services: Vec<String>,
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct AttackCoverage {
    class: String,
    probes: Vec<ProbeReference>,
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct AuthorityBoundaryCoverage {
    source_surface: String,
    denies_all_privileged_transitions: bool,
    probes: Vec<ProbeReference>,
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct DurableBoundaryCoverage {
    surface: String,
    recovery: Vec<String>,
    probes: Vec<ProbeReference>,
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct DataControlCoverage {
    class: String,
    exported: bool,
    deleted: bool,
    remaining_records: u64,
    remaining_objects: u64,
    probe: ProbeReference,
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct SafeAuditRecord {
    correlation_id: String,
    profile_id: String,
    surface: String,
    action: String,
    outcome: String,
    safe_summary: String,
}

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
    validate_source_security(root)?;
    let release = required_path("KEITH_SECURITY_RELEASE_PATH")?;
    let trusted_key = required_text("KEITH_SECURITY_TRUSTED_PUBLIC_KEY")?;
    verify_packaged_binaries(&release, &trusted_key)?;
    run_source_probes(root)?;
    println!(
        "security gate passed: {} attacks, {} packaged binaries, {} real test probes",
        REQUIRED_ATTACKS.len(),
        PACKAGED_BINARIES.len(),
        PROBES.len()
    );
    Ok(())
}

pub fn run_source(root: &Path) -> Result<(), String> {
    validate_source_security(root)?;
    run_source_probes(root)
}

fn validate_source_security(root: &Path) -> Result<(), String> {
    validate_corpus()?;
    validate_keith_everywhere_manifest(
        &fs::read(root.join("tests/security/keith_everywhere.json")).map_err(|error| {
            format!("Keith Everywhere security manifest is unavailable: {error}")
        })?,
    )?;
    validate_findings(
        &fs::read(root.join("security/findings.json"))
            .map_err(|error| format!("security finding ledger is unavailable: {error}"))?,
    )
}

fn run_source_probes(root: &Path) -> Result<(), String> {
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
            &[
                "test",
                "-p",
                package,
                "--release",
                "--locked",
                "--all-features",
            ],
        )?;
    }
    println!(
        "security source probes passed: {} attacks, {} real test probes",
        REQUIRED_ATTACKS.len(),
        PROBES.len()
    );
    Ok(())
}

fn validate_keith_everywhere_manifest(bytes: &[u8]) -> Result<(), String> {
    let manifest: KeithEverywhereSecurityManifest = serde_json::from_slice(bytes)
        .map_err(|error| format!("Keith Everywhere security manifest is invalid: {error}"))?;
    if manifest.schema_version != 1 {
        return Err(format!(
            "Keith Everywhere security schema {} is unsupported",
            manifest.schema_version
        ));
    }
    exact_strings(
        "security surfaces",
        &manifest.surfaces,
        KEITH_EVERYWHERE_SURFACES,
    )?;
    exact_strings(
        "privileged transitions",
        &manifest.privileged_transitions,
        PRIVILEGED_TRANSITIONS,
    )?;
    validate_manifest_attacks(&manifest)?;
    validate_manifest_authority_boundaries(&manifest)?;
    validate_manifest_durable_boundaries(&manifest)?;
    validate_manifest_data_control(&manifest)?;
    exact_strings(
        "forbidden audit fields",
        &manifest.forbidden_audit_fields,
        FORBIDDEN_AUDIT_FIELDS,
    )?;
    validate_audit_records(&manifest.audit_records)?;
    if manifest
        .unavailable_credentialed_services
        .iter()
        .any(|service| service.trim().is_empty())
    {
        return Err("credential availability contains an empty service name".into());
    }
    Ok(())
}

fn validate_manifest_attacks(manifest: &KeithEverywhereSecurityManifest) -> Result<(), String> {
    let attacks = manifest
        .attacks
        .iter()
        .map(|coverage| coverage.class.clone())
        .collect::<Vec<_>>();
    exact_strings(
        "Keith Everywhere attacks",
        &attacks,
        KEITH_EVERYWHERE_ATTACKS,
    )?;
    for coverage in &manifest.attacks {
        validate_probe_references(&coverage.probes, &format!("attack {}", coverage.class))?;
    }
    Ok(())
}

fn validate_manifest_authority_boundaries(
    manifest: &KeithEverywhereSecurityManifest,
) -> Result<(), String> {
    let authority_surfaces = manifest
        .authority_boundaries
        .iter()
        .map(|coverage| coverage.source_surface.clone())
        .collect::<Vec<_>>();
    exact_strings(
        "authority source surfaces",
        &authority_surfaces,
        KEITH_EVERYWHERE_SURFACES,
    )?;
    for coverage in &manifest.authority_boundaries {
        if !coverage.denies_all_privileged_transitions {
            return Err(format!(
                "{} does not deny every privileged cross-surface transition",
                coverage.source_surface
            ));
        }
        validate_probe_references(
            &coverage.probes,
            &format!("{} authority boundary", coverage.source_surface),
        )?;
    }
    Ok(())
}

fn validate_manifest_durable_boundaries(
    manifest: &KeithEverywhereSecurityManifest,
) -> Result<(), String> {
    let durable_surfaces = manifest
        .durable_boundaries
        .iter()
        .map(|coverage| coverage.surface.clone())
        .collect::<Vec<_>>();
    exact_strings(
        "durable boundary surfaces",
        &durable_surfaces,
        KEITH_EVERYWHERE_SURFACES,
    )?;
    let required_recovery = BTreeSet::from([
        "safe_reconciliation",
        "cancellation",
        "quarantine",
        "reversal",
        "deletion",
        "daemon_availability",
    ]);
    let mut observed_recovery = BTreeSet::new();
    for coverage in &manifest.durable_boundaries {
        let recovery = coverage
            .recovery
            .iter()
            .map(String::as_str)
            .collect::<BTreeSet<_>>();
        if !recovery.contains("safe_reconciliation")
            || !recovery.contains("daemon_availability")
            || !recovery.is_subset(&required_recovery)
        {
            return Err(format!(
                "{} durable boundary lacks safe reconciliation or daemon availability",
                coverage.surface
            ));
        }
        observed_recovery.extend(recovery);
        validate_probe_references(
            &coverage.probes,
            &format!("{} durable boundary", coverage.surface),
        )?;
    }
    if observed_recovery != required_recovery {
        return Err("durable boundary suite does not cover every required recovery outcome".into());
    }
    Ok(())
}

fn validate_manifest_data_control(
    manifest: &KeithEverywhereSecurityManifest,
) -> Result<(), String> {
    let data_classes = manifest
        .data_control
        .iter()
        .map(|coverage| coverage.class.clone())
        .collect::<Vec<_>>();
    exact_strings("data-control classes", &data_classes, DATA_CONTROL_CLASSES)?;
    for coverage in &manifest.data_control {
        if !coverage.exported
            || !coverage.deleted
            || coverage.remaining_records != 0
            || coverage.remaining_objects != 0
        {
            return Err(format!(
                "{} lacks complete export, deletion, or exact zero-remnant proof",
                coverage.class
            ));
        }
        validate_probe_references(
            std::slice::from_ref(&coverage.probe),
            &format!("{} data control", coverage.class),
        )?;
    }
    Ok(())
}

fn exact_strings(label: &str, actual: &[String], expected: &[&str]) -> Result<(), String> {
    let actual_len = actual.len();
    let actual = actual.iter().map(String::as_str).collect::<BTreeSet<_>>();
    let expected = expected.iter().copied().collect::<BTreeSet<_>>();
    if actual == expected && actual_len == expected.len() {
        Ok(())
    } else {
        Err(format!(
            "{label} mismatch: expected {expected:?}, got {actual:?}"
        ))
    }
}

fn validate_probe_references(probes: &[ProbeReference], label: &str) -> Result<(), String> {
    if probes.is_empty() {
        return Err(format!("{label} has no executable security probe"));
    }
    let known = PROBES
        .iter()
        .map(|probe| (probe.package, probe.test))
        .collect::<BTreeSet<_>>();
    for probe in probes {
        if !known.contains(&(probe.package.as_str(), probe.test.as_str())) {
            return Err(format!(
                "{label} references unknown probe {}::{}",
                probe.package, probe.test
            ));
        }
    }
    Ok(())
}

fn validate_audit_records(records: &[SafeAuditRecord]) -> Result<(), String> {
    if records.len() < 3 {
        return Err("cross-surface audit evidence requires at least three records".into());
    }
    let correlation_ids = records
        .iter()
        .map(|record| record.correlation_id.as_str())
        .collect::<BTreeSet<_>>();
    let profiles = records
        .iter()
        .map(|record| record.profile_id.as_str())
        .collect::<BTreeSet<_>>();
    let surfaces = records
        .iter()
        .map(|record| record.surface.as_str())
        .collect::<BTreeSet<_>>();
    if correlation_ids.len() != 1 || profiles.len() != 1 || surfaces.len() < 3 {
        return Err(
            "audit records must correlate at least three surfaces within one profile".into(),
        );
    }
    let known_surfaces = KEITH_EVERYWHERE_SURFACES
        .iter()
        .copied()
        .collect::<BTreeSet<_>>();
    for record in records {
        let summary = record.safe_summary.to_ascii_lowercase();
        if !known_surfaces.contains(record.surface.as_str())
            || record.action.trim().is_empty()
            || record.outcome.trim().is_empty()
            || record.safe_summary.len() > 160
            || summary.contains("secret")
            || summary.contains("token")
            || summary.contains("password")
            || summary.contains("http://")
            || summary.contains("https://")
        {
            return Err(format!(
                "audit record for {} is unbounded or contains private material",
                record.surface
            ));
        }
    }
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
            "--all-features",
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

    #[test]
    fn keith_everywhere_manifest_is_complete_and_tampering_fails_closed() {
        let manifest = include_bytes!("../../../tests/security/keith_everywhere.json");
        validate_keith_everywhere_manifest(manifest).unwrap();

        let mut missing_surface: serde_json::Value = serde_json::from_slice(manifest).unwrap();
        missing_surface["surfaces"].as_array_mut().unwrap().pop();
        assert!(
            validate_keith_everywhere_manifest(&serde_json::to_vec(&missing_surface).unwrap())
                .is_err()
        );

        let mut fabricated_probe: serde_json::Value = serde_json::from_slice(manifest).unwrap();
        fabricated_probe["attacks"][0]["probes"][0]["test"] =
            serde_json::Value::String("fabricated_success".into());
        assert!(
            validate_keith_everywhere_manifest(&serde_json::to_vec(&fabricated_probe).unwrap())
                .is_err()
        );
    }
}
