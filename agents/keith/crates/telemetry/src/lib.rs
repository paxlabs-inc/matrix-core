#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet, VecDeque};
use std::sync::{Mutex, MutexGuard};

use keith_agent_types::{
    ActionId, ChildId, CommandId, DeliveryId, EntityId, JobId, ProfileId, RootTreeId, SessionId,
    ToolCallId, TurnId, UtcTimestamp,
};
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MetricName {
    Workers,
    ActionQueueDepth,
    ModelLatency,
    ToolExecutions,
    Children,
    Kernels,
    RetrievalLatency,
    SchedulerLag,
    Deliveries,
    Initiatives,
    RefinementOutcomes,
}

impl MetricName {
    pub const ALL: [Self; 11] = [
        Self::Workers,
        Self::ActionQueueDepth,
        Self::ModelLatency,
        Self::ToolExecutions,
        Self::Children,
        Self::Kernels,
        Self::RetrievalLatency,
        Self::SchedulerLag,
        Self::Deliveries,
        Self::Initiatives,
        Self::RefinementOutcomes,
    ];
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct MetricContext {
    pub profile_id: Option<ProfileId>,
    pub root_tree_id: Option<RootTreeId>,
    pub session_id: Option<SessionId>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct MetricSample {
    pub name: MetricName,
    pub value: u64,
    pub context: MetricContext,
    pub recorded_at: UtcTimestamp,
}

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TraceKind {
    Command,
    Action,
    Turn,
    ProviderRequest,
    ToolCall,
    Child,
    JobAttempt,
    Wake,
    Delivery,
}

impl TraceKind {
    pub const ALL: [Self; 9] = [
        Self::Command,
        Self::Action,
        Self::Turn,
        Self::ProviderRequest,
        Self::ToolCall,
        Self::Child,
        Self::JobAttempt,
        Self::Wake,
        Self::Delivery,
    ];
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TracePhase {
    Queued,
    Started,
    Waiting,
    Completed,
    Failed,
    Cancelled,
}

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum FailureClass {
    Authentication,
    Authorization,
    InvalidInput,
    Unavailable,
    RateLimited,
    Timeout,
    Cancelled,
    OutputLimit,
    ResourceExhausted,
    CorruptState,
    Interrupted,
    Internal,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TraceCorrelation {
    pub command_id: Option<CommandId>,
    pub action_id: Option<ActionId>,
    pub turn_id: Option<TurnId>,
    pub provider_request_id: Option<EntityId>,
    pub tool_call_id: Option<ToolCallId>,
    pub child_id: Option<ChildId>,
    pub job_id: Option<JobId>,
    pub wake_id: Option<EntityId>,
    pub delivery_id: Option<DeliveryId>,
}

impl TraceCorrelation {
    fn supports(&self, kind: TraceKind) -> bool {
        match kind {
            TraceKind::Command => self.command_id.is_some(),
            TraceKind::Action => self.action_id.is_some(),
            TraceKind::Turn => self.turn_id.is_some(),
            TraceKind::ProviderRequest => self.provider_request_id.is_some(),
            TraceKind::ToolCall => self.tool_call_id.is_some(),
            TraceKind::Child => self.child_id.is_some(),
            TraceKind::JobAttempt => self.job_id.is_some(),
            TraceKind::Wake => self.wake_id.is_some(),
            TraceKind::Delivery => self.delivery_id.is_some(),
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TraceEvent {
    pub kind: TraceKind,
    pub phase: TracePhase,
    pub correlation: TraceCorrelation,
    pub duration_ms: Option<u64>,
    pub failure: Option<FailureClass>,
    pub recorded_at: UtcTimestamp,
}

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum LogComponent {
    Daemon,
    Worker,
    Session,
    Provider,
    Tool,
    Child,
    Kernel,
    Retrieval,
    Scheduler,
    Delivery,
    Initiative,
    Refinement,
    Channel,
    Desktop,
    Web,
    Tui,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum LogSeverity {
    Debug,
    Info,
    Warning,
    Error,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct StructuredLog {
    pub severity: LogSeverity,
    pub component: LogComponent,
    pub correlation: TraceCorrelation,
    pub failure: Option<FailureClass>,
    pub message: String,
    pub fields: BTreeMap<String, String>,
    pub recorded_at: UtcTimestamp,
}

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SensitiveClass {
    Prompt,
    PersonalMemory,
    ChannelContent,
    ToolOutput,
    ArtifactContent,
    Credential,
}

impl SensitiveClass {
    pub const ALL: [Self; 6] = [
        Self::Prompt,
        Self::PersonalMemory,
        Self::ChannelContent,
        Self::ToolOutput,
        Self::ArtifactContent,
        Self::Credential,
    ];
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SensitiveValue {
    pub class: SensitiveClass,
    pub value: String,
}

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ExportField {
    MetricAggregates,
    TraceTopology,
    CorrelationIdentifiers,
    FailureClasses,
    Durations,
}

impl ExportField {
    pub const fn disclosure(self) -> &'static str {
        match self {
            Self::MetricAggregates => "metric name, count, sum, and maximum",
            Self::TraceTopology => "trace subsystem kind and lifecycle phase",
            Self::CorrelationIdentifiers => "opaque command and runtime correlation identifiers",
            Self::FailureClasses => "closed failure class, component, and severity",
            Self::Durations => "operation duration in milliseconds",
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RemoteTelemetryPolicy {
    enabled: bool,
    fields: BTreeSet<ExportField>,
}

impl RemoteTelemetryPolicy {
    pub fn disabled() -> Self {
        Self {
            enabled: false,
            fields: BTreeSet::new(),
        }
    }

    /// Explicitly opts in to a non-empty, closed set of exported metadata.
    ///
    /// # Errors
    ///
    /// Returns an error when no metadata fields were selected.
    pub fn opt_in(fields: BTreeSet<ExportField>) -> Result<Self, TelemetryError> {
        if fields.is_empty() {
            return Err(TelemetryError::Invalid(
                "remote telemetry opt-in requires disclosed fields".into(),
            ));
        }
        Ok(Self {
            enabled: true,
            fields,
        })
    }

    pub const fn enabled(&self) -> bool {
        self.enabled
    }
}

impl Default for RemoteTelemetryPolicy {
    fn default() -> Self {
        Self::disabled()
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DisclosureItem {
    pub field: ExportField,
    pub metadata: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ExportDisclosure {
    pub enabled: bool,
    pub items: Vec<DisclosureItem>,
    pub excludes_content_and_secrets: bool,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "record", content = "data")]
pub enum RemoteRecord {
    MetricAggregate {
        name: MetricName,
        samples: u64,
        sum: u64,
        maximum: u64,
    },
    Trace {
        kind: Option<TraceKind>,
        phase: Option<TracePhase>,
        correlation: Option<Box<TraceCorrelation>>,
        duration_ms: Option<u64>,
        failure: Option<FailureClass>,
    },
    Failure {
        component: LogComponent,
        severity: LogSeverity,
        class: FailureClass,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RemoteBatch {
    pub disclosure: ExportDisclosure,
    pub records: Vec<RemoteRecord>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct TelemetryLimits {
    pub max_metrics: usize,
    pub max_traces: usize,
    pub max_logs: usize,
    pub max_message_bytes: usize,
    pub max_fields: usize,
    pub max_field_bytes: usize,
    pub max_sensitive_values: usize,
    pub max_diagnostic_bytes: usize,
    pub max_export_records: usize,
}

impl Default for TelemetryLimits {
    fn default() -> Self {
        Self {
            max_metrics: 4_096,
            max_traces: 4_096,
            max_logs: 2_048,
            max_message_bytes: 2_048,
            max_fields: 24,
            max_field_bytes: 512,
            max_sensitive_values: 256,
            max_diagnostic_bytes: 2 * 1_024 * 1_024,
            max_export_records: 1_024,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct DiagnosticRequest {
    pub max_entries_per_kind: usize,
    pub max_bytes: usize,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DiagnosticManifest {
    pub redacted: bool,
    pub user_review_required: bool,
    pub remote_export_enabled: bool,
    pub excluded_by_default: Vec<SensitiveClass>,
    pub omitted_metrics: usize,
    pub omitted_traces: usize,
    pub omitted_logs: usize,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DiagnosticBundle {
    pub manifest: DiagnosticManifest,
    pub metrics: Vec<MetricSample>,
    pub traces: Vec<TraceEvent>,
    pub logs: Vec<StructuredLog>,
}

impl DiagnosticBundle {
    /// Returns the complete bounded bundle in a user-reviewable representation.
    ///
    /// # Errors
    ///
    /// Returns an error if serialization unexpectedly fails.
    pub fn review_json(&self) -> Result<String, TelemetryError> {
        serde_json::to_string(self).map_err(TelemetryError::Serialize)
    }
}

#[derive(Debug, Error)]
pub enum TelemetryError {
    #[error("telemetry configuration or record is invalid: {0}")]
    Invalid(String),
    #[error("telemetry state lock was poisoned")]
    LockPoisoned,
    #[error("remote telemetry is disabled")]
    RemoteDisabled,
    #[error("telemetry serialization failed: {0}")]
    Serialize(serde_json::Error),
}

#[derive(Default)]
struct State {
    metrics: VecDeque<MetricSample>,
    traces: VecDeque<TraceEvent>,
    logs: VecDeque<StructuredLog>,
    remote: RemoteTelemetryPolicy,
}

struct Redactor {
    sensitive: Vec<String>,
}

pub struct TelemetryHub {
    limits: TelemetryLimits,
    redactor: Redactor,
    state: Mutex<State>,
}

impl TelemetryHub {
    /// Creates a local-only observability hub and registers values that must never be emitted.
    ///
    /// # Errors
    ///
    /// Returns an error for zero limits, empty sensitive values, or an oversized sensitive set.
    pub fn new(
        limits: TelemetryLimits,
        sensitive: impl IntoIterator<Item = SensitiveValue>,
    ) -> Result<Self, TelemetryError> {
        validate_limits(limits)?;
        let values = sensitive.into_iter().collect::<Vec<_>>();
        if values.len() > limits.max_sensitive_values
            || values.iter().any(|item| {
                item.value.is_empty() || item.value.len() > limits.max_message_bytes * 4
            })
        {
            return Err(TelemetryError::Invalid(
                "sensitive values must be non-empty and bounded".into(),
            ));
        }
        let mut sensitive = values
            .into_iter()
            .map(|item| item.value)
            .collect::<Vec<_>>();
        sensitive.sort_by_key(|value| std::cmp::Reverse(value.len()));
        sensitive.dedup();
        Ok(Self {
            limits,
            redactor: Redactor { sensitive },
            state: Mutex::new(State::default()),
        })
    }

    /// Records a metric whose closed schema cannot accept content-bearing labels.
    ///
    /// # Errors
    ///
    /// Returns an error if local state is unavailable.
    pub fn record_metric(&self, sample: MetricSample) -> Result<(), TelemetryError> {
        let mut state = self.lock()?;
        push_bounded(&mut state.metrics, sample, self.limits.max_metrics);
        Ok(())
    }

    /// Records a trace only when its declared subsystem identifier is present.
    ///
    /// # Errors
    ///
    /// Returns an error for missing correlation, inconsistent failure state, or unavailable state.
    pub fn record_trace(&self, event: TraceEvent) -> Result<(), TelemetryError> {
        if !event.correlation.supports(event.kind)
            || (event.phase == TracePhase::Failed) != event.failure.is_some()
        {
            return Err(TelemetryError::Invalid(
                "trace kind, correlation, phase, and failure class must agree".into(),
            ));
        }
        let mut state = self.lock()?;
        push_bounded(&mut state.traces, event, self.limits.max_traces);
        Ok(())
    }

    /// Redacts and bounds a structured local log before retention.
    ///
    /// # Errors
    ///
    /// Returns an error for excess fields or unavailable local state.
    pub fn record_log(&self, mut log: StructuredLog) -> Result<(), TelemetryError> {
        if log.fields.len() > self.limits.max_fields {
            return Err(TelemetryError::Invalid(
                "structured log field count exceeded its bound".into(),
            ));
        }
        log.message = self
            .redactor
            .redact(&log.message, self.limits.max_message_bytes);
        let mut fields = BTreeMap::new();
        for (key, value) in log.fields {
            let key = self.redactor.redact(&key, self.limits.max_field_bytes);
            let value = if sensitive_key(&key) {
                "[redacted]".into()
            } else {
                self.redactor.redact(&value, self.limits.max_field_bytes)
            };
            fields.insert(key, value);
        }
        log.fields = fields;
        let mut state = self.lock()?;
        push_bounded(&mut state.logs, log, self.limits.max_logs);
        Ok(())
    }

    /// Replaces the remote export policy. Export remains disabled unless this receives an opt-in.
    ///
    /// # Errors
    ///
    /// Returns an error if local state is unavailable.
    pub fn set_remote_policy(&self, policy: RemoteTelemetryPolicy) -> Result<(), TelemetryError> {
        self.lock()?.remote = policy;
        Ok(())
    }

    /// Returns the exact metadata disclosure for the current export policy.
    ///
    /// # Errors
    ///
    /// Returns an error if local state is unavailable.
    pub fn export_disclosure(&self) -> Result<ExportDisclosure, TelemetryError> {
        Ok(disclosure(&self.lock()?.remote))
    }

    /// Builds a content-free remote metadata batch without performing network I/O.
    ///
    /// # Errors
    ///
    /// Returns an error unless the operator explicitly opted in or local state is unavailable.
    pub fn remote_batch(&self) -> Result<RemoteBatch, TelemetryError> {
        let state = self.lock()?;
        if !state.remote.enabled {
            return Err(TelemetryError::RemoteDisabled);
        }
        let mut records = Vec::new();
        if state.remote.fields.contains(&ExportField::MetricAggregates) {
            let mut aggregates = BTreeMap::<MetricName, (u64, u64, u64)>::new();
            for metric in &state.metrics {
                let entry = aggregates.entry(metric.name).or_default();
                entry.0 = entry.0.saturating_add(1);
                entry.1 = entry.1.saturating_add(metric.value);
                entry.2 = entry.2.max(metric.value);
            }
            for (name, (samples, sum, maximum)) in aggregates {
                records.push(RemoteRecord::MetricAggregate {
                    name,
                    samples,
                    sum,
                    maximum,
                });
            }
        }
        let include_trace = [
            ExportField::TraceTopology,
            ExportField::CorrelationIdentifiers,
            ExportField::FailureClasses,
            ExportField::Durations,
        ]
        .iter()
        .any(|field| state.remote.fields.contains(field));
        if include_trace {
            for trace in &state.traces {
                let record = RemoteRecord::Trace {
                    kind: state
                        .remote
                        .fields
                        .contains(&ExportField::TraceTopology)
                        .then_some(trace.kind),
                    phase: state
                        .remote
                        .fields
                        .contains(&ExportField::TraceTopology)
                        .then_some(trace.phase),
                    correlation: state
                        .remote
                        .fields
                        .contains(&ExportField::CorrelationIdentifiers)
                        .then(|| Box::new(trace.correlation.clone())),
                    duration_ms: state
                        .remote
                        .fields
                        .contains(&ExportField::Durations)
                        .then_some(trace.duration_ms)
                        .flatten(),
                    failure: state
                        .remote
                        .fields
                        .contains(&ExportField::FailureClasses)
                        .then_some(trace.failure)
                        .flatten(),
                };
                let RemoteRecord::Trace {
                    kind,
                    phase,
                    correlation,
                    duration_ms,
                    failure,
                } = &record
                else {
                    unreachable!("the constructed record is always a trace")
                };
                if kind.is_some()
                    || phase.is_some()
                    || correlation.is_some()
                    || duration_ms.is_some()
                    || failure.is_some()
                {
                    records.push(record);
                }
            }
        }
        if state.remote.fields.contains(&ExportField::FailureClasses) {
            records.extend(state.logs.iter().filter_map(|log| {
                log.failure.map(|class| RemoteRecord::Failure {
                    component: log.component,
                    severity: log.severity,
                    class,
                })
            }));
        }
        records.truncate(self.limits.max_export_records);
        Ok(RemoteBatch {
            disclosure: disclosure(&state.remote),
            records,
        })
    }

    /// Creates a redacted, size-bounded bundle that must be reviewed before sharing.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid request bounds, serialization failure, or unavailable state.
    pub fn diagnostic_bundle(
        &self,
        request: DiagnosticRequest,
    ) -> Result<DiagnosticBundle, TelemetryError> {
        if request.max_entries_per_kind == 0
            || request.max_bytes < 512
            || request.max_bytes > self.limits.max_diagnostic_bytes
        {
            return Err(TelemetryError::Invalid(
                "diagnostic bundle bounds are invalid".into(),
            ));
        }
        let state = self.lock()?;
        let mut bundle = DiagnosticBundle {
            manifest: DiagnosticManifest {
                redacted: true,
                user_review_required: true,
                remote_export_enabled: state.remote.enabled,
                excluded_by_default: SensitiveClass::ALL.into(),
                omitted_metrics: state
                    .metrics
                    .len()
                    .saturating_sub(request.max_entries_per_kind),
                omitted_traces: state
                    .traces
                    .len()
                    .saturating_sub(request.max_entries_per_kind),
                omitted_logs: state
                    .logs
                    .len()
                    .saturating_sub(request.max_entries_per_kind),
            },
            metrics: newest(&state.metrics, request.max_entries_per_kind),
            traces: newest(&state.traces, request.max_entries_per_kind),
            logs: newest(&state.logs, request.max_entries_per_kind),
        };
        while serde_json::to_vec(&bundle)
            .map_err(TelemetryError::Serialize)?
            .len()
            > request.max_bytes
        {
            if bundle.logs.pop().is_some() {
                bundle.manifest.omitted_logs = bundle.manifest.omitted_logs.saturating_add(1);
            } else if bundle.traces.pop().is_some() {
                bundle.manifest.omitted_traces = bundle.manifest.omitted_traces.saturating_add(1);
            } else if bundle.metrics.pop().is_some() {
                bundle.manifest.omitted_metrics = bundle.manifest.omitted_metrics.saturating_add(1);
            } else {
                return Err(TelemetryError::Invalid(
                    "diagnostic byte bound cannot hold its manifest".into(),
                ));
            }
        }
        Ok(bundle)
    }

    /// Returns bounded local counts without exposing retained content.
    ///
    /// # Errors
    ///
    /// Returns an error if local state is unavailable.
    pub fn local_counts(&self) -> Result<(usize, usize, usize), TelemetryError> {
        let state = self.lock()?;
        Ok((state.metrics.len(), state.traces.len(), state.logs.len()))
    }

    fn lock(&self) -> Result<MutexGuard<'_, State>, TelemetryError> {
        self.state.lock().map_err(|_| TelemetryError::LockPoisoned)
    }
}

impl Redactor {
    fn redact(&self, value: &str, maximum: usize) -> String {
        let mut redacted = value.to_owned();
        for sensitive in &self.sensitive {
            redacted = redacted.replace(sensitive, "[redacted]");
        }
        for marker in [
            "Bearer ",
            "password=",
            "token=",
            "secret=",
            "api_key=",
            "apikey=",
        ] {
            redact_after_marker(&mut redacted, marker);
        }
        bounded_text(redacted, maximum)
    }
}

fn redact_after_marker(value: &mut String, marker: &str) {
    let mut search_from = 0;
    while let Some(relative) = value[search_from..].find(marker) {
        let start = search_from + relative + marker.len();
        let end = value[start..]
            .char_indices()
            .find_map(|(offset, character)| {
                (character.is_whitespace() || matches!(character, ',' | ';' | ')' | ']' | '}'))
                    .then_some(start + offset)
            })
            .unwrap_or(value.len());
        value.replace_range(start..end, "[redacted]");
        search_from = start + "[redacted]".len();
    }
}

fn sensitive_key(key: &str) -> bool {
    let key = key.to_ascii_lowercase();
    [
        "secret",
        "token",
        "password",
        "credential",
        "authorization",
        "cookie",
        "prompt",
        "memory",
        "content",
        "output",
        "artifact",
    ]
    .iter()
    .any(|marker| key.contains(marker))
}

fn bounded_text(mut value: String, maximum: usize) -> String {
    if value.len() <= maximum {
        return value;
    }
    let mut boundary = maximum;
    while boundary > 0 && !value.is_char_boundary(boundary) {
        boundary -= 1;
    }
    value.truncate(boundary);
    value
}

fn validate_limits(limits: TelemetryLimits) -> Result<(), TelemetryError> {
    if [
        limits.max_metrics,
        limits.max_traces,
        limits.max_logs,
        limits.max_message_bytes,
        limits.max_fields,
        limits.max_field_bytes,
        limits.max_sensitive_values,
        limits.max_diagnostic_bytes,
        limits.max_export_records,
    ]
    .contains(&0)
        || limits.max_diagnostic_bytes < 512
    {
        return Err(TelemetryError::Invalid(
            "telemetry limits must be non-zero and internally usable".into(),
        ));
    }
    Ok(())
}

fn push_bounded<T>(queue: &mut VecDeque<T>, value: T, maximum: usize) {
    if queue.len() == maximum {
        queue.pop_front();
    }
    queue.push_back(value);
}

fn newest<T: Clone>(queue: &VecDeque<T>, maximum: usize) -> Vec<T> {
    let mut values = queue
        .iter()
        .rev()
        .take(maximum)
        .cloned()
        .collect::<Vec<_>>();
    values.reverse();
    values
}

fn disclosure(policy: &RemoteTelemetryPolicy) -> ExportDisclosure {
    ExportDisclosure {
        enabled: policy.enabled,
        items: policy
            .fields
            .iter()
            .map(|field| DisclosureItem {
                field: *field,
                metadata: field.disclosure().into(),
            })
            .collect(),
        excludes_content_and_secrets: true,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn limits() -> TelemetryLimits {
        TelemetryLimits {
            max_metrics: 3,
            max_traces: 3,
            max_logs: 3,
            max_message_bytes: 256,
            max_fields: 8,
            max_field_bytes: 128,
            max_sensitive_values: 16,
            max_diagnostic_bytes: 4_096,
            max_export_records: 32,
        }
    }

    fn correlation(kind: TraceKind) -> TraceCorrelation {
        let mut correlation = TraceCorrelation::default();
        match kind {
            TraceKind::Command => correlation.command_id = Some(CommandId::new()),
            TraceKind::Action => correlation.action_id = Some(ActionId::new()),
            TraceKind::Turn => correlation.turn_id = Some(TurnId::new()),
            TraceKind::ProviderRequest => {
                correlation.provider_request_id = Some(EntityId::new());
            }
            TraceKind::ToolCall => correlation.tool_call_id = Some(ToolCallId::new()),
            TraceKind::Child => correlation.child_id = Some(ChildId::new()),
            TraceKind::JobAttempt => correlation.job_id = Some(JobId::new()),
            TraceKind::Wake => correlation.wake_id = Some(EntityId::new()),
            TraceKind::Delivery => correlation.delivery_id = Some(DeliveryId::new()),
        }
        correlation
    }

    fn trace(kind: TraceKind, phase: TracePhase) -> TraceEvent {
        TraceEvent {
            kind,
            phase,
            correlation: correlation(kind),
            duration_ms: Some(12),
            failure: (phase == TracePhase::Failed).then_some(FailureClass::Timeout),
            recorded_at: UtcTimestamp::UNIX_EPOCH,
        }
    }

    #[test]
    fn closed_metrics_and_traces_cover_every_required_subsystem_and_correlation() {
        assert_eq!(MetricName::ALL.len(), 11);
        assert_eq!(TraceKind::ALL.len(), 9);
        let hub = TelemetryHub::new(limits(), []).unwrap();
        for (index, name) in MetricName::ALL.into_iter().enumerate() {
            hub.record_metric(MetricSample {
                name,
                value: u64::try_from(index).unwrap(),
                context: MetricContext::default(),
                recorded_at: UtcTimestamp::UNIX_EPOCH,
            })
            .unwrap();
        }
        for kind in TraceKind::ALL {
            hub.record_trace(trace(kind, TracePhase::Completed))
                .unwrap();
        }
        assert_eq!(hub.local_counts().unwrap(), (3, 3, 0));
        assert!(matches!(
            hub.record_trace(TraceEvent {
                kind: TraceKind::ToolCall,
                phase: TracePhase::Completed,
                correlation: TraceCorrelation::default(),
                duration_ms: None,
                failure: None,
                recorded_at: UtcTimestamp::UNIX_EPOCH,
            }),
            Err(TelemetryError::Invalid(_))
        ));
    }

    #[test]
    fn local_retention_is_bounded_and_remote_export_requires_exact_opt_in() {
        let hub = TelemetryHub::new(limits(), []).unwrap();
        for index in 0..10 {
            hub.record_metric(MetricSample {
                name: MetricName::Workers,
                value: index,
                context: MetricContext::default(),
                recorded_at: UtcTimestamp::from_unix_millis(i64::try_from(index).unwrap()),
            })
            .unwrap();
        }
        assert_eq!(hub.local_counts().unwrap(), (3, 0, 0));
        assert!(!hub.export_disclosure().unwrap().enabled);
        assert!(matches!(
            hub.remote_batch(),
            Err(TelemetryError::RemoteDisabled)
        ));

        let fields = BTreeSet::from([ExportField::MetricAggregates, ExportField::FailureClasses]);
        hub.set_remote_policy(RemoteTelemetryPolicy::opt_in(fields.clone()).unwrap())
            .unwrap();
        let disclosure = hub.export_disclosure().unwrap();
        assert_eq!(
            disclosure
                .items
                .iter()
                .map(|item| item.field)
                .collect::<BTreeSet<_>>(),
            fields
        );
        assert!(disclosure.excludes_content_and_secrets);
        assert!(matches!(
            &hub.remote_batch().unwrap().records[0],
            RemoteRecord::MetricAggregate {
                name: MetricName::Workers,
                samples: 3,
                sum: 24,
                maximum: 9,
            }
        ));
    }

    #[test]
    fn every_private_subsystem_is_redacted_from_actionable_bounded_diagnostics() {
        let private = [
            (SensitiveClass::Prompt, "prompt-private-value"),
            (SensitiveClass::PersonalMemory, "memory-private-value"),
            (SensitiveClass::ChannelContent, "channel-private-value"),
            (SensitiveClass::ToolOutput, "tool-private-value"),
            (SensitiveClass::ArtifactContent, "artifact-private-value"),
            (SensitiveClass::Credential, "credential-private-value"),
        ];
        let hub = TelemetryHub::new(
            limits(),
            private.iter().map(|(class, value)| SensitiveValue {
                class: *class,
                value: (*value).into(),
            }),
        )
        .unwrap();
        for (class, value) in private {
            hub.record_log(StructuredLog {
                severity: LogSeverity::Error,
                component: LogComponent::Tool,
                correlation: correlation(TraceKind::ToolCall),
                failure: Some(FailureClass::Timeout),
                message: format!("{class:?} failed around {value}; Bearer bearer-private"),
                fields: BTreeMap::from([
                    ("private_content".into(), value.into()),
                    ("classification".into(), "timeout".into()),
                    ("authorization".into(), "bearer-private".into()),
                ]),
                recorded_at: UtcTimestamp::UNIX_EPOCH,
            })
            .unwrap();
        }
        hub.record_trace(trace(TraceKind::ToolCall, TracePhase::Failed))
            .unwrap();
        let bundle = hub
            .diagnostic_bundle(DiagnosticRequest {
                max_entries_per_kind: 3,
                max_bytes: 2_048,
            })
            .unwrap();
        let review = bundle.review_json().unwrap();
        assert!(review.len() <= 2_048);
        assert!(review.contains("timeout"));
        assert!(review.contains("[redacted]"));
        for (_, value) in private {
            assert!(!review.contains(value));
        }
        assert!(!review.contains("bearer-private"));
        assert_eq!(
            bundle.manifest.excluded_by_default,
            SensitiveClass::ALL.to_vec()
        );
        assert!(bundle.manifest.user_review_required);

        hub.set_remote_policy(
            RemoteTelemetryPolicy::opt_in(BTreeSet::from([
                ExportField::TraceTopology,
                ExportField::FailureClasses,
                ExportField::Durations,
            ]))
            .unwrap(),
        )
        .unwrap();
        let exported = serde_json::to_string(&hub.remote_batch().unwrap()).unwrap();
        assert!(exported.contains("timeout"));
        for (_, value) in private {
            assert!(!exported.contains(value));
        }
    }

    #[test]
    fn diagnostic_byte_limit_omits_records_instead_of_leaking_or_overflowing() {
        let hub = TelemetryHub::new(limits(), []).unwrap();
        for index in 0..3 {
            hub.record_log(StructuredLog {
                severity: LogSeverity::Warning,
                component: LogComponent::Daemon,
                correlation: TraceCorrelation::default(),
                failure: Some(FailureClass::ResourceExhausted),
                message: format!("bounded failure {index} {}", "x".repeat(200)),
                fields: BTreeMap::new(),
                recorded_at: UtcTimestamp::UNIX_EPOCH,
            })
            .unwrap();
        }
        let bundle = hub
            .diagnostic_bundle(DiagnosticRequest {
                max_entries_per_kind: 3,
                max_bytes: 700,
            })
            .unwrap();
        assert!(bundle.review_json().unwrap().len() <= 700);
        assert!(bundle.manifest.omitted_logs > 0);
    }
}
