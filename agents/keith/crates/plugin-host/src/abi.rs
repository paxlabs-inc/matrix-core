use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::{Arc, Condvar, Mutex};
use std::thread;
use std::time::Duration;

use keith_plugin_component_bindings as component_bindings;
use keith_plugin_sdk::{
    HostRequest, HostResponse, PayloadError, PayloadFormat, PluginGrant, PluginHostCall,
    PluginHostCallResult, PluginHttpRequest, PluginHttpResponse, PluginLogLevel, PluginManifest,
    PluginOperation, PluginRisk, PluginStatus, PluginStorageRequest, PluginStreamFrame,
    PluginToolDescriptor, decode_response, encode_request,
};
use thiserror::Error;
use url::Url;
use wasmparser::{Encoding, Parser, Payload, Validator};
use wasmtime::component::{Component, HasSelf, Linker as ComponentLinker};
use wasmtime::{
    Caller, Config, Engine, Extern, Linker, Memory, Module, Store, StoreLimits, StoreLimitsBuilder,
};

pub const ABI_IMPORT_MODULE: &str = "keith:plugin/host@1.0.0";
pub const ABI_MEMORY_EXPORT: &str = "memory";
pub const ABI_ALLOC_EXPORT: &str = "keith_alloc";
pub const ABI_INVOKE_EXPORT: &str = "keith_invoke";

const ABI_ENVELOPE_OVERHEAD: usize = 16 * 1_024;
const HOST_CALL_DENIED: i32 = -1;
const HOST_CALL_INVALID: i32 = -2;
const HOST_CALL_OUTPUT_TOO_LARGE: i32 = -3;
const HOST_CALL_FAILED: i32 = -4;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum HostCallKind {
    Http,
    Credential,
    Storage,
    EmitEvent,
    CreateArtifact,
    Clock,
    SafeLog,
}

#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
pub enum PluginHostCallError {
    #[error("plugin host capability was denied")]
    Denied,
    #[error("plugin host capability failed safely")]
    Failed,
}

#[allow(clippy::missing_errors_doc)]
pub trait PluginHostContext: Send + Sync {
    fn http(
        &self,
        _request: &PluginHttpRequest,
    ) -> Result<PluginHttpResponse, PluginHostCallError> {
        Err(PluginHostCallError::Denied)
    }

    fn credential(&self, _name: &str) -> Result<Vec<u8>, PluginHostCallError> {
        Err(PluginHostCallError::Denied)
    }

    fn storage(
        &self,
        _request: &PluginStorageRequest,
    ) -> Result<Option<Vec<u8>>, PluginHostCallError> {
        Err(PluginHostCallError::Denied)
    }

    fn emit_event(&self, _topic: &str, _payload: &[u8]) -> Result<(), PluginHostCallError> {
        Err(PluginHostCallError::Denied)
    }

    fn create_artifact(
        &self,
        _name: &str,
        _media_type: &str,
        _payload: &[u8],
    ) -> Result<String, PluginHostCallError> {
        Err(PluginHostCallError::Denied)
    }

    fn now_millis(&self) -> Result<u64, PluginHostCallError> {
        Err(PluginHostCallError::Denied)
    }

    fn safe_log(&self, _level: PluginLogLevel, _message: &str) -> Result<(), PluginHostCallError> {
        Err(PluginHostCallError::Denied)
    }

    fn cancelled(&self, _cancellation_id: &str) -> bool {
        false
    }
}

#[derive(Default)]
pub struct DenyAllPluginHostContext;

impl PluginHostContext for DenyAllPluginHostContext {}

struct StoreState {
    limits: StoreLimits,
    manifest: PluginManifest,
    context: Arc<dyn PluginHostContext>,
    cancellation_id: String,
    secrets: Vec<Vec<u8>>,
}

pub struct ExecutablePlugin {
    manifest: PluginManifest,
    module_bytes: Arc<[u8]>,
    is_component: bool,
    context: Arc<dyn PluginHostContext>,
    active_calls: Arc<AtomicU32>,
}

impl ExecutablePlugin {
    /// # Errors
    ///
    /// Returns an error when the manifest or WebAssembly component is invalid.
    pub fn compile(
        manifest: PluginManifest,
        module_bytes: impl Into<Arc<[u8]>>,
        context: Arc<dyn PluginHostContext>,
    ) -> Result<Self, PluginAbiError> {
        manifest.validate()?;
        let module_bytes = module_bytes.into();
        let engine = bounded_engine()?;
        let is_component = binary_encoding(&module_bytes)? == Encoding::Component;
        if is_component {
            Component::from_binary(&engine, &module_bytes)
                .map_err(|error| PluginAbiError::InvalidModule(error.to_string()))?;
        } else {
            Module::from_binary(&engine, &module_bytes)
                .map_err(|error| PluginAbiError::InvalidModule(error.to_string()))?;
        }
        Ok(Self {
            manifest,
            module_bytes,
            is_component,
            context,
            active_calls: Arc::new(AtomicU32::new(0)),
        })
    }

    pub const fn manifest(&self) -> &PluginManifest {
        &self.manifest
    }

