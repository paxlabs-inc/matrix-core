use std::path::Path;
use std::process::Command;

use keith_platform::{
    CredentialBackend, HostPlatform, LocalTransportBackend, PlatformCapabilities, PlatformPaths,
    ProcessControlBackend,
};

struct Probe {
    package: &'static str,
    filter: &'static str,
}

const PROBES: &[Probe] = &[
    Probe {
        package: "keith-platform",
        filter: "tests::platform_paths_are_absolute_scoped_and_stable",
    },
    Probe {
        package: "keith-platform",
        filter: "tests::capabilities_name_every_supported_native_backend",
    },
    Probe {
        package: "keith-platform",
        filter: "tests::reduced_isolation_is_denied_or_explicitly_acknowledged",
    },
    Probe {
        package: "keith-platform",
        filter: "tests::portable_paths_reject_host_specific_or_escaping_forms",
    },
    Probe {
        package: "keith-platform",
        filter: "tests::atomic_replacement_overwrites_an_existing_file_portably",
    },
    Probe {
        package: "keith-connection",
        filter: "tests::framed_json_conformance_over_permissionable_local_stream",
    },
    Probe {
        package: "keith-state-store",
        filter: "tests::concurrent_readers_and_writers_preserve_every_record",
    },
    Probe {
        package: "keith-supervisor",
        filter: "renewal_loss_stops_stale_worker_and_forced_replacement_advances_generation",
    },
    Probe {
        package: "keith-credentials",
        filter: "tests::encrypted_store_scopes_provider_channel_mcp_and_tool_resolution",
    },
    Probe {
        package: "keith-credentials",
        filter: "tests::native_master_key_store_identifies_the_host_backend_without_exposing_a_key",
    },
    Probe {
        package: "keith-agent-tui",
        filter: "connection::tests::local_attach_and_reconnect_run_the_real_protocol_twice",
    },
    Probe {
        package: "keith-agent-tui",
        filter: "tests::every_operator_surface_is_keyboard_reachable_and_renderable_at_all_widths",
    },
    Probe {
        package: "keith-agent-web",
        filter: "platform_web_startup_serves_the_login_shell",
    },
    Probe {
        package: "keith-agent-desktop",
        filter: "platform_desktop_startup_connects_and_stops_owned_daemon",
    },
    Probe {
        package: "keith-agentd",
        filter: "daemon_process_is_lazy_contains_crashes_and_adopts_after_restart",
    },
];

pub fn run(root: &Path) -> Result<(), String> {
    let capabilities = PlatformCapabilities::current();
    validate_capabilities(capabilities)?;
    PlatformPaths::discover().map_err(|error| format!("native paths are unavailable: {error}"))?;
    run_command(
        root,
        "cargo",
        &["build", "--workspace", "--bins", "--locked"],
    )?;
    for probe in PROBES {
        if !listed_tests(root, probe.package)?
            .iter()
            .any(|test| test == probe.filter)
        {
            return Err(format!(
                "platform probe {}::{} is missing",
                probe.package, probe.filter
            ));
        }
        run_command(
            root,
            "cargo",
            &[
                "test",
                "-p",
                probe.package,
                "--locked",
                probe.filter,
                "--",
                "--exact",
            ],
        )?;
    }
    println!(
        "platform acceptance passed on {:?}: {} native probes",
        capabilities.platform,
        PROBES.len()
    );
    Ok(())
}

fn listed_tests(root: &Path, package: &str) -> Result<Vec<String>, String> {
    let output = Command::new("cargo")
        .args(["test", "-q", "-p", package, "--locked", "--", "--list"])
        .current_dir(root)
        .output()
        .map_err(|error| format!("failed to enumerate {package} platform probes: {error}"))?;
    if !output.status.success() {
        return Err(format!(
            "failed to enumerate {package} platform probes: {}",
            String::from_utf8_lossy(&output.stderr)
        ));
    }
    Ok(String::from_utf8_lossy(&output.stdout)
        .lines()
        .filter_map(|line| line.strip_suffix(": test"))
        .map(str::to_owned)
        .collect())
}

fn validate_capabilities(capabilities: PlatformCapabilities) -> Result<(), String> {
    if capabilities.platform == HostPlatform::Unsupported
        || capabilities.local_transport == LocalTransportBackend::Unsupported
        || capabilities.process_control == ProcessControlBackend::Unsupported
        || capabilities.credentials == CredentialBackend::Unsupported
        || !capabilities.tui_restoration
        || !capabilities.web_startup
        || !capabilities.desktop_startup
    {
        return Err(format!(
            "platform is unsupported or missing a required backend: {capabilities:?}"
        ));
    }
    Ok(())
}

fn run_command(root: &Path, program: &str, arguments: &[&str]) -> Result<(), String> {
    let status = Command::new(program)
        .args(arguments)
        .current_dir(root)
        .status()
        .map_err(|error| format!("failed to run {program}: {error}"))?;
    if status.success() {
        Ok(())
    } else {
        Err(format!(
            "{program} {} failed with {status}",
            arguments.join(" ")
        ))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn all_release_platforms_require_complete_native_capabilities() {
        for platform in [
            HostPlatform::Linux,
            HostPlatform::MacOs,
            HostPlatform::Windows,
        ] {
            validate_capabilities(PlatformCapabilities::for_platform(platform)).unwrap();
        }
        assert!(
            validate_capabilities(PlatformCapabilities::for_platform(
                HostPlatform::Unsupported
            ))
            .is_err()
        );
    }
}
