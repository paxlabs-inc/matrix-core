use std::collections::BTreeSet;
use std::fmt::Write as _;
use std::fs;
use std::path::PathBuf;
use std::process::Command;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex, OnceLock};

use keith_plugin_host::abi::{ExecutablePlugin, PluginHostCallError, PluginHostContext};
use keith_plugin_sdk::{
    HOST_API_VERSION, HostRequest, HostResponse, PayloadFormat, PluginDigest, PluginGrant,
    PluginHook, PluginKind, PluginLogLevel, PluginManifest, PluginMigrationContract,
    PluginOperation, PluginPublisher, PluginRisk, PluginSignature, PluginStatus, PluginStreamFrame,
    PluginToolDescriptor, ResourceGrants,
};

#[derive(Default)]
struct ReferenceContext {
    cancellation_checks: AtomicUsize,
    cancel_on_check: Option<usize>,
    events: Mutex<Vec<(String, Vec<u8>)>>,
    logs: Mutex<Vec<String>>,
}

impl PluginHostContext for ReferenceContext {
    fn credential(&self, name: &str) -> Result<Vec<u8>, PluginHostCallError> {
        if name == "provider-token" {
            Ok(b"top-secret".to_vec())
        } else {
            Err(PluginHostCallError::Denied)
        }
    }

    fn emit_event(&self, topic: &str, payload: &[u8]) -> Result<(), PluginHostCallError> {
        self.events
            .lock()
            .expect("event lock")
            .push((topic.to_owned(), payload.to_vec()));
        Ok(())
    }

    fn safe_log(&self, _level: PluginLogLevel, message: &str) -> Result<(), PluginHostCallError> {
        self.logs.lock().expect("log lock").push(message.to_owned());
        Ok(())
    }

    fn cancelled(&self, _cancellation_id: &str) -> bool {
        let check = self.cancellation_checks.fetch_add(1, Ordering::SeqCst);
        self.cancel_on_check == Some(check)
    }
}

fn descriptor(streaming: bool) -> PluginToolDescriptor {
    PluginToolDescriptor {
        name: "echo".to_owned(),
        description: "Echoes a typed message".to_owned(),
        input_schema: r#"{"type":"object","required":["message"],"properties":{"message":{"type":"string"}},"additionalProperties":false}"#.to_owned(),
        output_schema: r#"{"type":"object","required":["echo"],"properties":{"echo":{"type":"string"}},"additionalProperties":false}"#.to_owned(),
        risk: PluginRisk::ReadOnly,
        timeout_ms: 1_000,
        supports_cancellation: true,
        streaming,
        concurrency_limit: 1,
        required_grants: BTreeSet::from([PluginGrant::SafeLog]),
    }
}

fn manifest(allow_events: bool) -> PluginManifest {
    let grants = ResourceGrants {
        max_memory_bytes: 2 * 1_024 * 1_024,
        max_fuel: 100_000,
        max_wall_time_ms: 1_000,
        allow_events,
        credential_names: BTreeSet::from(["provider-token".to_owned()]),
        ..ResourceGrants::default()
    };
    PluginManifest {
        manifest_version: 2,
        id: "reference-component".to_owned(),
        name: "Reference Component".to_owned(),
        version: "1.0.0".to_owned(),
        host_api_min: HOST_API_VERSION,
        host_api_max: HOST_API_VERSION,
        kind: PluginKind::WasiComponent,
        hooks: BTreeSet::from([PluginHook::Tool]),
        grants,
        publisher: Some(PluginPublisher {
            id: "keith-reference".to_owned(),
            name: "Keith Reference Publisher".to_owned(),
            key_id: "reference-key".to_owned(),
        }),
        digest: Some(PluginDigest {
            algorithm: "sha256".to_owned(),
            value: "ab".repeat(32),
        }),
        signature: Some(PluginSignature {
            algorithm: "ed25519".to_owned(),
            key_id: "reference-key".to_owned(),
            value: "reference-signature".to_owned(),
        }),
        tools: vec![descriptor(true)],
        commands: Vec::new(),
        migration: Some(PluginMigrationContract::default()),
    }
}

fn request() -> HostRequest {
    HostRequest {
        interface_version: HOST_API_VERSION,
        invocation_id: "invocation-1".to_owned(),
        operation: PluginOperation::Tool,
        target: Some("echo".to_owned()),
        payload_format: PayloadFormat::Json,
        payload: br#"{"message":"hello"}"#.to_vec(),
        cancellation_id: "cancel-1".to_owned(),
    }
}

