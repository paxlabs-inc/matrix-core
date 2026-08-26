fn main() {
    if let Err(error) = keith_worker_runtime::run_from_environment_with_runtime(|arguments| {
        let path = arguments
            .runtime_config
            .as_deref()
            .ok_or_else(|| "--runtime-config is required for the daemon worker host".to_owned())?;
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