    /// Validates that the package exposes the typed Keith component contract and that its
    /// self-described tools and commands exactly match the signed manifest.
    ///
    /// # Errors
    ///
    /// Returns an error for legacy core modules, incompatible component imports or exports,
    /// descriptor drift, resource exhaustion, or traps during bounded component discovery.
    pub fn validate_contract(&self) -> Result<(), PluginAbiError> {
        if !self.is_component {
            return Err(PluginAbiError::InvalidAbi(
                "plugin authority requires the typed component ABI".to_owned(),
            ));
        }

        let engine = bounded_engine()?;
        let component = Component::from_binary(&engine, &self.module_bytes)
            .map_err(|error| PluginAbiError::InvalidModule(error.to_string()))?;
        let mut linker = ComponentLinker::new(&engine);
        component_bindings::Plugin::add_to_linker::<_, HasSelf<_>>(&mut linker, |state| state)
            .map_err(|error| PluginAbiError::InvalidAbi(error.to_string()))?;
        let limits = StoreLimitsBuilder::new()
            .memory_size(self.manifest.grants.max_memory_bytes)
            .instances(16)
            .memories(8)
            .build();
        let mut store = Store::new(
            &engine,
            StoreState {
                limits,
                manifest: self.manifest.clone(),
                context: Arc::clone(&self.context),
                cancellation_id: "authority-contract-validation".to_owned(),
                secrets: Vec::new(),
            },
        );
        store.limiter(|state| &mut state.limits);
        store
            .set_fuel(self.manifest.grants.max_fuel)
            .map_err(|error| PluginAbiError::Invocation(error.to_string()))?;
        store.set_epoch_deadline(1);

        let timeout = Duration::from_millis(self.manifest.grants.max_wall_time_ms);
        let timer = InvocationTimer::start(engine, timeout);
        let result = (|| {
            let bindings = component_bindings::Plugin::instantiate(&mut store, &component, &linker)
                .map_err(|error| PluginAbiError::InvalidAbi(error.to_string()))?;
            let guest = bindings.keith_plugin_guest();
            let tools = guest
                .call_describe_tools(&mut store)
                .map_err(|error| PluginAbiError::Invocation(error.to_string()))?;
            let commands = guest
                .call_describe_commands(&mut store)
                .map_err(|error| PluginAbiError::Invocation(error.to_string()))?;
            validate_component_descriptors(&self.manifest, tools, commands)
        })();
        timer.finish();
        result
    }

    /// # Errors
    ///
    /// Returns an error for invalid payloads, ABI violations, limits, traps, or secret leakage.
    pub fn invoke(&self, request: &HostRequest) -> Result<HostResponse, PluginAbiError> {
        request.validate(&self.manifest)?;
        let concurrency_limit = request
            .target
            .as_deref()
            .and_then(|target| self.manifest.descriptor(request.operation, target))
            .map_or(self.manifest.grants.max_concurrent_calls, |descriptor| {
                descriptor
                    .concurrency_limit
                    .min(self.manifest.grants.max_concurrent_calls)
            });
        let _permit = InvocationPermit::acquire(&self.active_calls, concurrency_limit)?;
        if self.context.cancelled(&request.cancellation_id) {
            return Ok(cancelled_response(request));
        }

        let timeout = request
            .target
            .as_deref()
            .and_then(|target| self.manifest.descriptor(request.operation, target))
            .map_or(self.manifest.grants.max_wall_time_ms, |descriptor| {
                descriptor
                    .timeout_ms
                    .min(self.manifest.grants.max_wall_time_ms)
            });
        if self.is_component {
            return self.invoke_component(request, Duration::from_millis(timeout));
        }
        self.invoke_core(request, Duration::from_millis(timeout))
    }