fn response(status: PluginStatus, stream: bool) -> HostResponse {
    HostResponse {
        interface_version: HOST_API_VERSION,
        invocation_id: "invocation-1".to_owned(),
        status,
        payload_format: PayloadFormat::Json,
        payload: if status == PluginStatus::Completed {
            br#"{"echo":"hello"}"#.to_vec()
        } else {
            b"null".to_vec()
        },
        stream: if stream {
            vec![PluginStreamFrame {
                sequence: 0,
                payload_format: PayloadFormat::Json,
                payload: br#"{"echo":"hel"}"#.to_vec(),
            }]
        } else {
            Vec::new()
        },
        safe_error: None,
    }
}

fn legacy_core_module(
    completed: &HostResponse,
    cancelled: &HostResponse,
    denied: &HostResponse,
) -> Vec<u8> {
    let completed = serde_json::to_vec(completed).expect("completed response");
    let cancelled = serde_json::to_vec(cancelled).expect("cancelled response");
    let denied = serde_json::to_vec(denied).expect("denied response");
    let event = serde_json::to_vec(&keith_plugin_sdk::PluginHostCall::EmitEvent {
        topic: "progress".to_owned(),
        payload: b"halfway".to_vec(),
    })
    .expect("event request");
    let completed_pointer = 2_048_u64;
    let cancelled_pointer = 8_192_u64;
    let denied_pointer = 12_288_u64;
    let completed_packed = (completed_pointer << 32) | completed.len() as u64;
    let cancelled_packed = (cancelled_pointer << 32) | cancelled.len() as u64;
    let denied_packed = (denied_pointer << 32) | denied.len() as u64;
    let wat = format!(
        r#"(module $reference
            (import "keith:plugin/host@1.0.0" "emit_event"
                (func $emit_event (param i32 i32 i32 i32) (result i32)))
            (import "keith:plugin/host@1.0.0" "cancelled"
                (func $cancelled (param i32 i32) (result i32)))
            (memory (export "memory") 2 2)
            (global $heap (mut i32) (i32.const 32768))
            (data (i32.const 1024) "{cancellation_id}")
            (data (i32.const 2048) "{completed}")
            (data (i32.const 8192) "{cancelled}")
            (data (i32.const 12288) "{denied}")
            (data (i32.const 16384) "{event}")
            (func (export "keith_alloc") (param $length i32) (result i32)
                (local $pointer i32)
                global.get $heap
                local.tee $pointer
                local.get $length
                i32.add
                global.set $heap
                local.get $pointer)
            (func (export "keith_invoke") (param $request i32) (param $length i32) (result i64)
                local.get $length
                i32.eqz
                if
                    i64.const {denied_packed}
                    return
                end
                local.get $request
                i32.load8_u
                i32.const 123
                i32.ne
                if
                    i64.const {denied_packed}
                    return
                end
                i32.const 1024
                i32.const {cancellation_len}
                call $cancelled
                if
                    i64.const {cancelled_packed}
                    return
                end
                i32.const 16384
                i32.const {event_len}
                i32.const 24576
                i32.const 1024
                call $emit_event
                i32.const 0
                i32.lt_s
                if
                    i64.const {denied_packed}
                    return
                end
                i64.const {completed_packed})
        )"#,
        cancellation_id = wat_bytes(b"cancel-1"),
        cancellation_len = b"cancel-1".len(),
        completed = wat_bytes(&completed),
        cancelled = wat_bytes(&cancelled),
        denied = wat_bytes(&denied),
        event = wat_bytes(&event),
        event_len = event.len(),
    );
    wat::parse_str(wat).expect("reference component WAT")
}

fn reference_component() -> Vec<u8> {
    static COMPONENT: OnceLock<Vec<u8>> = OnceLock::new();
    COMPONENT.get_or_init(build_reference_component).clone()
}

