#![forbid(unsafe_code)]

use std::path::PathBuf;

fn main() {
    if std::env::args_os().nth(1).as_deref() != Some(std::ffi::OsStr::new("--build-info")) {
        eprintln!("release report host only supports --build-info");
        std::process::exit(2);
    }
    let executable = std::env::current_exe().unwrap_or_else(|error| fail(&error.to_string()));
    let report = report_path(&executable);
    let bytes = std::fs::read(&report).unwrap_or_else(|error| fail(&error.to_string()));
    if let Err(error) = std::io::Write::write_all(&mut std::io::stdout().lock(), &bytes) {
        fail(&error.to_string());
    }
}

fn report_path(executable: &std::path::Path) -> PathBuf {
    let mut name = executable
        .file_name()
        .expect("release host executable has a file name")
        .to_os_string();
    name.push(".report.json");
    executable.with_file_name(name)
}

fn fail(message: &str) -> ! {
    eprintln!("release report host failed: {message}");
    std::process::exit(1);
}
