fn main() {
    if matches!(std::env::args().nth(1).as_deref(), Some("--version" | "-V")) {
        println!("{} {}", env!("CARGO_BIN_NAME"), env!("CARGO_PKG_VERSION"));
        return;
    }
    if matches!(std::env::args().nth(1).as_deref(), Some("--build-info")) {
        let report = keith_build_info::worker_report();
        match serde_json::to_string_pretty(&report) {
            Ok(json) => println!("{json}"),
            Err(error) => {
                eprintln!("{error}");
                std::process::exit(1);
            }
        }
        return;
    }
    if let Err(error) = keith_worker_runtime::run_from_environment_with_runtime(|arguments| {
        if arguments.canary {
            return Ok(Box::new(keith_local_runtime::CandidateCanaryRuntime::new()));
        }
        let path = arguments
            .runtime_config
            .as_deref()
            .ok_or_else(|| "--runtime-config is required for agent-worker".to_owned())?;
        let launch = keith_local_runtime::LocalRuntimeLaunchConfig::load(path)
            .map_err(|error| error.to_string())?;
        let runtime = launch
            .open_worker(
                arguments.grant.root_tree_id.clone(),
                arguments.grant.worker_id.clone(),
                arguments.grant.authentication.clone(),
            )
            .map_err(|error| error.to_string())?;
        Ok(Box::new(runtime))
    }) {
        eprintln!("{error}");
        std::process::exit(1);
    }
}