fn build_reference_component() -> Vec<u8> {
    let fixture = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../plugin-sdk/reference-component/Cargo.toml");
    let target = std::env::var_os("CARGO_TARGET_DIR")
        .map_or_else(
            || PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../target"),
            PathBuf::from,
        )
        .join("reference-component-fixture");
    fs::create_dir_all(&target).expect("reference component target");
    let output = Command::new(env!("CARGO"))
        .args([
            "build",
            "--manifest-path",
            fixture.to_str().expect("UTF-8 component manifest"),
            "--lib",
            "--target",
            "wasm32-unknown-unknown",
            "--release",
            "--locked",
        ])
        .env("CARGO_INCREMENTAL", "0")
        .env("CARGO_TARGET_DIR", &target)
        .output()
        .expect("build reference component");
    assert!(
        output.status.success(),
        "reference component build failed: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    let componentizer = Command::new(env!("CARGO"))
        .args([
            "build",
            "--manifest-path",
            fixture.to_str().expect("UTF-8 component manifest"),
            "--bin",
            "componentize",
            "--release",
            "--locked",
        ])
        .env("CARGO_INCREMENTAL", "0")
        .env("CARGO_TARGET_DIR", &target)
        .output()
        .expect("build component encoder");
    assert!(
        componentizer.status.success(),
        "component encoder build failed: {}",
        String::from_utf8_lossy(&componentizer.stderr)
    );
    let core = target.join("wasm32-unknown-unknown/release/keith_reference_plugin_component.wasm");
    let component = target.join("reference-component.wasm");
    let encoded = Command::new(target.join("release/componentize"))
        .arg(core)
        .arg(&component)
        .output()
        .expect("componentize reference plugin");
    assert!(
        encoded.status.success(),
        "component encoding failed: {}",
        String::from_utf8_lossy(&encoded.stderr)
    );
    std::fs::read(component).expect("read reference component")
}

fn credential_logging_component(completed: &HostResponse) -> Vec<u8> {
    let completed = serde_json::to_vec(completed).expect("completed response");
    let credential = serde_json::to_vec(&keith_plugin_sdk::PluginHostCall::Credential {
        name: "provider-token".to_owned(),
    })
    .expect("credential request");
    let log = serde_json::to_vec(&keith_plugin_sdk::PluginHostCall::SafeLog {
        level: PluginLogLevel::Info,
        message: "credential top-secret must be hidden".to_owned(),
    })
    .expect("log request");
    let packed = (2_048_u64 << 32) | completed.len() as u64;
    let wat = format!(
        r#"(module $credential_logger
            (import "keith:plugin/host@1.0.0" "credential"
                (func $credential (param i32 i32 i32 i32) (result i32)))
            (import "keith:plugin/host@1.0.0" "safe_log"
                (func $safe_log (param i32 i32 i32 i32) (result i32)))
            (memory (export "memory") 2 2)
            (global $heap (mut i32) (i32.const 32768))
            (data (i32.const 2048) "{completed}")
            (data (i32.const 8192) "{credential}")
            (data (i32.const 12288) "{log}")
            (func (export "keith_alloc") (param $length i32) (result i32)
                (local $pointer i32)
                global.get $heap
                local.tee $pointer
                local.get $length
                i32.add
                global.set $heap
                local.get $pointer)
            (func (export "keith_invoke") (param i32 i32) (result i64)
                i32.const 8192
                i32.const {credential_len}
                i32.const 16384
                i32.const 1024
                call $credential
                drop
                i32.const 12288
                i32.const {log_len}
                i32.const 20480
                i32.const 1024
                call $safe_log
                drop
                i64.const {packed})
        )"#,
        completed = wat_bytes(&completed),
        credential = wat_bytes(&credential),
        credential_len = credential.len(),
        log = wat_bytes(&log),
        log_len = log.len(),
    );
    wat::parse_str(wat).expect("credential component WAT")
}

fn fault_component(memory_pages: u32, invoke_body: &str) -> Vec<u8> {
    wat::parse_str(format!(
        r#"(module $fault
            (memory (export "memory") {memory_pages} {memory_pages})
            (global $heap (mut i32) (i32.const 32768))
            (func (export "keith_alloc") (param $length i32) (result i32)
                (local $pointer i32)
                global.get $heap
                local.tee $pointer
                local.get $length
                i32.add
                global.set $heap
                local.get $pointer)
            (func (export "keith_invoke") (param i32 i32) (result i64)
                {invoke_body})
        )"#
    ))
    .expect("fault component WAT")
}

fn wat_bytes(bytes: &[u8]) -> String {
    let mut encoded = String::with_capacity(bytes.len() * 3);
    for byte in bytes {
        write!(encoded, r"\{byte:02x}").expect("write to string");
    }
    encoded
}

