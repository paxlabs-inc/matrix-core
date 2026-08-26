#![forbid(unsafe_code)]

use std::path::PathBuf;
use std::sync::Arc;
use std::sync::atomic::AtomicBool;
use std::time::Duration;

use keith_daemon_core::{DaemonCore, DaemonOptions};
use signal_hook::consts::{SIGINT, SIGTERM};

fn main() {
    if let Err(error) = run() {
        eprintln!("{error}");
        std::process::exit(1);
    }
}

fn run() -> Result<(), String> {
    let mut arguments = std::env::args_os();
    let _program = arguments.next();
    let mut data_root = None;
    let mut socket = None;
    let mut worker = None;
    while let Some(argument) = arguments.next() {
        let value = arguments
            .next()
            .ok_or_else(|| "missing daemon argument value".to_owned())?;
        match argument.to_str() {
            Some("--data-root") => data_root = Some(PathBuf::from(value)),
            Some("--socket") => socket = Some(PathBuf::from(value)),
            Some("--worker-executable") => worker = Some(PathBuf::from(value)),
            Some(
                "--idle-seconds"
                | "--credential-root"
                | "--credential-key-env"
                | "--workspace-root",
            ) => {}
            _ => return Err("unknown daemon argument".into()),
        }
    }
    let shutdown = Arc::new(AtomicBool::new(false));
    signal_hook::flag::register(SIGTERM, Arc::clone(&shutdown))
        .map_err(|error| error.to_string())?;
    signal_hook::flag::register(SIGINT, Arc::clone(&shutdown))
        .map_err(|error| error.to_string())?;
    let mut daemon = DaemonCore::open(
        data_root.ok_or_else(|| "missing data root".to_owned())?,
        worker.ok_or_else(|| "missing worker executable".to_owned())?,
        DaemonOptions {
            idle_evict_after: Duration::from_secs(60),
            ..DaemonOptions::default()
        },
    )
    .map_err(|error| error.to_string())?;
    daemon
        .serve_local(
            &socket.ok_or_else(|| "missing socket".to_owned())?,
            &shutdown,
        )
        .map_err(|error| error.to_string())
}
