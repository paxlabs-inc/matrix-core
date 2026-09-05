#![forbid(unsafe_code)]

use std::path::PathBuf;

use keith_cua_runner::{LinuxComputerRuntime, RunnerService};

fn main() {
    if let Err(error) = run() {
        eprintln!("keith-cua-runner failed safely: {error}");
        std::process::exit(1);
    }
}

fn run() -> Result<(), Box<dyn std::error::Error>> {
    let mut arguments = std::env::args_os().skip(1);
    let mut root = None;
    let mut headless_process_test = false;
    while let Some(argument) = arguments.next() {
        if argument == "--root" {
            root = arguments.next().map(PathBuf::from);
        } else if argument == "--allow-headless-process-test" {
            headless_process_test = true;
        } else {
            return Err(format!("unsupported argument: {}", argument.to_string_lossy()).into());
        }
    }
    let root = root.ok_or("--root is required")?;
    let runtime = if headless_process_test {
        LinuxComputerRuntime::discover_for_process_tests()?
    } else {
        LinuxComputerRuntime::discover()?
    };
    let mut service = RunnerService::open(root, runtime)?;
    service.run_stdio()?;
    Ok(())
}