#[test]
fn abi_real_component_exchanges_typed_payload_stream_and_event() {
    let context = Arc::new(ReferenceContext::default());
    let component = reference_component();
    let executable = ExecutablePlugin::compile(manifest(true), component, context.clone())
        .expect("compile reference component");
    let output = executable.invoke(&request()).expect("typed invocation");
    assert_eq!(output.status, PluginStatus::Completed);
    assert_eq!(output.payload, br#"{"echo":"hello"}"#);
    assert_eq!(output.stream.len(), 1);
    assert_eq!(
        context.events.lock().expect("event lock").as_slice(),
        &[("progress".to_owned(), b"halfway".to_vec())]
    );
    assert_eq!(
        context.logs.lock().expect("log lock").as_slice(),
        &["credential [REDACTED] must be hidden".to_owned()]
    );
}

#[test]
fn abi_real_component_observes_cancellation_during_execution() {
    let context = Arc::new(ReferenceContext {
        cancel_on_check: Some(1),
        ..ReferenceContext::default()
    });
    let component = reference_component();
    let executable = ExecutablePlugin::compile(manifest(true), component, context)
        .expect("compile reference component");
    let output = executable.invoke(&request()).expect("cancelled invocation");
    assert_eq!(output.status, PluginStatus::Cancelled);
    assert!(output.stream.is_empty());
}

#[test]
fn abi_real_component_receives_explicit_grant_denial() {
    let context = Arc::new(ReferenceContext::default());
    let component = reference_component();
    let executable = ExecutablePlugin::compile(manifest(false), component, context.clone())
        .expect("compile reference component");
    let output = executable.invoke(&request()).expect("denied invocation");
    assert_eq!(output.status, PluginStatus::Denied);
    assert!(context.events.lock().expect("event lock").is_empty());
}

#[test]
fn abi_real_component_schema_validation_rejects_before_execution() {
    let context = Arc::new(ReferenceContext::default());
    let executable =
        ExecutablePlugin::compile(manifest(true), reference_component(), context.clone())
            .expect("compile reference component");
    let mut invalid = request();
    invalid.payload = br#"{"unexpected":true}"#.to_vec();
    assert!(executable.invoke(&invalid).is_err());
    assert!(context.events.lock().expect("event lock").is_empty());
}

#[test]
fn abi_named_credentials_are_redacted_before_safe_logging() {
    let context = Arc::new(ReferenceContext::default());
    let component = credential_logging_component(&response(PluginStatus::Completed, true));
    let executable = ExecutablePlugin::compile(manifest(true), component, context.clone())
        .expect("compile credential component");
    executable
        .invoke(&request())
        .expect("credential invocation");
    assert_eq!(
        context.logs.lock().expect("log lock").as_slice(),
        &["credential [REDACTED] must be hidden".to_owned()]
    );
}

#[test]
fn abi_ambient_wasi_import_is_rejected_instead_of_inherited() {
    let module = wat::parse_str(
        r#"(module
            (import "wasi_snapshot_preview1" "fd_write"
                (func (param i32 i32 i32 i32) (result i32)))
            (memory (export "memory") 1)
            (func (export "keith_alloc") (param i32) (result i32) i32.const 0)
            (func (export "keith_invoke") (param i32 i32) (result i64) i64.const 0))"#,
    )
    .expect("ambient module");
    let executable = ExecutablePlugin::compile(
        manifest(true),
        module,
        Arc::new(ReferenceContext::default()),
    )
    .expect("module compiles without receiving imports");
    assert!(executable.invoke(&request()).is_err());
}

#[test]
fn abi_resource_exhaustion_and_crash_are_isolated_from_later_invocations() {
    let context = Arc::new(ReferenceContext::default());
    let cases = [
        fault_component(2, "(loop $again br $again) i64.const 0"),
        fault_component(2, "unreachable"),
        fault_component(33, "i64.const 0"),
        fault_component(2, "i64.const 1000000"),
    ];
    for component in cases {
        let executable = ExecutablePlugin::compile(manifest(true), component, context.clone())
            .expect("fault component compiles");
        assert!(executable.invoke(&request()).is_err());
    }

    let healthy = reference_component();
    let executable = ExecutablePlugin::compile(manifest(true), healthy, context)
        .expect("healthy component compiles after faults");
    assert_eq!(
        executable
            .invoke(&request())
            .expect("healthy invocation")
            .status,
        PluginStatus::Completed
    );
}

#[test]
fn abi_legacy_core_wire_module_remains_compatible() {
    let context = Arc::new(ReferenceContext::default());
    let component = legacy_core_module(
        &response(PluginStatus::Completed, true),
        &response(PluginStatus::Cancelled, false),
        &response(PluginStatus::Denied, false),
    );
    let executable = ExecutablePlugin::compile(manifest(true), component, context)
        .expect("compile legacy wire component");
    assert_eq!(
        executable
            .invoke(&request())
            .expect("legacy invocation")
            .status,
        PluginStatus::Completed
    );
}
