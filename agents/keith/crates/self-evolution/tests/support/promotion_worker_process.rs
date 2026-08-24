fn main() {
    if let Err(error) = keith_worker_runtime::run_from_environment() {
        eprintln!("{error}");
        std::process::exit(1);
    }
}