    fn invoke_core(
        &self,
        request: &HostRequest,
        timeout: Duration,
    ) -> Result<HostResponse, PluginAbiError> {
        let engine = bounded_engine()?;
        let executable = executable_core(&self.module_bytes)?;
        let module = Module::from_binary(&engine, executable)
            .map_err(|error| PluginAbiError::InvalidModule(error.to_string()))?;
        let mut linker = Linker::new(&engine);
        link_host_calls(&mut linker)?;

        let limits = StoreLimitsBuilder::new()
            .memory_size(self.manifest.grants.max_memory_bytes)
            .instances(1)
            .memories(1)
            .build();
        let mut store = Store::new(
            &engine,
            StoreState {
                limits,
                manifest: self.manifest.clone(),
                context: Arc::clone(&self.context),
                cancellation_id: request.cancellation_id.clone(),
                secrets: Vec::new(),
            },
        );
        store.limiter(|state| &mut state.limits);
        store
            .set_fuel(self.manifest.grants.max_fuel)
            .map_err(|error| PluginAbiError::Invocation(error.to_string()))?;
        store.set_epoch_deadline(1);

        let instance = linker
            .instantiate(&mut store, &module)
            .map_err(|error| PluginAbiError::InvalidAbi(error.to_string()))?;
        let memory = instance
            .get_memory(&mut store, ABI_MEMORY_EXPORT)
            .ok_or(PluginAbiError::MissingExport(ABI_MEMORY_EXPORT))?;
        let allocate = instance
            .get_typed_func::<i32, i32>(&mut store, ABI_ALLOC_EXPORT)
            .map_err(|_| PluginAbiError::MissingExport(ABI_ALLOC_EXPORT))?;
        let invoke = instance
            .get_typed_func::<(i32, i32), i64>(&mut store, ABI_INVOKE_EXPORT)
            .map_err(|_| PluginAbiError::MissingExport(ABI_INVOKE_EXPORT))?;

        let request_bound = wire_bound(self.manifest.grants.max_input_bytes)?;
        let encoded = encode_request(request, request_bound)?;
        let request_len =
            i32::try_from(encoded.len()).map_err(|_| PluginAbiError::InputTooLarge)?;
        let request_ptr = allocate
            .call(&mut store, request_len)
            .map_err(|error| PluginAbiError::Invocation(error.to_string()))?;
        if request_ptr < 0 {
            return Err(PluginAbiError::InvalidPointer);
        }
        let request_offset =
            usize::try_from(request_ptr).map_err(|_| PluginAbiError::InvalidPointer)?;
        memory
            .write(&mut store, request_offset, &encoded)
            .map_err(|_| PluginAbiError::InvalidPointer)?;

        let timer = InvocationTimer::start(engine.clone(), timeout);
        let packed = invoke
            .call(&mut store, (request_ptr, request_len))
            .map_err(|error| PluginAbiError::Invocation(error.to_string()));
        timer.finish();
        let packed = packed?;

        let packed = packed.cast_unsigned();
        let response_ptr =
            usize::try_from(packed >> 32).map_err(|_| PluginAbiError::InvalidPointer)?;
        let response_len = usize::try_from(packed & u64::from(u32::MAX))
            .map_err(|_| PluginAbiError::OutputTooLarge)?;
        let response_bound = wire_bound(self.manifest.grants.max_output_bytes)?;
        if response_len == 0 || response_len > response_bound {
            return Err(PluginAbiError::OutputTooLarge);
        }
        let mut bytes = vec![0; response_len];
        memory
            .read(&store, response_ptr, &mut bytes)
            .map_err(|_| PluginAbiError::InvalidPointer)?;
        let response = decode_response(&bytes, response_bound)?;
        response.validate(request, &self.manifest)?;

        if response_contains_secret(&response, &store.data().secrets) {
            return Err(PluginAbiError::SecretLeak);
        }
        if self.context.cancelled(&request.cancellation_id)
            && response.status != PluginStatus::Cancelled
        {
            return Ok(cancelled_response(request));
        }
        Ok(response)
    }

    fn invoke_component(
        &self,
        request: &HostRequest,
        timeout: Duration,
    ) -> Result<HostResponse, PluginAbiError> {
        let engine = bounded_engine()?;
        let component = Component::from_binary(&engine, &self.module_bytes)
            .map_err(|error| PluginAbiError::InvalidModule(error.to_string()))?;
        let mut linker = ComponentLinker::new(&engine);
        component_bindings::Plugin::add_to_linker::<_, HasSelf<_>>(&mut linker, |state| state)
            .map_err(|error| PluginAbiError::InvalidAbi(error.to_string()))?;

        let limits = StoreLimitsBuilder::new()
            .memory_size(self.manifest.grants.max_memory_bytes)
            .instances(16)
            .memories(8)
            .build();
        let mut store = Store::new(
            &engine,
            StoreState {
                limits,
                manifest: self.manifest.clone(),
                context: Arc::clone(&self.context),
                cancellation_id: request.cancellation_id.clone(),
                secrets: Vec::new(),
            },
        );
        store.limiter(|state| &mut state.limits);
        store
            .set_fuel(self.manifest.grants.max_fuel)
            .map_err(|error| PluginAbiError::Invocation(error.to_string()))?;
        store.set_epoch_deadline(1);

        let bindings = component_bindings::Plugin::instantiate(&mut store, &component, &linker)
            .map_err(|error| PluginAbiError::InvalidAbi(error.to_string()))?;
        let guest = bindings.keith_plugin_guest();
        let declared_tools = guest
            .call_describe_tools(&mut store)
            .map_err(|error| PluginAbiError::Invocation(error.to_string()))?;
        let declared_commands = guest
            .call_describe_commands(&mut store)
            .map_err(|error| PluginAbiError::Invocation(error.to_string()))?;
        validate_component_descriptors(&self.manifest, declared_tools, declared_commands)?;

        let invocation = component_invocation(request);
        let timer = InvocationTimer::start(engine, timeout);
        let response = guest
            .call_invoke(&mut store, &invocation)
            .map_err(|error| error.to_string());
        timer.finish();
        let response = response
            .map_err(|error| PluginAbiError::Invocation(redact(&error, &store.data().secrets)))?;
        let response = host_response(response);
        response.validate(request, &self.manifest)?;
        if response_contains_secret(&response, &store.data().secrets) {
            return Err(PluginAbiError::SecretLeak);
        }
        if self.context.cancelled(&request.cancellation_id)
            && response.status != PluginStatus::Cancelled
        {
            return Ok(cancelled_response(request));
        }
        Ok(response)
    }
}

