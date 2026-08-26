#![forbid(unsafe_code)]

use std::collections::BTreeSet;
use std::path::PathBuf;
use std::sync::Arc;

use keith_agent_types::{SessionId, UtcTimestamp};
use keith_kernel_broker::{
    DenyBridge, KernelBroker, KernelIsolation, KernelLimits, KernelNetwork, KernelRuntime,
    KernelSpec, NoKernelOutput,
};
use keith_provider_core::CancellationToken;

fn argument_value(arguments: &[String], name: &str) -> Result<String, String> {
    let index = arguments
        .iter()
        .position(|argument| argument == name)
        .ok_or_else(|| format!("{name} is required"))?;
    arguments
        .get(index.saturating_add(1))
        .cloned()
        .ok_or_else(|| format!("{name} requires a value"))
}

fn run() -> Result<(), String> {
    let arguments = std::env::args().skip(1).collect::<Vec<_>>();
    if arguments
        .iter()
        .any(|argument| matches!(argument.as_str(), "--version" | "-V"))
    {
        println!("{} {}", env!("CARGO_BIN_NAME"), env!("CARGO_PKG_VERSION"));
        return Ok(());
    }
    let workspace = PathBuf::from(argument_value(&arguments, "--workspace")?);
    let state = PathBuf::from(argument_value(&arguments, "--state")?);
    let code = argument_value(&arguments, "--code")?;
    let python = arguments
        .iter()
        .position(|argument| argument == "--python")
        .and_then(|index| arguments.get(index.saturating_add(1)))
        .map_or_else(|| PathBuf::from("/usr/bin/python3"), PathBuf::from);
    let broker =
        KernelBroker::open(state, Arc::new(DenyBridge), None).map_err(|error| error.to_string())?;
    let kernel_id = broker
        .start(
            KernelSpec {
                session_id: SessionId::new(),
                runtime: KernelRuntime::Python { executable: python },
                working_directory: workspace,
                isolation: KernelIsolation::Untrusted,
                network: KernelNetwork::Denied,
                limits: KernelLimits::default(),
                allowed_bridge: BTreeSet::new(),
            },
            UtcTimestamp::now().map_err(|error| error.to_string())?,
        )
        .map_err(|error| error.to_string())?;
    let mut output = NoKernelOutput;
    let execution = broker
        .execute(
            &kernel_id,
            code,
            &CancellationToken::default(),
            &mut output,
            UtcTimestamp::now().map_err(|error| error.to_string())?,
        )
        .map_err(|error| error.to_string())?;
    let spill = execution.spill.as_ref().map(|spill| {
        serde_json::json!({
            "artifact_id": spill.artifact_id,
            "path": spill.path,
            "bytes": spill.bytes,
            "preview": spill.preview,
            "media_type": spill.media_type,
        })
    });
    println!(
        "{}",
        serde_json::json!({
            "kernel_id": kernel_id,
            "result": execution.result,
            "error": execution.error,
            "preview": execution.preview,
            "total_output_bytes": execution.total_output_bytes,
            "output_truncated": execution.output_truncated,
            "spill": spill,
            "usage": execution.usage,
        })
    );
    broker
        .shutdown(&kernel_id)
        .map_err(|error| error.to_string())?;
    Ok(())
}

fn main() {
    if let Err(error) = run() {
        eprintln!("{error}");
        std::process::exit(1);
    }
}
