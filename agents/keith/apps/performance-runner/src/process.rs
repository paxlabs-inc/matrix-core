use std::collections::BTreeMap;
use std::fs;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::time::{Duration, Instant};

use keith_worker_runtime::{read_registration, registration_path};
use serde::Serialize;

#[cfg(unix)]
use nix::sys::signal::{Signal, kill};
#[cfg(unix)]
use nix::unistd::Pid;

#[derive(Clone, Debug, Serialize)]
pub struct ResourceSample {
    pub elapsed_millis: u64,
    pub component: String,
    pub pid: u32,
    pub resident_bytes: Option<u64>,
    pub virtual_bytes: Option<u64>,
    pub file_descriptors: Option<usize>,
    pub threads: Option<u64>,
}

#[derive(Clone, Debug, Serialize)]
pub struct ResourceSummary {
    pub samples: usize,
    pub first_resident_bytes: Option<u64>,
    pub maximum_resident_bytes: Option<u64>,
    pub last_resident_bytes: Option<u64>,
    pub resident_growth_bytes: Option<i64>,
    pub maximum_file_descriptors: Option<usize>,
}

pub struct ManagedProcess {
    pub child: Child,
}

impl ManagedProcess {
    pub fn spawn(label: &str, command: &mut Command, log_path: &Path) -> Result<Self, String> {
        let stdout = fs::File::create(log_path).map_err(|error| error.to_string())?;
        let stderr = stdout.try_clone().map_err(|error| error.to_string())?;
        let child = command
            .stdin(Stdio::null())
            .stdout(Stdio::from(stdout))
            .stderr(Stdio::from(stderr))
            .spawn()
            .map_err(|error| format!("failed to start {label}: {error}"))?;
        Ok(Self { child })
    }

    pub fn pid(&self) -> u32 {
        self.child.id()
    }

    pub fn terminate(&mut self) -> Result<(), String> {
        terminate(self.child.id(), false)?;
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            if self
                .child
                .try_wait()
                .map_err(|error| error.to_string())?
                .is_some()
            {
                return Ok(());
            }
            if Instant::now() >= deadline {
                terminate(self.child.id(), true)?;
                self.child.wait().map_err(|error| error.to_string())?;
                return Ok(());
            }
            std::thread::sleep(Duration::from_millis(20));
        }
    }
}

impl Drop for ManagedProcess {
    fn drop(&mut self) {
        let _ = self.terminate();
    }
}

pub fn sample_process(started: Instant, component: impl Into<String>, pid: u32) -> ResourceSample {
    let status = fs::read_to_string(Path::new("/proc").join(pid.to_string()).join("status")).ok();
    let kibibytes = |prefix: &str| {
        status.as_deref().and_then(|status| {
            status
                .lines()
                .find_map(|line| line.strip_prefix(prefix))
                .and_then(|value| value.split_whitespace().next())
                .and_then(|value| value.parse::<u64>().ok())
                .and_then(|value| value.checked_mul(1_024))
        })
    };
    let threads = status.as_deref().and_then(|status| {
        status
            .lines()
            .find_map(|line| line.strip_prefix("Threads:"))
            .and_then(|value| value.trim().parse::<u64>().ok())
    });
    let file_descriptors = fs::read_dir(Path::new("/proc").join(pid.to_string()).join("fd"))
        .ok()
        .map(Iterator::count);
    ResourceSample {
        elapsed_millis: u64::try_from(started.elapsed().as_millis()).unwrap_or(u64::MAX),
        component: component.into(),
        pid,
        resident_bytes: kibibytes("VmRSS:"),
        virtual_bytes: kibibytes("VmSize:"),
        file_descriptors,
        threads,
    }
}

pub fn worker_pids(data_root: &Path) -> Vec<(String, u32)> {
    let runtime = data_root.join("runtime");
    let Ok(entries) = fs::read_dir(runtime.join("workers")) else {
        return Vec::new();
    };
    let mut workers = entries
        .flatten()
        .filter_map(|entry| {
            let name = entry.file_name();
            let name = name.to_str()?;
            let root = name.strip_suffix(".json")?.parse().ok()?;
            let registration = read_registration(&registration_path(&runtime, &root)).ok()?;
            Some((
                format!("worker:{}", registration.root_tree_id),
                registration.pid,
            ))
        })
        .collect::<Vec<_>>();
    workers.sort();
    workers.dedup();
    workers
}

pub fn descendant_pids(parent: u32) -> Vec<u32> {
    let mut pending = vec![parent];
    let mut descendants = Vec::new();
    while let Some(pid) = pending.pop() {
        let children = fs::read_to_string(
            Path::new("/proc")
                .join(pid.to_string())
                .join("task")
                .join(pid.to_string())
                .join("children"),
        )
        .unwrap_or_default();
        for child in children
            .split_whitespace()
            .filter_map(|value| value.parse::<u32>().ok())
        {
            if child != parent && !descendants.contains(&child) {
                descendants.push(child);
                pending.push(child);
            }
        }
    }
    descendants.sort_unstable();
    descendants
}

pub fn summarize_resources(samples: &[ResourceSample]) -> BTreeMap<String, ResourceSummary> {
    let mut grouped = BTreeMap::<String, Vec<&ResourceSample>>::new();
    for sample in samples {
        grouped
            .entry(sample.component.clone())
            .or_default()
            .push(sample);
    }
    grouped
        .into_iter()
        .map(|(component, samples)| {
            let resident = samples
                .iter()
                .filter_map(|sample| sample.resident_bytes)
                .collect::<Vec<_>>();
            let first = resident.first().copied();
            let last = resident.last().copied();
            let growth = first.zip(last).map(|(first, last)| {
                i64::try_from(last).unwrap_or(i64::MAX) - i64::try_from(first).unwrap_or(i64::MAX)
            });
            (
                component,
                ResourceSummary {
                    samples: samples.len(),
                    first_resident_bytes: first,
                    maximum_resident_bytes: resident.into_iter().max(),
                    last_resident_bytes: last,
                    resident_growth_bytes: growth,
                    maximum_file_descriptors: samples
                        .iter()
                        .filter_map(|sample| sample.file_descriptors)
                        .max(),
                },
            )
        })
        .collect()
}

#[cfg(unix)]
pub fn terminate(pid: u32, force: bool) -> Result<(), String> {
    let pid = i32::try_from(pid).map_err(|error| error.to_string())?;
    let signal = if force {
        Signal::SIGKILL
    } else {
        Signal::SIGTERM
    };
    kill(Pid::from_raw(pid), signal).map_err(|error| error.to_string())
}

#[cfg(windows)]
pub fn terminate(pid: u32, force: bool) -> Result<(), String> {
    let mut command = Command::new("taskkill");
    command.args(["/PID", &pid.to_string()]);
    if force {
        command.arg("/F");
    }
    let status = command.status().map_err(|error| error.to_string())?;
    status
        .success()
        .then_some(())
        .ok_or_else(|| format!("taskkill failed with {status}"))
}

#[allow(dead_code)]
fn _portable_placeholder(_path: PathBuf) {}