#[derive(Debug, Error)]
pub enum PluginAbiError {
    #[error("plugin manifest is invalid: {0}")]
    Manifest(#[from] keith_plugin_sdk::ManifestError),
    #[error("plugin payload is invalid: {0}")]
    Payload(#[from] PayloadError),
    #[error("plugin module is invalid: {0}")]
    InvalidModule(String),
    #[error("plugin executable ABI is invalid: {0}")]
    InvalidAbi(String),
    #[error("plugin invocation failed within its sandbox: {0}")]
    Invocation(String),
    #[error("plugin is at its declared concurrent-call limit")]
    Busy,
    #[error("plugin input exceeds its declared bound")]
    InputTooLarge,
    #[error("plugin output exceeds its declared bound")]
    OutputTooLarge,
    #[error("plugin returned an invalid memory pointer")]
    InvalidPointer,
    #[error("plugin is missing required export {0}")]
    MissingExport(&'static str),
    #[error("plugin attempted to expose a named credential in its result")]
    SecretLeak,
}

struct InvocationPermit {
    active_calls: Arc<AtomicU32>,
}

impl InvocationPermit {
    fn acquire(active_calls: &Arc<AtomicU32>, limit: u32) -> Result<Self, PluginAbiError> {
        active_calls
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |active| {
                (active < limit).then_some(active + 1)
            })
            .map_err(|_| PluginAbiError::Busy)?;
        Ok(Self {
            active_calls: Arc::clone(active_calls),
        })
    }
}

impl Drop for InvocationPermit {
    fn drop(&mut self) {
        self.active_calls.fetch_sub(1, Ordering::AcqRel);
    }
}

struct InvocationTimer {
    state: Arc<(Mutex<bool>, Condvar)>,
    thread: thread::JoinHandle<()>,
}

impl InvocationTimer {
    fn start(engine: Engine, timeout: Duration) -> Self {
        let state = Arc::new((Mutex::new(false), Condvar::new()));
        let timer_state = Arc::clone(&state);
        let thread = thread::spawn(move || {
            let (lock, wake) = &*timer_state;
            let finished = lock
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            let (finished, timeout_result) = wake
                .wait_timeout_while(finished, timeout, |finished| !*finished)
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            if !*finished && timeout_result.timed_out() {
                engine.increment_epoch();
            }
        });
        Self { state, thread }
    }

    fn finish(self) {
        let (lock, wake) = &*self.state;
        let mut finished = lock
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        *finished = true;
        wake.notify_one();
        drop(finished);
        let _ = self.thread.join();
    }
}

fn bounded_engine() -> Result<Engine, PluginAbiError> {
    let mut config = Config::new();
    config.consume_fuel(true);
    config.epoch_interruption(true);
    Engine::new(&config).map_err(|error| PluginAbiError::InvalidModule(error.to_string()))
}

fn binary_encoding(bytes: &[u8]) -> Result<Encoding, PluginAbiError> {
    match Parser::new(0).parse_all(bytes).next() {
        Some(Ok(Payload::Version { encoding, .. })) => Ok(encoding),
        Some(Err(error)) => Err(PluginAbiError::InvalidModule(error.to_string())),
        _ => Err(PluginAbiError::InvalidModule(
            "missing WebAssembly header".to_owned(),
        )),
    }
}

fn executable_core(bytes: &[u8]) -> Result<&[u8], PluginAbiError> {
    let mut payloads = Parser::new(0).parse_all(bytes);
    let encoding = binary_encoding(bytes)?;
    let _ = payloads.next();
    if encoding == Encoding::Module {
        return Ok(bytes);
    }
    Validator::new()
        .validate_all(bytes)
        .map_err(|error| PluginAbiError::InvalidModule(error.to_string()))?;
    for payload in payloads {
        if let Payload::ModuleSection {
            unchecked_range, ..
        } = payload.map_err(|error| PluginAbiError::InvalidModule(error.to_string()))?
        {
            return bytes.get(unchecked_range).ok_or_else(|| {
                PluginAbiError::InvalidModule("component core module is out of bounds".to_owned())
            });
        }
    }
    Err(PluginAbiError::InvalidModule(
        "component does not contain an executable core module".to_owned(),
    ))
}

fn link_host_calls(linker: &mut Linker<StoreState>) -> Result<(), PluginAbiError> {
    let calls = [
        ("http", HostCallKind::Http),
        ("credential", HostCallKind::Credential),
        ("storage", HostCallKind::Storage),
        ("emit_event", HostCallKind::EmitEvent),
        ("create_artifact", HostCallKind::CreateArtifact),
        ("clock", HostCallKind::Clock),
        ("safe_log", HostCallKind::SafeLog),
    ];
    for (name, kind) in calls {
        linker
            .func_wrap(
                ABI_IMPORT_MODULE,
                name,
                move |mut caller: Caller<'_, StoreState>,
                      request_ptr: i32,
                      request_len: i32,
                      response_ptr: i32,
                      response_capacity: i32|
                      -> i32 {
                    dispatch_host_call(
                        &mut caller,
                        kind,
                        request_ptr,
                        request_len,
                        response_ptr,
                        response_capacity,
                    )
                },
            )
            .map_err(|error| PluginAbiError::InvalidAbi(error.to_string()))?;
    }
    linker
        .func_wrap(
            ABI_IMPORT_MODULE,
            "cancelled",
            |mut caller: Caller<'_, StoreState>, pointer: i32, length: i32| -> i32 {
                let Ok(cancellation_id) = read_guest_string(&mut caller, pointer, length) else {
                    return 1;
                };
                i32::from(
                    cancellation_id != caller.data().cancellation_id
                        || caller.data().context.cancelled(&cancellation_id),
                )
            },
        )
        .map_err(|error| PluginAbiError::InvalidAbi(error.to_string()))?;
    Ok(())
}

fn dispatch_host_call(
    caller: &mut Caller<'_, StoreState>,
    expected: HostCallKind,
    request_ptr: i32,
    request_len: i32,
    response_ptr: i32,
    response_capacity: i32,
) -> i32 {
    let bound = caller.data().manifest.grants.max_host_call_bytes;
    let Ok(request) = read_guest_bytes(caller, request_ptr, request_len, bound) else {
        return HOST_CALL_INVALID;
    };
    let Ok(call) = serde_json::from_slice::<PluginHostCall>(&request) else {
        return HOST_CALL_INVALID;
    };
    if !call_matches(expected, &call) {
        return HOST_CALL_INVALID;
    }
    let result = match execute_host_call(caller.data_mut(), call) {
        Ok(result) => result,
        Err(PluginHostCallError::Denied) => return HOST_CALL_DENIED,
        Err(PluginHostCallError::Failed) => return HOST_CALL_FAILED,
    };
    let Ok(response) = serde_json::to_vec(&result) else {
        return HOST_CALL_FAILED;
    };
    let Ok(response_capacity) = usize::try_from(response_capacity) else {
        return HOST_CALL_OUTPUT_TOO_LARGE;
    };
    if response.len() > bound || response.len() > response_capacity {
        return HOST_CALL_OUTPUT_TOO_LARGE;
    }
    if write_guest_bytes(caller, response_ptr, &response).is_err() {
        return HOST_CALL_INVALID;
    }
    i32::try_from(response.len()).unwrap_or(HOST_CALL_OUTPUT_TOO_LARGE)
}

fn execute_host_call(
    state: &mut StoreState,
    call: PluginHostCall,
) -> Result<PluginHostCallResult, PluginHostCallError> {
    let context = Arc::clone(&state.context);
    let grants = state.manifest.grants.clone();
    match call {
        PluginHostCall::Http { request } => {
            let url = Url::parse(&request.url).map_err(|_| PluginHostCallError::Denied)?;
            let host = url.host_str().ok_or(PluginHostCallError::Denied)?;
            if url.scheme() != "https"
                || !grants.allows(&PluginGrant::HttpHost(host.to_owned()))
                || request.body.len() > grants.max_host_call_bytes
            {
                return Err(PluginHostCallError::Denied);
            }
            let response = context.http(&request)?;
            if response.body.len() > grants.max_host_call_bytes {
                return Err(PluginHostCallError::Failed);
            }
            Ok(PluginHostCallResult::Http(response))
        }
        PluginHostCall::Credential { name } => {
            if !grants.allows(&PluginGrant::Credential(name.clone())) {
                return Err(PluginHostCallError::Denied);
            }
            let secret = context.credential(&name)?;
            if secret.is_empty() || secret.len() > grants.max_host_call_bytes {
                return Err(PluginHostCallError::Failed);
            }
            state.secrets.push(secret.clone());
            Ok(PluginHostCallResult::Credential(secret))
        }
        PluginHostCall::Storage { request } => {
            let grant = if request.requires_write() {
                PluginGrant::StorageWrite(request.namespace().to_owned())
            } else {
                PluginGrant::StorageRead(request.namespace().to_owned())
            };
            if !grants.allows(&grant)
                || matches!(&request, PluginStorageRequest::Put { value, .. } if value.len() > grants.max_storage_bytes)
            {
                return Err(PluginHostCallError::Denied);
            }
            let value = context.storage(&request)?;
            if value
                .as_ref()
                .is_some_and(|value| value.len() > grants.max_storage_bytes)
            {
                return Err(PluginHostCallError::Failed);
            }
            Ok(PluginHostCallResult::Storage(value))
        }
        PluginHostCall::EmitEvent { topic, payload } => {
            if !grants.allows(&PluginGrant::EmitEvent)
                || topic.is_empty()
                || topic.len() > 256
                || payload.len() > grants.max_host_call_bytes
            {
                return Err(PluginHostCallError::Denied);
            }
            context.emit_event(&topic, &payload)?;
            Ok(PluginHostCallResult::Empty)
        }
        PluginHostCall::CreateArtifact {
            name,
            media_type,
            payload,
        } => {
            if !grants.allows(&PluginGrant::CreateArtifact)
                || name.is_empty()
                || name.len() > 256
                || media_type.is_empty()
                || media_type.len() > 256
                || payload.len() > grants.max_host_call_bytes
            {
                return Err(PluginHostCallError::Denied);
            }
            let id = context.create_artifact(&name, &media_type, &payload)?;
            if id.is_empty() || id.len() > 256 {
                return Err(PluginHostCallError::Failed);
            }
            Ok(PluginHostCallResult::Artifact(id))
        }
        PluginHostCall::Clock => {
            if !grants.allows(&PluginGrant::Clock) {
                return Err(PluginHostCallError::Denied);
            }
            context.now_millis().map(PluginHostCallResult::Clock)
        }
        PluginHostCall::SafeLog { level, message } => {
            if !grants.allows(&PluginGrant::SafeLog) || message.len() > 4_096 {
                return Err(PluginHostCallError::Denied);
            }
            let redacted = redact(&message, &state.secrets);
            context.safe_log(level, &redacted)?;
            Ok(PluginHostCallResult::Empty)
        }
    }
}

impl component_bindings::keith::plugin::types::Host for StoreState {}

impl component_bindings::keith::plugin::host::Host for StoreState {
    fn http(
        &mut self,
        request: component_bindings::keith::plugin::host::HttpRequest,
    ) -> Result<component_bindings::keith::plugin::host::HttpResponse, String> {
        let request = PluginHttpRequest {
            method: request.method,
            url: request.url,
            headers: request.headers.into_iter().collect(),
            body: request.body,
        };
        match execute_host_call(self, PluginHostCall::Http { request })
            .map_err(component_host_error)?
        {
            PluginHostCallResult::Http(response) => {
                Ok(component_bindings::keith::plugin::host::HttpResponse {
                    status: response.status,
                    headers: response.headers.into_iter().collect(),
                    body: response.body,
                })
            }
            _ => Err("host returned an invalid HTTP result".to_owned()),
        }
    }

    fn credential(&mut self, name: String) -> Result<Vec<u8>, String> {
        match execute_host_call(self, PluginHostCall::Credential { name })
            .map_err(component_host_error)?
        {
            PluginHostCallResult::Credential(secret) => Ok(secret),
            _ => Err("host returned an invalid credential result".to_owned()),
        }
    }

    fn storage(
        &mut self,
        namespace: String,
        operation: component_bindings::keith::plugin::host::StorageOperation,
    ) -> Result<Option<Vec<u8>>, String> {
        let request = match operation {
            component_bindings::keith::plugin::host::StorageOperation::Get(key) => {
                PluginStorageRequest::Get { namespace, key }
            }
            component_bindings::keith::plugin::host::StorageOperation::Put((key, value)) => {
                PluginStorageRequest::Put {
                    namespace,
                    key,
                    value,
                }
            }
            component_bindings::keith::plugin::host::StorageOperation::Delete(key) => {
                PluginStorageRequest::Delete { namespace, key }
            }
        };
        match execute_host_call(self, PluginHostCall::Storage { request })
            .map_err(component_host_error)?
        {
            PluginHostCallResult::Storage(value) => Ok(value),
            _ => Err("host returned an invalid storage result".to_owned()),
        }
    }

    fn emit_event(&mut self, topic: String, payload: Vec<u8>) -> Result<(), String> {
        match execute_host_call(self, PluginHostCall::EmitEvent { topic, payload })
            .map_err(component_host_error)?
        {
            PluginHostCallResult::Empty => Ok(()),
            _ => Err("host returned an invalid event result".to_owned()),
        }
    }

    fn create_artifact(
        &mut self,
        name: String,
        media_type: String,
        payload: Vec<u8>,
    ) -> Result<String, String> {
        match execute_host_call(
            self,
            PluginHostCall::CreateArtifact {
                name,
                media_type,
                payload,
            },
        )
        .map_err(component_host_error)?
        {
            PluginHostCallResult::Artifact(id) => Ok(id),
            _ => Err("host returned an invalid artifact result".to_owned()),
        }
    }

    fn now_millis(&mut self) -> Result<u64, String> {
        match execute_host_call(self, PluginHostCall::Clock).map_err(component_host_error)? {
            PluginHostCallResult::Clock(value) => Ok(value),
            _ => Err("host returned an invalid clock result".to_owned()),
        }
    }

    fn log(
        &mut self,
        level: component_bindings::keith::plugin::host::LogLevel,
        message: String,
    ) -> Result<(), String> {
        let level = match level {
            component_bindings::keith::plugin::host::LogLevel::Error => PluginLogLevel::Error,
            component_bindings::keith::plugin::host::LogLevel::Warn => PluginLogLevel::Warn,
            component_bindings::keith::plugin::host::LogLevel::Info => PluginLogLevel::Info,
            component_bindings::keith::plugin::host::LogLevel::Debug => PluginLogLevel::Debug,
        };
        match execute_host_call(self, PluginHostCall::SafeLog { level, message })
            .map_err(component_host_error)?
        {
            PluginHostCallResult::Empty => Ok(()),
            _ => Err("host returned an invalid log result".to_owned()),
        }
    }

    fn cancelled(&mut self, cancellation_id: String) -> bool {
        cancellation_id != self.cancellation_id || self.context.cancelled(&cancellation_id)
    }
}

fn component_host_error(error: PluginHostCallError) -> String {
    match error {
        PluginHostCallError::Denied => "denied".to_owned(),
        PluginHostCallError::Failed => "failed".to_owned(),
    }
}

fn component_invocation(
    request: &HostRequest,
) -> component_bindings::keith::plugin::types::Invocation {
    component_bindings::keith::plugin::types::Invocation {
        interface_version: request.interface_version,
        invocation_id: request.invocation_id.clone(),
        operation: match request.operation {
            PluginOperation::Activate => {
                component_bindings::keith::plugin::types::Operation::Activate
            }
            PluginOperation::Health => component_bindings::keith::plugin::types::Operation::Health,
            PluginOperation::Command => {
                component_bindings::keith::plugin::types::Operation::Command
            }
            PluginOperation::Tool => component_bindings::keith::plugin::types::Operation::Tool,
            PluginOperation::Migrate => {
                component_bindings::keith::plugin::types::Operation::Migrate
            }
            PluginOperation::Deactivate => {
                component_bindings::keith::plugin::types::Operation::Deactivate
            }
        },
        target: request.target.clone(),
        payload_format: component_payload_format(request.payload_format),
        payload: request.payload.clone(),
        cancellation_id: request.cancellation_id.clone(),
    }
}

fn host_response(response: component_bindings::keith::plugin::types::Response) -> HostResponse {
    HostResponse {
        interface_version: response.interface_version,
        invocation_id: response.invocation_id,
        status: match response.status {
            component_bindings::keith::plugin::types::Status::Completed => PluginStatus::Completed,
            component_bindings::keith::plugin::types::Status::Cancelled => PluginStatus::Cancelled,
            component_bindings::keith::plugin::types::Status::Denied => PluginStatus::Denied,
            component_bindings::keith::plugin::types::Status::Failed => PluginStatus::Failed,
        },
        payload_format: host_payload_format(response.payload_format),
        payload: response.payload,
        stream: response
            .frames
            .into_iter()
            .map(|frame| PluginStreamFrame {
                sequence: frame.sequence,
                payload_format: host_payload_format(frame.payload_format),
                payload: frame.payload,
            })
            .collect(),
        safe_error: response.safe_error,
    }
}

fn component_payload_format(
    format: PayloadFormat,
) -> component_bindings::keith::plugin::types::PayloadFormat {
    match format {
        PayloadFormat::Json => component_bindings::keith::plugin::types::PayloadFormat::Json,
        PayloadFormat::Bytes => component_bindings::keith::plugin::types::PayloadFormat::Bytes,
    }
}

fn host_payload_format(
    format: component_bindings::keith::plugin::types::PayloadFormat,
) -> PayloadFormat {
    match format {
        component_bindings::keith::plugin::types::PayloadFormat::Json => PayloadFormat::Json,
        component_bindings::keith::plugin::types::PayloadFormat::Bytes => PayloadFormat::Bytes,
    }
}

fn validate_component_descriptors(
    manifest: &PluginManifest,
    tools: Vec<component_bindings::keith::plugin::types::CallableDescriptor>,
    commands: Vec<component_bindings::keith::plugin::types::CallableDescriptor>,
) -> Result<(), PluginAbiError> {
    let tools = tools.into_iter().map(host_descriptor).collect::<Vec<_>>();
    let commands = commands
        .into_iter()
        .map(host_descriptor)
        .collect::<Vec<_>>();
    if tools != manifest.tools || commands != manifest.commands {
        return Err(PluginAbiError::InvalidAbi(
            "component descriptors do not match the signed manifest".to_owned(),
        ));
    }
    Ok(())
}

fn host_descriptor(
    descriptor: component_bindings::keith::plugin::types::CallableDescriptor,
) -> PluginToolDescriptor {
    PluginToolDescriptor {
        name: descriptor.name,
        description: descriptor.description,
        input_schema: descriptor.input_schema,
        output_schema: descriptor.output_schema,
        risk: match descriptor.risk {
            component_bindings::keith::plugin::types::Risk::ReadOnly => PluginRisk::ReadOnly,
            component_bindings::keith::plugin::types::Risk::Reversible => PluginRisk::Reversible,
            component_bindings::keith::plugin::types::Risk::Consequential => {
                PluginRisk::Consequential
            }
            component_bindings::keith::plugin::types::Risk::Irreversible => {
                PluginRisk::Irreversible
            }
        },
        timeout_ms: descriptor.timeout_ms,
        supports_cancellation: descriptor.supports_cancellation,
        streaming: descriptor.streaming,
        concurrency_limit: descriptor.concurrency_limit,
        required_grants: descriptor
            .required_grants
            .into_iter()
            .map(host_grant)
            .collect(),
    }
}

fn host_grant(grant: component_bindings::keith::plugin::types::Grant) -> PluginGrant {
    match grant {
        component_bindings::keith::plugin::types::Grant::HttpHost(host) => {
            PluginGrant::HttpHost(host)
        }
        component_bindings::keith::plugin::types::Grant::Credential(name) => {
            PluginGrant::Credential(name)
        }
        component_bindings::keith::plugin::types::Grant::StorageRead(namespace) => {
            PluginGrant::StorageRead(namespace)
        }
        component_bindings::keith::plugin::types::Grant::StorageWrite(namespace) => {
            PluginGrant::StorageWrite(namespace)
        }
        component_bindings::keith::plugin::types::Grant::EmitEvent => PluginGrant::EmitEvent,
        component_bindings::keith::plugin::types::Grant::CreateArtifact => {
            PluginGrant::CreateArtifact
        }
        component_bindings::keith::plugin::types::Grant::Clock => PluginGrant::Clock,
        component_bindings::keith::plugin::types::Grant::SafeLog => PluginGrant::SafeLog,
    }
}

const fn call_matches(expected: HostCallKind, call: &PluginHostCall) -> bool {
    matches!(
        (expected, call),
        (HostCallKind::Http, PluginHostCall::Http { .. })
            | (HostCallKind::Credential, PluginHostCall::Credential { .. })
            | (HostCallKind::Storage, PluginHostCall::Storage { .. })
            | (HostCallKind::EmitEvent, PluginHostCall::EmitEvent { .. })
            | (
                HostCallKind::CreateArtifact,
                PluginHostCall::CreateArtifact { .. }
            )
            | (HostCallKind::Clock, PluginHostCall::Clock)
            | (HostCallKind::SafeLog, PluginHostCall::SafeLog { .. })
    )
}

fn read_guest_string(
    caller: &mut Caller<'_, StoreState>,
    pointer: i32,
    length: i32,
) -> Result<String, ()> {
    let bytes = read_guest_bytes(
        caller,
        pointer,
        length,
        caller.data().manifest.grants.max_host_call_bytes,
    )?;
    String::from_utf8(bytes).map_err(|_| ())
}

fn read_guest_bytes(
    caller: &mut Caller<'_, StoreState>,
    pointer: i32,
    length: i32,
    bound: usize,
) -> Result<Vec<u8>, ()> {
    let (Ok(pointer), Ok(length)) = (usize::try_from(pointer), usize::try_from(length)) else {
        return Err(());
    };
    if length > bound {
        return Err(());
    }
    let memory = guest_memory(caller)?;
    let mut bytes = vec![0; length];
    memory.read(&*caller, pointer, &mut bytes).map_err(|_| ())?;
    Ok(bytes)
}

fn write_guest_bytes(
    caller: &mut Caller<'_, StoreState>,
    pointer: i32,
    bytes: &[u8],
) -> Result<(), ()> {
    let pointer = usize::try_from(pointer).map_err(|_| ())?;
    let memory = guest_memory(caller)?;
    memory.write(caller, pointer, bytes).map_err(|_| ())
}

fn guest_memory(caller: &mut Caller<'_, StoreState>) -> Result<Memory, ()> {
    caller
        .get_export(ABI_MEMORY_EXPORT)
        .and_then(Extern::into_memory)
        .ok_or(())
}

fn wire_bound(payload_bound: usize) -> Result<usize, PluginAbiError> {
    payload_bound
        .checked_add(ABI_ENVELOPE_OVERHEAD)
        .ok_or(PluginAbiError::OutputTooLarge)
}

fn cancelled_response(request: &HostRequest) -> HostResponse {
    HostResponse {
        interface_version: request.interface_version,
        invocation_id: request.invocation_id.clone(),
        status: PluginStatus::Cancelled,
        payload_format: PayloadFormat::Json,
        payload: b"null".to_vec(),
        stream: Vec::new(),
        safe_error: None,
    }
}

fn response_contains_secret(response: &HostResponse, secrets: &[Vec<u8>]) -> bool {
    secrets
        .iter()
        .filter(|secret| !secret.is_empty())
        .any(|secret| {
            contains_bytes(&response.payload, secret)
                || response
                    .stream
                    .iter()
                    .any(|frame| contains_bytes(&frame.payload, secret))
                || response
                    .safe_error
                    .as_ref()
                    .is_some_and(|error| contains_bytes(error.as_bytes(), secret))
        })
}

fn contains_bytes(haystack: &[u8], needle: &[u8]) -> bool {
    needle.len() <= haystack.len()
        && haystack
            .windows(needle.len())
            .any(|window| window == needle)
}

fn redact(message: &str, secrets: &[Vec<u8>]) -> String {
    secrets.iter().fold(message.to_owned(), |redacted, secret| {
        std::str::from_utf8(secret).map_or(redacted.clone(), |secret| {
            if secret.is_empty() {
                redacted
            } else {
                redacted.replace(secret, "[REDACTED]")
            }
        })
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn abi_redaction_removes_every_observed_named_credential() {
        let secrets = vec![b"top-secret".to_vec(), b"second".to_vec()];
        assert_eq!(
            redact("top-secret and second", &secrets),
            "[REDACTED] and [REDACTED]"
        );
    }

    #[test]
    fn abi_response_secret_detection_covers_payload_stream_and_error() {
        let mut response = HostResponse {
            interface_version: 2,
            invocation_id: "invocation".to_owned(),
            status: PluginStatus::Completed,
            payload_format: PayloadFormat::Json,
            payload: b"safe".to_vec(),
            stream: Vec::new(),
            safe_error: None,
        };
        assert!(!response_contains_secret(&response, &[b"secret".to_vec()]));
        response.safe_error = Some("contains secret".to_owned());
        assert!(response_contains_secret(&response, &[b"secret".to_vec()]));
    }

    #[test]
    fn abi_call_imports_cannot_be_relabelled_to_widen_authority() {
        assert!(!call_matches(
            HostCallKind::SafeLog,
            &PluginHostCall::Credential {
                name: "provider".to_owned()
            }
        ));
    }
}
