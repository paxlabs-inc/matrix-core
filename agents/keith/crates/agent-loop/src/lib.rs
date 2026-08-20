#![forbid(unsafe_code)]

use keith_agent_types::{
    ArtifactId, EntityId, EntryId, ProfileId, TimestampError, ToolCallId, ToolEffectState,
    ToolErrorCategory, ToolFailure, ToolFailureStatus, TurnId, UtcTimestamp, canonical_json_bytes,
};
use keith_artifacts::{ArtifactError, OutputSpill};
use keith_model_registry::{CredentialResolver, ModelPurpose, ModelRegistry, RegistryError};
use keith_provider_core::{
    CancellationToken, ContentBlock, ContextProvenance, ContextRecord, Message, MessageRole,
    ModelEvent, ModelRequest, ModelVisibility, PersistPolicy, StopReason, StreamControl,
    ToolBehavior, ToolDefinition, Usage,
};
use keith_session_store::{
    CompactionTrigger, ContentBlock as StoredContentBlock, MessageRole as StoredMessageRole,
    SessionEntryPayload, SessionStoreError, SessionWriter, StepBoundaryState, StoredMessage,
};
use keith_tool_core::{ToolExecutionError, ToolExecutor, ToolInvocation};
use serde::{Deserialize, Serialize};
use std::collections::{BTreeMap, BTreeSet};
use std::panic::{AssertUnwindSafe, catch_unwind};
use thiserror::Error;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "event")]
pub enum AgentEventKind {
    AgentStarted,
    TurnStarted {
        turn_id: TurnId,
        number: u32,
    },
    MessageStarted {
        turn_id: TurnId,
    },
    MessageDelta {
        turn_id: TurnId,
        text: String,
    },
    MessageCompleted {
        turn_id: TurnId,
        complete: bool,
    },
    AssistantActivityCompleted {
        turn_id: TurnId,
        activity_id: EntryId,
    },
    FinalCandidateCompleted {
        turn_id: TurnId,
        candidate_id: EntryId,
    },
    ToolStarted {
        turn_id: TurnId,
        call_id: ToolCallId,
        name: String,
    },
    ToolCompleted {
        turn_id: TurnId,
        call_id: ToolCallId,
        name: String,
        is_error: bool,
        artifact_id: Option<ArtifactId>,
    },
    StrategyChanged {
        turn_id: TurnId,
        reason: String,
    },
    TurnEnded {
        turn_id: TurnId,
        usage: Usage,
    },
    AgentEnded {
        outcome: AgentOutcome,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentEvent {
    pub sequence: u64,
    pub kind: AgentEventKind,
}

pub trait AgentEventSubscriber: Send {
    fn on_event(&mut self, event: &AgentEvent);
}

impl<F> AgentEventSubscriber for F
where
    F: FnMut(&AgentEvent) + Send,
{
    fn on_event(&mut self, event: &AgentEvent) {
        self(event);
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AgentOutcome {
    Completed,
    Cancelled,
    Exhausted,
}

pub trait SteeringSource: Send + Sync {
    fn take_at_boundary(&self) -> Option<String>;
}

#[derive(Default)]
pub struct NoSteering;

impl SteeringSource for NoSteering {
    fn take_at_boundary(&self) -> Option<String> {
        None
    }
}

pub trait ContextCompactor: Send + Sync {
    /// # Errors
    ///
    /// Returns an overflow error when the request cannot be reduced safely.
    fn compact(
        &self,
        session: &mut SessionWriter,
        request: &ModelRequest,
        trigger: CompactionTrigger,
        cancellation: &CancellationToken,
    ) -> Result<CompactionProgress, AgentLoopError>;
}

#[derive(Default)]
pub struct NoCompaction;

impl ContextCompactor for NoCompaction {
    fn compact(
        &self,
        session: &mut SessionWriter,
        request: &ModelRequest,
        _trigger: CompactionTrigger,
        _cancellation: &CancellationToken,
    ) -> Result<CompactionProgress, AgentLoopError> {
        Ok(CompactionProgress {
            request: request.clone(),
            previous_generation: session.manifest().compaction_generation,
            current_generation: session.manifest().compaction_generation,
        })
    }
}

#[derive(Clone, Debug, PartialEq)]
pub struct CompactionProgress {
    pub request: ModelRequest,
    pub previous_generation: u64,
    pub current_generation: u64,
}

impl CompactionProgress {
    pub const fn advanced(&self) -> bool {
        self.current_generation > self.previous_generation
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct AgentLoopConfig {
    pub max_turns: u32,
    pub max_parallel_reads: usize,
    pub inline_tool_output_bytes: usize,
    pub identical_failure_limit: u32,
    pub context_overflow_retries: u32,
    pub empty_response_retries: u32,
}

impl Default for AgentLoopConfig {
    fn default() -> Self {
        Self {
            max_turns: 32,
            max_parallel_reads: 4,
            inline_tool_output_bytes: 64 * 1_024,
            identical_failure_limit: 3,
            context_overflow_retries: 2,
            empty_response_retries: 1,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AgentRunResult {
    pub outcome: AgentOutcome,
    pub final_candidate_id: EntryId,
    pub turns: u32,
    pub usage: Usage,
}

#[derive(Debug, Error)]
pub enum AgentLoopError {
    #[error("model routing failed: {0}")]
    Registry(#[from] RegistryError),
    #[error("session commit failed: {0}")]
    Session(#[from] SessionStoreError),
    #[error("artifact output failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("artifact service failed: {0}")]
    Artifact(#[from] ArtifactError),
    #[error("clock failed: {0}")]
    Time(#[from] TimestampError),
    #[error("model response was empty")]
    EmptyResponse,
    #[error("model context overflow persisted after compaction")]
    ContextOverflow,
    #[error("durable context compaction failed: {0}")]
    Compaction(String),
    #[error("tool stream was malformed: {0}")]
    MalformedTool(String),
    #[error("identical failure repeated beyond the configured bound: {0}")]
    RepeatedFailure(String),
    #[error("agent reached its turn budget")]
    TurnBudget,
    #[error("agent run was cancelled")]
    Cancelled,
    #[error("subscriber sequence overflowed")]
    SequenceOverflow,
    #[error("tool worker panicked")]
    ToolWorkerPanicked,
}

pub struct AgentLoop<'a> {
    registry: &'a ModelRegistry,
    profile_id: &'a ProfileId,
    credentials: &'a dyn CredentialResolver,
    tools: &'a dyn ToolExecutor,
    spill: &'a dyn OutputSpill,
    compactor: &'a dyn ContextCompactor,
    steering: &'a dyn SteeringSource,
    session: &'a mut SessionWriter,
    subscribers: Vec<Box<dyn AgentEventSubscriber + 'a>>,
    config: AgentLoopConfig,
    sequence: u64,
}

impl<'a> AgentLoop<'a> {
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        registry: &'a ModelRegistry,
        profile_id: &'a ProfileId,
        credentials: &'a dyn CredentialResolver,
        tools: &'a dyn ToolExecutor,
        spill: &'a dyn OutputSpill,
        compactor: &'a dyn ContextCompactor,
        steering: &'a dyn SteeringSource,
        session: &'a mut SessionWriter,
        config: AgentLoopConfig,
    ) -> Self {
        Self {
            registry,
            profile_id,
            credentials,
            tools,
            spill,
            compactor,
            steering,
            session,
            subscribers: Vec::new(),
            config,
            sequence: 0,
        }
    }

    pub fn subscribe(&mut self, subscriber: impl AgentEventSubscriber + 'a) {
        self.subscribers.push(Box::new(subscriber));
    }

    /// # Errors
    ///
    /// Returns a typed loop error for provider, storage, tool, cancellation, or budget failure.
    #[allow(clippy::too_many_lines)]
    pub fn run(
        &mut self,
        mut request: ModelRequest,
        cancellation: &CancellationToken,
    ) -> Result<AgentRunResult, AgentLoopError> {
        let durable_turn_id = request
            .context
            .messages
            .iter()
            .flatten()
            .find(|record| record.entry_id == request.context.active_user_entry_id)
            .map(|record| record.turn_id.clone())
            .ok_or_else(|| {
                AgentLoopError::Compaction(
                    "model request is missing its active user turn identity".into(),
                )
            })?;
        let turn_id = &durable_turn_id;
        self.emit(AgentEventKind::AgentStarted)?;
        let mut total_usage = Usage::default();
        let mut empty_attempts = 0;
        let mut overflow_attempts = 0;
        let mut failures = BTreeMap::<String, u32>::new();

        for number in 1..=self.config.max_turns {
            if cancellation.is_cancelled() {
                self.finish(AgentOutcome::Cancelled)?;
                return Err(AgentLoopError::Cancelled);
            }
            if let Some(text) = self.steering.take_at_boundary() {
                self.add_controller_guidance(&mut request, turn_id, "steering", &text)?;
            }
            match self.compactor.compact(
                self.session,
                &request,
                CompactionTrigger::Pressure,
                cancellation,
            ) {
                Ok(progress) => request = progress.request,
                Err(error) => {
                    self.emit(AgentEventKind::StrategyChanged {
                        turn_id: turn_id.clone(),
                        reason: format!("pressure_compaction_failed:{error}"),
                    })?;
                }
            }
            self.emit(AgentEventKind::TurnStarted {
                turn_id: turn_id.clone(),
                number,
            })?;
            let step_request_id = request.request_id.clone();
            self.append(SessionEntryPayload::StepBoundary {
                turn_id: turn_id.clone(),
                step: number,
                provider_request_id: step_request_id.clone(),
                state: StepBoundaryState::Started,
                detail: None,
            })?;
            self.emit(AgentEventKind::MessageStarted {
                turn_id: turn_id.clone(),
            })?;
            let mut assembly = StreamAssembly::default();
            let stream_result = self.registry.stream_with_fallback(
                self.profile_id,
                ModelPurpose::Primary,
                &request,
                self.credentials,
                cancellation,
                &mut |event| {
                    if let ModelEvent::TextDelta { text } = &event {
                        self.emit(AgentEventKind::MessageDelta {
                            turn_id: turn_id.clone(),
                            text: text.clone(),
                        })
                        .map_err(|error| loop_error_as_provider(&error))?;
                    }
                    assembly.accept(event);
                    Ok(StreamControl::Continue)
                },
            );
            let attempt = match stream_result {
                Ok(attempt) => attempt,
                Err(error) if is_context_overflow(&error) => {
                    self.append(SessionEntryPayload::StepBoundary {
                        turn_id: turn_id.clone(),
                        step: number,
                        provider_request_id: step_request_id.clone(),
                        state: StepBoundaryState::Failed,
                        detail: Some("provider_context_overflow".into()),
                    })?;
                    self.emit(AgentEventKind::MessageCompleted {
                        turn_id: turn_id.clone(),
                        complete: false,
                    })?;
                    if overflow_attempts >= self.config.context_overflow_retries {
                        self.finish(AgentOutcome::Exhausted)?;
                        return Err(AgentLoopError::ContextOverflow);
                    }
                    overflow_attempts += 1;
                    let Ok(progress) = self.compactor.compact(
                        self.session,
                        &request,
                        CompactionTrigger::ProviderOverflow,
                        cancellation,
                    ) else {
                        self.finish(AgentOutcome::Exhausted)?;
                        return Err(AgentLoopError::ContextOverflow);
                    };
                    if !progress.advanced() {
                        self.finish(AgentOutcome::Exhausted)?;
                        return Err(AgentLoopError::ContextOverflow);
                    }
                    request = progress.request;
                    self.emit(AgentEventKind::StrategyChanged {
                        turn_id: turn_id.clone(),
                        reason: "context_overflow_compaction".into(),
                    })?;
                    continue;
                }
                Err(RegistryError::Provider(error))
                    if error.kind == keith_provider_core::ProviderErrorKind::Cancelled =>
                {
                    self.append(SessionEntryPayload::StepBoundary {
                        turn_id: turn_id.clone(),
                        step: number,
                        provider_request_id: step_request_id.clone(),
                        state: StepBoundaryState::Cancelled,
                        detail: Some("provider_cancelled".into()),
                    })?;
                    self.emit(AgentEventKind::MessageCompleted {
                        turn_id: turn_id.clone(),
                        complete: false,
                    })?;
                    self.finish(AgentOutcome::Cancelled)?;
                    return Err(AgentLoopError::Cancelled);
                }
                Err(error) => {
                    self.append(SessionEntryPayload::StepBoundary {
                        turn_id: turn_id.clone(),
                        step: number,
                        provider_request_id: step_request_id.clone(),
                        state: StepBoundaryState::Failed,
                        detail: Some(error.to_string()),
                    })?;
                    self.emit(AgentEventKind::MessageCompleted {
                        turn_id: turn_id.clone(),
                        complete: false,
                    })?;
                    return Err(error.into());
                }
            };
            let completed = match assembly.finish() {
                Ok(completed) => completed,
                Err(error) => {
                    self.append(SessionEntryPayload::StepBoundary {
                        turn_id: turn_id.clone(),
                        step: number,
                        provider_request_id: step_request_id.clone(),
                        state: StepBoundaryState::Failed,
                        detail: Some(error.to_string()),
                    })?;
                    self.emit(AgentEventKind::MessageCompleted {
                        turn_id: turn_id.clone(),
                        complete: false,
                    })?;
                    return Err(error);
                }
            };
            add_usage(&mut total_usage, attempt.usage);
            overflow_attempts = 0;

            if completed.text.trim().is_empty() && completed.calls.is_empty() {
                if empty_attempts >= self.config.empty_response_retries {
                    self.finish(AgentOutcome::Exhausted)?;
                    return Err(AgentLoopError::EmptyResponse);
                }
                empty_attempts += 1;
                self.append(SessionEntryPayload::StepBoundary {
                    turn_id: turn_id.clone(),
                    step: number,
                    provider_request_id: step_request_id.clone(),
                    state: StepBoundaryState::Failed,
                    detail: Some("empty_provider_response".into()),
                })?;
                self.emit(AgentEventKind::MessageCompleted {
                    turn_id: turn_id.clone(),
                    complete: false,
                })?;
                self.add_controller_guidance(
                    &mut request,
                    turn_id,
                    "empty_response_retry",
                    "The previous response was empty. Continue with a substantive response.",
                )?;
                self.emit(AgentEventKind::StrategyChanged {
                    turn_id: turn_id.clone(),
                    reason: "empty_response_retry".into(),
                })?;
                continue;
            }

            self.commit_model_change(&attempt.provider, &attempt.model)?;
            if completed.calls.is_empty() {
                let candidate = self.commit_final_candidate(turn_id, &completed, attempt.usage)?;
                self.commit_usage(attempt.usage)?;
                self.append(SessionEntryPayload::StepBoundary {
                    turn_id: turn_id.clone(),
                    step: number,
                    provider_request_id: step_request_id.clone(),
                    state: StepBoundaryState::Completed,
                    detail: None,
                })?;
                self.emit(AgentEventKind::FinalCandidateCompleted {
                    turn_id: turn_id.clone(),
                    candidate_id: candidate.clone(),
                })?;
                self.emit(AgentEventKind::TurnEnded {
                    turn_id: turn_id.clone(),
                    usage: attempt.usage,
                })?;
                self.finish(AgentOutcome::Completed)?;
                return Ok(AgentRunResult {
                    outcome: AgentOutcome::Completed,
                    final_candidate_id: candidate,
                    turns: number,
                    usage: total_usage,
                });
            }

            let activity = self.commit_assistant_activity(turn_id, &completed)?;
            self.emit(AgentEventKind::AssistantActivityCompleted {
                turn_id: turn_id.clone(),
                activity_id: activity.entry_id.clone(),
            })?;

            let outcomes =
                match self.execute_calls(turn_id, &request.tools, &completed.calls, cancellation) {
                    Ok(outcomes) => outcomes,
                    Err(error) => {
                        self.commit_unknown_tool_results(turn_id, &completed.calls)?;
                        self.append(SessionEntryPayload::StepBoundary {
                            turn_id: turn_id.clone(),
                            step: number,
                            provider_request_id: step_request_id.clone(),
                            state: StepBoundaryState::Failed,
                            detail: Some(error.to_string()),
                        })?;
                        self.emit(AgentEventKind::MessageCompleted {
                            turn_id: turn_id.clone(),
                            complete: false,
                        })?;
                        return Err(error);
                    }
                };
            append_tool_exchange(&mut request, &completed, &activity, &outcomes);
            if cancellation.is_cancelled() {
                self.append(SessionEntryPayload::StepBoundary {
                    turn_id: turn_id.clone(),
                    step: number,
                    provider_request_id: step_request_id.clone(),
                    state: StepBoundaryState::Cancelled,
                    detail: Some("cancelled_after_tool_results".into()),
                })?;
                self.emit(AgentEventKind::MessageCompleted {
                    turn_id: turn_id.clone(),
                    complete: false,
                })?;
                self.finish(AgentOutcome::Cancelled)?;
                return Err(AgentLoopError::Cancelled);
            }
            let mut changed_strategy = false;
            for outcome in &outcomes {
                if outcome.is_error {
                    let signature = tool_failure_fingerprint(outcome);
                    let count = failures.entry(signature.clone()).or_default();
                    *count = count.saturating_add(1);
                    if *count >= self.config.identical_failure_limit {
                        self.finish(AgentOutcome::Exhausted)?;
                        return Err(AgentLoopError::RepeatedFailure(signature));
                    }
                    if *count > 1 {
                        changed_strategy = true;
                    }
                }
            }
            if changed_strategy {
                self.add_controller_guidance(
                    &mut request,
                    turn_id,
                    "repeated_tool_failure",
                    "A tool failure repeated. Change strategy; do not repeat the identical call.",
                )?;
                self.emit(AgentEventKind::StrategyChanged {
                    turn_id: turn_id.clone(),
                    reason: "repeated_tool_failure".into(),
                })?;
            }
            self.commit_usage(attempt.usage)?;
            self.append(SessionEntryPayload::StepBoundary {
                turn_id: turn_id.clone(),
                step: number,
                provider_request_id: step_request_id,
                state: StepBoundaryState::Completed,
                detail: None,
            })?;
            self.emit(AgentEventKind::TurnEnded {
                turn_id: turn_id.clone(),
                usage: attempt.usage,
            })?;
        }
        self.finish(AgentOutcome::Exhausted)?;
        Err(AgentLoopError::TurnBudget)
    }

    fn execute_calls(
        &mut self,
        turn_id: &TurnId,
        definitions: &[ToolDefinition],
        calls: &[ToolInvocation],
        cancellation: &CancellationToken,
    ) -> Result<Vec<ToolOutcome>, AgentLoopError> {
        let behavior = definitions
            .iter()
            .map(|definition| (definition.name.as_str(), definition.behavior))
            .collect::<BTreeMap<_, _>>();
        let mut outcomes = Vec::with_capacity(calls.len());
        let mut index = 0;
        while index < calls.len() {
            if cancellation.is_cancelled() {
                for call in &calls[index..] {
                    let mut failure = ToolFailure::not_committed(
                        ToolErrorCategory::Cancelled,
                        "TOOL_NOT_STARTED",
                        "cancelled_before_dispatch",
                        "tool call was durably requested but cancelled before dispatch",
                    );
                    failure.status = ToolFailureStatus::NotStarted;
                    failure.effect_state = ToolEffectState::NotStarted;
                    failure.retry.automatic = true;
                    failure.retry.reason = "The tool body did not start".into();
                    outcomes.push(self.finish_tool(
                        turn_id,
                        call,
                        Err(ToolExecutionError::typed(failure)),
                    )?);
                }
                break;
            }
            if behavior.get(calls[index].name.as_str()) == Some(&ToolBehavior::ReadOnly) {
                let end = calls[index..]
                    .iter()
                    .position(|call| {
                        behavior.get(call.name.as_str()) != Some(&ToolBehavior::ReadOnly)
                    })
                    .map_or(calls.len(), |offset| index + offset);
                for chunk in calls[index..end].chunks(self.config.max_parallel_reads.max(1)) {
                    for call in chunk {
                        self.emit(AgentEventKind::ToolStarted {
                            turn_id: turn_id.clone(),
                            call_id: call.call_id.clone(),
                            name: call.name.clone(),
                        })?;
                    }
                    let results = std::thread::scope(|scope| {
                        chunk
                            .iter()
                            .map(|call| scope.spawn(|| self.tools.execute(call, cancellation)))
                            .collect::<Vec<_>>()
                            .into_iter()
                            .map(|handle| {
                                handle
                                    .join()
                                    .map_err(|_| AgentLoopError::ToolWorkerPanicked)
                            })
                            .collect::<Result<Vec<_>, _>>()
                    })?;
                    for (call, result) in chunk.iter().zip(results) {
                        outcomes.push(self.finish_tool(turn_id, call, result)?);
                    }
                }
                index = end;
            } else {
                let call = &calls[index];
                self.emit(AgentEventKind::ToolStarted {
                    turn_id: turn_id.clone(),
                    call_id: call.call_id.clone(),
                    name: call.name.clone(),
                })?;
                let result =
                    catch_unwind(AssertUnwindSafe(|| self.tools.execute(call, cancellation)))
                        .map_err(|_| AgentLoopError::ToolWorkerPanicked)?;
                outcomes.push(self.finish_tool(turn_id, call, result)?);
                index += 1;
            }
        }
        Ok(outcomes)
    }

    fn finish_tool(
        &mut self,
        turn_id: &TurnId,
        call: &ToolInvocation,
        result: Result<Vec<u8>, ToolExecutionError>,
    ) -> Result<ToolOutcome, AgentLoopError> {
        let (bytes, is_error, failure) = match result {
            Ok(bytes) => (bytes, false, None),
            Err(error) => (error.message.into_bytes(), true, Some(*error.failure)),
        };
        let (content, artifact_id) = if bytes.len() > self.config.inline_tool_output_bytes {
            let spilled = self.spill.spill(&bytes)?;
            (
                format!(
                    "Tool output stored as artifact {} ({} bytes, {}). Preview:\n{}",
                    spilled.artifact_id, spilled.bytes, spilled.media_type, spilled.preview
                ),
                Some(spilled.artifact_id),
            )
        } else {
            (String::from_utf8_lossy(&bytes).into_owned(), None)
        };
        let mut outcome = ToolOutcome {
            call_id: call.call_id.clone(),
            name: call.name.clone(),
            content,
            is_error,
            artifact_id: artifact_id.clone(),
            arguments: call.arguments.clone(),
            failure,
            entry_id: None,
        };
        outcome.entry_id = Some(self.commit_tool(call, &outcome)?);
        self.emit(AgentEventKind::ToolCompleted {
            turn_id: turn_id.clone(),
            call_id: call.call_id.clone(),
            name: call.name.clone(),
            is_error,
            artifact_id,
        })?;
        Ok(outcome)
    }

    fn commit_unknown_tool_results(
        &mut self,
        turn_id: &TurnId,
        calls: &[ToolInvocation],
    ) -> Result<(), AgentLoopError> {
        let completed = self
            .session
            .active_ancestry()?
            .into_iter()
            .filter_map(|entry| match entry.payload {
                SessionEntryPayload::ToolResult { call_id, .. } => Some(call_id),
                _ => None,
            })
            .collect::<BTreeSet<_>>();
        for call in calls
            .iter()
            .filter(|call| !completed.contains(&call.call_id))
        {
            let mut failure = ToolFailure::execution(
                "the tool scheduler stopped before a terminal result became durable",
                false,
            );
            failure.error.code = "TOOL_OUTCOME_UNKNOWN".into();
            failure.error.reason = "tool_outcome_unknown".into();
            failure.retry.reason =
                "Inspect external state before deciding whether the operation can be retried"
                    .into();
            self.finish_tool(turn_id, call, Err(ToolExecutionError::typed(failure)))?;
        }
        Ok(())
    }

    fn commit_model_change(&mut self, provider: &str, model: &str) -> Result<(), AgentLoopError> {
        self.append(SessionEntryPayload::ModelChanged {
            provider: provider.into(),
            model: model.into(),
        })
    }

    fn commit_assistant_activity(
        &mut self,
        turn_id: &TurnId,
        completed: &CompletedMessage,
    ) -> Result<CommittedActivity, AgentLoopError> {
        let mut content = Vec::new();
        if !completed.text.is_empty() {
            content.push(StoredContentBlock::Text {
                text: completed.text.clone(),
            });
        }
        if !completed.reasoning.is_empty() {
            content.push(StoredContentBlock::Reasoning {
                text: completed.reasoning.clone(),
                visibility: keith_session_store::ReasoningVisibility::Hidden,
            });
        }
        let activity_entry_id = self.append_entry(SessionEntryPayload::AssistantActivity {
            turn_id: turn_id.clone(),
            message: StoredMessage {
                role: StoredMessageRole::Assistant,
                content,
                provider_metadata: BTreeMap::new(),
            },
        })?;
        let mut call_entry_ids = BTreeMap::new();
        for call in &completed.calls {
            let entry_id = self.append_entry(SessionEntryPayload::ToolCall {
                call_id: call.call_id.clone(),
                name: call.name.clone(),
                arguments: call.arguments.clone(),
            })?;
            call_entry_ids.insert(call.call_id.clone(), entry_id);
        }
        Ok(CommittedActivity {
            entry_id: activity_entry_id,
            call_entry_ids,
            session_id: self.session.manifest().session_id.clone(),
            turn_id: turn_id.clone(),
        })
    }

    fn commit_final_candidate(
        &mut self,
        turn_id: &TurnId,
        completed: &CompletedMessage,
        usage: Usage,
    ) -> Result<EntryId, AgentLoopError> {
        let mut content = vec![StoredContentBlock::Text {
            text: completed.text.clone(),
        }];
        if !completed.reasoning.is_empty() {
            content.push(StoredContentBlock::Reasoning {
                text: completed.reasoning.clone(),
                visibility: keith_session_store::ReasoningVisibility::Hidden,
            });
        }
        let candidate = self.session.append_final_candidate(
            UtcTimestamp::now()?,
            turn_id.clone(),
            StoredMessage {
                role: StoredMessageRole::Assistant,
                content,
                provider_metadata: BTreeMap::new(),
            },
            usage.input_tokens,
            usage.output_tokens,
            usage.cached_input_tokens,
        )?;
        Ok(candidate.id)
    }

    fn commit_tool(
        &mut self,
        call: &ToolInvocation,
        outcome: &ToolOutcome,
    ) -> Result<keith_agent_types::EntryId, AgentLoopError> {
        let content = if let Some(artifact_id) = &outcome.artifact_id {
            vec![StoredContentBlock::Artifact {
                artifact_id: artifact_id.clone(),
                media_type: "application/octet-stream".into(),
            }]
        } else {
            vec![StoredContentBlock::Text {
                text: outcome.content.clone(),
            }]
        };
        let entry_id = self.append_entry(SessionEntryPayload::ToolResult {
            call_id: call.call_id.clone(),
            content,
            is_error: outcome.is_error,
            failure: outcome.failure.clone(),
        })?;
        Ok(entry_id)
    }

    fn add_controller_guidance(
        &mut self,
        request: &mut ModelRequest,
        turn_id: &TurnId,
        source_id: &str,
        text: &str,
    ) -> Result<(), AgentLoopError> {
        let entry_id = self.append_entry(SessionEntryPayload::ControllerGuidance {
            turn_id: turn_id.clone(),
            source_id: source_id.into(),
            text: text.to_owned(),
        })?;
        request.system.push(ContentBlock::Text {
            text: format!(
                "<controller_guidance source=\"{source_id}\">{text}</controller_guidance>"
            ),
        });
        request.context.system.push(ContextRecord {
            session_id: self.session.manifest().session_id.clone(),
            turn_id: turn_id.clone(),
            entry_id,
            source_id: source_id.into(),
            provenance: ContextProvenance::ControllerGuidance,
            current_turn: true,
            persist_policy: PersistPolicy::Session,
            model_visibility: ModelVisibility::Visible,
        });
        request.request_id = EntityId::new();
        Ok(())
    }

    fn commit_usage(&mut self, usage: Usage) -> Result<(), AgentLoopError> {
        self.append(SessionEntryPayload::Usage {
            input_tokens: usage.input_tokens,
            output_tokens: usage.output_tokens,
            cost_micros: None,
        })
    }

    fn append(&mut self, payload: SessionEntryPayload) -> Result<(), AgentLoopError> {
        self.append_entry(payload)?;
        Ok(())
    }

    fn append_entry(
        &mut self,
        payload: SessionEntryPayload,
    ) -> Result<keith_agent_types::EntryId, AgentLoopError> {
        let parent = self.session.manifest().active_leaf.clone();
        let entry = self.session.append(parent, UtcTimestamp::now()?, payload)?;
        Ok(entry.id)
    }

    fn finish(&mut self, outcome: AgentOutcome) -> Result<(), AgentLoopError> {
        self.emit(AgentEventKind::AgentEnded { outcome })
    }

    fn emit(&mut self, kind: AgentEventKind) -> Result<(), AgentLoopError> {
        self.sequence = self
            .sequence
            .checked_add(1)
            .ok_or(AgentLoopError::SequenceOverflow)?;
        let event = AgentEvent {
            sequence: self.sequence,
            kind,
        };
        for subscriber in &mut self.subscribers {
            subscriber.on_event(&event);
        }
        Ok(())
    }
}

#[derive(Default)]
struct StreamAssembly {
    text: String,
    reasoning: String,
    calls: Vec<ToolInvocation>,
    partial: BTreeMap<ToolCallId, PartialCall>,
    stop: Option<StopReason>,
}

#[derive(Clone, Default)]
struct PartialCall {
    name: String,
    arguments: String,
}

#[derive(Clone, Debug, PartialEq)]
struct CompletedMessage {
    text: String,
    reasoning: String,
    calls: Vec<ToolInvocation>,
}

impl StreamAssembly {
    fn accept(&mut self, event: ModelEvent) {
        match event {
            ModelEvent::TextDelta { text } => self.text.push_str(&text),
            ModelEvent::ReasoningDelta { text } => self.reasoning.push_str(&text),
            ModelEvent::ToolCallStarted { id, name } => {
                let id = if self.partial.contains_key(&id)
                    || self.calls.iter().any(|call| call.call_id == id)
                {
                    ToolCallId::new()
                } else {
                    id
                };
                self.partial.insert(
                    id,
                    PartialCall {
                        name,
                        arguments: String::new(),
                    },
                );
            }
            ModelEvent::ToolCallArgumentsDelta { id, delta } => {
                self.partial
                    .entry(id)
                    .or_default()
                    .arguments
                    .push_str(&delta);
            }
            ModelEvent::ToolCallCompleted {
                id,
                name,
                arguments,
            } => {
                self.partial.remove(&id);
                let id = if self.calls.iter().any(|call| call.call_id == id) {
                    ToolCallId::new()
                } else {
                    id
                };
                self.calls.push(ToolInvocation {
                    call_id: id,
                    name,
                    arguments,
                });
            }
            ModelEvent::Finished { reason } => self.stop = Some(reason),
            ModelEvent::Started { .. } | ModelEvent::Usage { .. } => {}
        }
    }

    fn finish(mut self) -> Result<CompletedMessage, AgentLoopError> {
        if self.stop.is_none() {
            return Err(AgentLoopError::MalformedTool(
                "stream ended without a stop reason".into(),
            ));
        }
        for (id, partial) in self.partial {
            if partial.name.trim().is_empty() {
                return Err(AgentLoopError::MalformedTool(
                    "tool arguments arrived without a tool name".into(),
                ));
            }
            let arguments = repair_json_arguments(&partial.arguments)?;
            self.calls.push(ToolInvocation {
                call_id: id,
                name: partial.name,
                arguments,
            });
        }
        Ok(CompletedMessage {
            text: self.text,
            reasoning: self.reasoning,
            calls: self.calls,
        })
    }
}

#[derive(Clone, Debug)]
struct ToolOutcome {
    call_id: ToolCallId,
    name: String,
    content: String,
    is_error: bool,
    artifact_id: Option<ArtifactId>,
    arguments: serde_json::Value,
    failure: Option<ToolFailure>,
    entry_id: Option<keith_agent_types::EntryId>,
}

struct CommittedActivity {
    entry_id: keith_agent_types::EntryId,
    call_entry_ids: BTreeMap<ToolCallId, keith_agent_types::EntryId>,
    session_id: keith_agent_types::SessionId,
    turn_id: TurnId,
}

fn tool_failure_fingerprint(outcome: &ToolOutcome) -> String {
    let arguments = canonical_json_bytes(&outcome.arguments).map_or_else(
        |_| "<invalid-canonical-arguments>".into(),
        |bytes| String::from_utf8_lossy(&bytes).into_owned(),
    );
    let failure = outcome.failure.as_ref();
    format!(
        "{}|{}|{:?}|{}|{:?}",
        outcome.name,
        arguments,
        failure.map(|value| value.error.category),
        failure.map_or("UNCLASSIFIED", |value| value.error.code.as_str()),
        failure.map(|value| value.effect_state),
    )
}

fn repair_json_arguments(raw: &str) -> Result<serde_json::Value, AgentLoopError> {
    if raw.trim().is_empty() {
        return Ok(serde_json::Value::Object(serde_json::Map::new()));
    }
    if let Ok(value) = serde_json::from_str(raw) {
        return Ok(value);
    }
    let opens = raw.chars().filter(|character| *character == '{').count();
    let closes = raw.chars().filter(|character| *character == '}').count();
    if opens > closes {
        let mut repaired = raw.to_owned();
        repaired.extend(std::iter::repeat_n('}', opens - closes));
        if let Ok(value) = serde_json::from_str(&repaired) {
            return Ok(value);
        }
    }
    Err(AgentLoopError::MalformedTool(
        "tool arguments are not recoverable JSON".into(),
    ))
}

fn append_tool_exchange(
    request: &mut ModelRequest,
    message: &CompletedMessage,
    activity: &CommittedActivity,
    outcomes: &[ToolOutcome],
) {
    let mut assistant = Vec::new();
    let mut assistant_context = Vec::new();
    if !message.text.is_empty() {
        assistant.push(ContentBlock::Text {
            text: message.text.clone(),
        });
        assistant_context.push(ContextRecord {
            session_id: activity.session_id.clone(),
            turn_id: activity.turn_id.clone(),
            entry_id: activity.entry_id.clone(),
            source_id: "assistant_activity".into(),
            provenance: ContextProvenance::AssistantCommentary,
            current_turn: true,
            persist_policy: PersistPolicy::Session,
            model_visibility: ModelVisibility::Visible,
        });
    }
    for call in &message.calls {
        assistant.push(ContentBlock::ToolCall {
            id: call.call_id.clone(),
            name: call.name.clone(),
            arguments: call.arguments.clone(),
        });
        assistant_context.push(ContextRecord {
            session_id: activity.session_id.clone(),
            turn_id: activity.turn_id.clone(),
            entry_id: activity
                .call_entry_ids
                .get(&call.call_id)
                .expect("every committed call has an entry")
                .clone(),
            source_id: call.call_id.to_string(),
            provenance: ContextProvenance::ToolCall,
            current_turn: true,
            persist_policy: PersistPolicy::Session,
            model_visibility: ModelVisibility::Visible,
        });
    }
    request.messages.push(Message {
        role: MessageRole::Assistant,
        content: assistant,
    });
    request.context.messages.push(assistant_context);
    request.messages.push(Message {
        role: MessageRole::Tool,
        content: outcomes
            .iter()
            .map(|outcome| ContentBlock::ToolResult {
                call_id: outcome.call_id.clone(),
                content: outcome.content.clone(),
                is_error: outcome.is_error,
            })
            .collect(),
    });
    request.context.messages.push(
        outcomes
            .iter()
            .map(|outcome| ContextRecord {
                session_id: activity.session_id.clone(),
                turn_id: activity.turn_id.clone(),
                entry_id: outcome
                    .entry_id
                    .as_ref()
                    .expect("every tool outcome has a stored entry")
                    .clone(),
                source_id: outcome.call_id.to_string(),
                provenance: ContextProvenance::ToolResult,
                current_turn: true,
                persist_policy: PersistPolicy::Session,
                model_visibility: ModelVisibility::Visible,
            })
            .collect(),
    );
    request.request_id = EntityId::new();
}

fn is_context_overflow(error: &RegistryError) -> bool {
    matches!(
        error,
        RegistryError::Provider(provider)
            if provider.kind == keith_provider_core::ProviderErrorKind::ContextOverflow
    )
}

fn loop_error_as_provider(error: &AgentLoopError) -> keith_provider_core::ProviderError {
    keith_provider_core::ProviderError::new(
        keith_provider_core::ProviderErrorKind::Internal,
        error.to_string(),
    )
}

fn add_usage(total: &mut Usage, next: Usage) {
    total.input_tokens = total.input_tokens.saturating_add(next.input_tokens);
    total.output_tokens = total.output_tokens.saturating_add(next.output_tokens);
    total.cached_input_tokens = total
        .cached_input_tokens
        .saturating_add(next.cached_input_tokens);
}

#[cfg(test)]
mod tests {
    use std::collections::VecDeque;
    use std::fs;
    use std::io::{Read, Write};
    use std::net::{TcpListener, TcpStream};
    use std::process::Command;
    use std::sync::{Arc, Mutex};
    use std::thread;
    use std::time::Duration;

    use keith_agent_types::{Generation, RootTreeId, SessionId, WorkerId, WorkspaceId};
    use keith_artifacts::{
        ArtifactLimits, ArtifactScope, ArtifactService, ArtifactSource, ArtifactSpill,
        RetentionPolicy,
    };
    use keith_model_registry::{ModelRoute, ModelSelection};
    use keith_provider_adapters::{OpenAiProvider, ProviderHttpConfig};
    use keith_provider_core::{
        ModelDescriptor, ModelEventSink, ModelProvider, ProviderCredential, ProviderError,
        ProviderErrorKind,
    };
    use keith_session_store::{NewSession, SessionKind, SessionStore, WriterIdentity};
    use serde_json::json;
    use tempfile::TempDir;

    use super::*;

    struct ScriptedResponse {
        events: Vec<ModelEvent>,
        terminal: Result<Usage, ProviderError>,
    }

    struct ScriptedProvider {
        responses: Mutex<VecDeque<ScriptedResponse>>,
    }

    impl ScriptedProvider {
        fn new(responses: Vec<ScriptedResponse>) -> Self {
            Self {
                responses: Mutex::new(responses.into()),
            }
        }
    }

    impl ModelProvider for ScriptedProvider {
        fn provider_id(&self) -> &'static str {
            "scripted"
        }

        fn list_models(
            &self,
            _credential: &ProviderCredential,
        ) -> Result<Vec<ModelDescriptor>, ProviderError> {
            Ok(vec![ModelDescriptor {
                provider: "scripted".into(),
                id: "script-model".into(),
                display_name: "Script Model".into(),
                context_tokens: Some(100_000),
                output_tokens: Some(4_096),
                supports_tools: true,
                supports_reasoning: true,
                supports_vision: false,
            }])
        }

        fn stream(
            &self,
            _request: &ModelRequest,
            _credential: &ProviderCredential,
            cancellation: &CancellationToken,
            sink: &mut dyn ModelEventSink,
        ) -> Result<Usage, ProviderError> {
            cancellation.check()?;
            let response = self
                .responses
                .lock()
                .unwrap()
                .pop_front()
                .expect("script response");
            for event in response.events {
                cancellation.check()?;
                if sink.emit(event)? == StreamControl::Cancel {
                    return Err(ProviderError::new(
                        ProviderErrorKind::Cancelled,
                        "script cancelled",
                    ));
                }
            }
            response.terminal
        }

        fn count_tokens(
            &self,
            request: &ModelRequest,
            _credential: &ProviderCredential,
        ) -> Result<u64, ProviderError> {
            keith_provider_core::approximate_token_count(request)
        }

        fn cancel(&self, _request_id: &EntityId) -> Result<(), ProviderError> {
            Ok(())
        }
    }

    struct ProcessTool;

    impl ToolExecutor for ProcessTool {
        fn execute(
            &self,
            invocation: &ToolInvocation,
            cancellation: &CancellationToken,
        ) -> Result<Vec<u8>, ToolExecutionError> {
            if cancellation.is_cancelled() {
                return Err(ToolExecutionError::new("cancelled"));
            }
            if invocation.name == "fail" {
                return Err(ToolExecutionError::new("stable failure"));
            }
            Command::new("sh")
                .args([
                    "-c",
                    "printf 'real-process-output-abcdefghijklmnopqrstuvwxyz'",
                ])
                .output()
                .map(|output| output.stdout)
                .map_err(|error| ToolExecutionError::new(error.to_string()))
        }
    }

    struct PanickingTool;

    impl ToolExecutor for PanickingTool {
        fn execute(
            &self,
            _invocation: &ToolInvocation,
            _cancellation: &CancellationToken,
        ) -> Result<Vec<u8>, ToolExecutionError> {
            panic!("simulated tool worker crash");
        }
    }

    fn response(events: Vec<ModelEvent>) -> ScriptedResponse {
        ScriptedResponse {
            events,
            terminal: Ok(Usage {
                input_tokens: 5,
                output_tokens: 3,
                cached_input_tokens: 0,
            }),
        }
    }

    fn text_response(text: &str) -> ScriptedResponse {
        response(vec![
            ModelEvent::Started {
                provider_request_id: None,
                model: "script-model".into(),
            },
            ModelEvent::TextDelta { text: text.into() },
            ModelEvent::Finished {
                reason: StopReason::EndTurn,
            },
        ])
    }

    fn request(tools: Vec<ToolDefinition>) -> ModelRequest {
        let system = vec![ContentBlock::Text {
            text: "system".into(),
        }];
        let messages = vec![Message {
            role: MessageRole::User,
            content: vec![ContentBlock::Text {
                text: "work".into(),
            }],
        }];
        let context = keith_provider_core::RequestContext::synthetic(&system, &messages);
        ModelRequest {
            request_id: EntityId::new(),
            purpose: keith_provider_core::ModelRequestPurpose::Primary,
            model: "script-model".into(),
            system,
            messages,
            tools,
            max_output_tokens: Some(1_024),
            temperature: None,
            reasoning_effort: None,
            context,
        }
    }

    fn tool_definition(name: &str, behavior: ToolBehavior) -> ToolDefinition {
        ToolDefinition {
            name: name.into(),
            description: "test tool".into(),
            input_schema: json!({"type":"object"}),
            behavior,
        }
    }

    fn registry(provider: Arc<dyn ModelProvider>, profile_id: &ProfileId) -> ModelRegistry {
        let registry = ModelRegistry::new();
        let provider_id = provider.provider_id().to_owned();
        registry.register_provider(provider).unwrap();
        registry
            .refresh_models(
                &provider_id,
                &ProviderCredential::new("test-credential").unwrap(),
            )
            .unwrap();
        registry
            .set_profile_route(
                profile_id.clone(),
                ModelRoute {
                    primary: ModelSelection {
                        model: if provider_id == "openai" {
                            "model-a".into()
                        } else {
                            "script-model".into()
                        },
                        provider: provider_id,
                        credential_ref: Some("test".into()),
                    },
                    fallbacks: Vec::new(),
                    classification: None,
                    summarization: None,
                    review: None,
                    vision: None,
                },
            )
            .unwrap();
        registry
    }

    fn session() -> (TempDir, SessionStore, SessionId, ProfileId, SessionWriter) {
        let directory = tempfile::tempdir().unwrap();
        let store = SessionStore::open(directory.path()).unwrap();
        let session_id = SessionId::new();
        let profile_id = ProfileId::new();
        store
            .create(NewSession {
                kind: SessionKind::Root,
                session_id: session_id.clone(),
                root_tree_id: RootTreeId::new(),
                parent_session_id: None,
                profile_id: profile_id.clone(),
                workspace_id: WorkspaceId::new(),
                created_at: UtcTimestamp::UNIX_EPOCH,
                label: None,
                profile_snapshot: None,
            })
            .unwrap();
        let writer = store
            .acquire_writer(
                &session_id,
                WriterIdentity {
                    worker_id: WorkerId::new(),
                    owner_instance: EntityId::new(),
                    generation: Generation::new(1),
                    acquired_at: UtcTimestamp::UNIX_EPOCH,
                },
            )
            .unwrap();
        (directory, store, session_id, profile_id, writer)
    }

    fn credential(
        _provider: &str,
        _reference: Option<&str>,
    ) -> Result<ProviderCredential, ProviderError> {
        ProviderCredential::new("test-credential")
    }

    fn test_spill(path: &std::path::Path) -> ArtifactSpill {
        Arc::new(ArtifactService::open(path, ArtifactLimits::default()).unwrap()).scoped_spill(
            ArtifactScope {
                root_tree_id: RootTreeId::new(),
                session_id: SessionId::new(),
                profile_id: ProfileId::new(),
            },
            ArtifactSource::Tool,
            "auto",
            RetentionPolicy::Retain,
        )
    }

    fn final_candidate_text(
        store: &SessionStore,
        session_id: &SessionId,
        candidate_id: &EntryId,
    ) -> String {
        let index = store.load_index(session_id).unwrap();
        let entry = index.get(candidate_id).expect("durable final candidate");
        let SessionEntryPayload::AssistantFinalCandidate { message, .. } = &entry.payload else {
            panic!("result must cite an assistant final candidate");
        };
        message
            .content
            .iter()
            .find_map(|block| match block {
                keith_session_store::ContentBlock::Text { text } => Some(text.clone()),
                keith_session_store::ContentBlock::Reasoning { .. }
                | keith_session_store::ContentBlock::Artifact { .. }
                | keith_session_store::ContentBlock::Resource { .. } => None,
            })
            .expect("candidate text")
    }

    #[test]
    fn text_turn_commits_only_after_complete_stream_and_orders_subscribers() {
        let provider = Arc::new(ScriptedProvider::new(vec![text_response("hello")])) as Arc<_>;
        let (_directory, store, session_id, profile_id, mut writer) = session();
        let registry = registry(provider, &profile_id);
        let artifacts = tempfile::tempdir().unwrap();
        let spill = test_spill(artifacts.path());
        let observed = Arc::new(Mutex::new(Vec::new()));
        let first = Arc::clone(&observed);
        let second = Arc::clone(&observed);
        let mut loop_ = AgentLoop::new(
            &registry,
            &profile_id,
            &credential,
            &ProcessTool,
            &spill,
            &NoCompaction,
            &NoSteering,
            &mut writer,
            AgentLoopConfig::default(),
        );
        loop_.subscribe(move |event: &AgentEvent| {
            first.lock().unwrap().push((1_u8, event.sequence));
        });
        loop_.subscribe(move |event: &AgentEvent| {
            second.lock().unwrap().push((2_u8, event.sequence));
        });
        let result = loop_
            .run(request(Vec::new()), &CancellationToken::default())
            .unwrap();
        drop(loop_);
        drop(writer);
        assert_eq!(
            final_candidate_text(&store, &session_id, &result.final_candidate_id),
            "hello"
        );
        let pairs = observed.lock().unwrap();
        assert!(
            pairs
                .chunks_exact(2)
                .all(|pair| { pair[0].0 == 1 && pair[1].0 == 2 && pair[0].1 == pair[1].1 })
        );
        let manifest = store.manifest(&session_id).unwrap();
        let ancestry = store
            .load_index(&session_id)
            .unwrap()
            .ancestry(manifest.active_leaf.as_ref().unwrap())
            .unwrap();
        assert!(ancestry.iter().any(|entry| matches!(
            &entry.payload,
            SessionEntryPayload::AssistantFinalCandidate { .. }
        )));
        assert!(!ancestry.iter().any(|entry| matches!(
            &entry.payload,
            SessionEntryPayload::AssistantMessage { .. }
                | SessionEntryPayload::AssistantFinal { .. }
                | SessionEntryPayload::AssistantActivity { .. }
        )));
    }

    #[test]
    fn failed_partial_stream_is_incomplete_and_never_enters_history() {
        let provider = Arc::new(ScriptedProvider::new(vec![ScriptedResponse {
            events: vec![ModelEvent::TextDelta {
                text: "partial".into(),
            }],
            terminal: Err(ProviderError::new(
                ProviderErrorKind::MalformedResponse,
                "broken stream",
            )),
        }])) as Arc<_>;
        let (_directory, store, session_id, profile_id, mut writer) = session();
        let registry = registry(provider, &profile_id);
        let artifacts = tempfile::tempdir().unwrap();
        let spill = test_spill(artifacts.path());
        let events = Arc::new(Mutex::new(Vec::new()));
        let captured = Arc::clone(&events);
        let mut loop_ = AgentLoop::new(
            &registry,
            &profile_id,
            &credential,
            &ProcessTool,
            &spill,
            &NoCompaction,
            &NoSteering,
            &mut writer,
            AgentLoopConfig::default(),
        );
        loop_
            .subscribe(move |event: &AgentEvent| captured.lock().unwrap().push(event.kind.clone()));
        assert!(
            loop_
                .run(request(Vec::new()), &CancellationToken::default())
                .is_err()
        );
        drop(loop_);
        drop(writer);
        let manifest = store.manifest(&session_id).unwrap();
        let ancestry = store
            .load_index(&session_id)
            .unwrap()
            .ancestry(manifest.active_leaf.as_ref().unwrap())
            .unwrap();
        assert!(!ancestry.iter().any(|entry| matches!(
            entry.payload,
            SessionEntryPayload::AssistantFinalCandidate { .. }
                | SessionEntryPayload::AssistantMessage { .. }
                | SessionEntryPayload::AssistantActivity { .. }
                | SessionEntryPayload::AssistantFinal { .. }
        )));
        assert!(ancestry.iter().any(|entry| matches!(
            entry.payload,
            SessionEntryPayload::StepBoundary {
                state: StepBoundaryState::Failed,
                ..
            }
        )));
        assert!(events.lock().unwrap().iter().any(|event| matches!(
            event,
            AgentEventKind::MessageCompleted {
                complete: false,
                ..
            }
        )));
    }

    #[test]
    fn repairs_partial_tool_json_runs_real_process_and_spills_large_output() {
        let call_id = ToolCallId::new();
        let provider = Arc::new(ScriptedProvider::new(vec![
            response(vec![
                ModelEvent::ToolCallStarted {
                    id: call_id.clone(),
                    name: "process".into(),
                },
                ModelEvent::ToolCallArgumentsDelta {
                    id: call_id,
                    delta: "{\"query\":1".into(),
                },
                ModelEvent::Finished {
                    reason: StopReason::ToolUse,
                },
            ]),
            text_response("finished"),
        ])) as Arc<_>;
        let (_directory, store, session_id, profile_id, mut writer) = session();
        let registry = registry(provider, &profile_id);
        let artifacts = tempfile::tempdir().unwrap();
        let spill = test_spill(artifacts.path());
        let config = AgentLoopConfig {
            inline_tool_output_bytes: 8,
            ..AgentLoopConfig::default()
        };
        let mut loop_ = AgentLoop::new(
            &registry,
            &profile_id,
            &credential,
            &ProcessTool,
            &spill,
            &NoCompaction,
            &NoSteering,
            &mut writer,
            config,
        );
        let result = loop_
            .run(
                request(vec![tool_definition("process", ToolBehavior::ReadOnly)]),
                &CancellationToken::default(),
            )
            .unwrap();
        drop(loop_);
        drop(writer);
        assert_eq!(result.turns, 2);
        assert_eq!(fs::read_dir(artifacts.path()).unwrap().count(), 1);
        let manifest = store.manifest(&session_id).unwrap();
        let ancestry = store
            .load_index(&session_id)
            .unwrap()
            .ancestry(manifest.active_leaf.as_ref().unwrap())
            .unwrap();
        let call_position = ancestry
            .iter()
            .position(|entry| matches!(entry.payload, SessionEntryPayload::ToolCall { .. }))
            .unwrap();
        assert!(matches!(
            ancestry[call_position + 1].payload,
            SessionEntryPayload::ToolResult { .. }
        ));
    }

    #[test]
    fn overflow_without_durable_generation_advance_is_not_retried() {
        let overflow = ScriptedResponse {
            events: Vec::new(),
            terminal: Err(ProviderError::new(
                ProviderErrorKind::ContextOverflow,
                "context token limit exceeded",
            )),
        };
        let provider = Arc::new(ScriptedProvider::new(vec![
            overflow,
            text_response("after"),
        ])) as Arc<_>;
        let (_directory, _store, _session_id, profile_id, mut writer) = session();
        let first_registry = registry(provider, &profile_id);
        let artifacts = tempfile::tempdir().unwrap();
        let spill = test_spill(artifacts.path());
        let mut model_request = request(Vec::new());
        model_request.messages.extend([
            Message {
                role: MessageRole::Assistant,
                content: vec![ContentBlock::Text { text: "old".into() }],
            },
            Message {
                role: MessageRole::User,
                content: vec![ContentBlock::Text { text: "new".into() }],
            },
        ]);
        model_request.context = keith_provider_core::RequestContext::synthetic(
            &model_request.system,
            &model_request.messages,
        );
        let mut loop_ = AgentLoop::new(
            &first_registry,
            &profile_id,
            &credential,
            &ProcessTool,
            &spill,
            &NoCompaction,
            &NoSteering,
            &mut writer,
            AgentLoopConfig::default(),
        );
        assert!(matches!(
            loop_.run(model_request, &CancellationToken::default()),
            Err(AgentLoopError::ContextOverflow)
        ));
    }

    #[test]
    fn cancelled_run_stops_before_provider_or_final_candidate() {
        let artifacts = tempfile::tempdir().unwrap();
        let spill = test_spill(artifacts.path());

        let provider = Arc::new(ScriptedProvider::new(Vec::new())) as Arc<_>;
        let (_directory, store, session_id, profile_id, mut writer) = session();
        let registry = registry(provider, &profile_id);
        let cancellation = CancellationToken::default();
        cancellation.cancel();
        let mut loop_ = AgentLoop::new(
            &registry,
            &profile_id,
            &credential,
            &ProcessTool,
            &spill,
            &NoCompaction,
            &NoSteering,
            &mut writer,
            AgentLoopConfig::default(),
        );
        assert!(matches!(
            loop_.run(request(Vec::new()), &cancellation),
            Err(AgentLoopError::Cancelled)
        ));
        drop(loop_);
        drop(writer);
        let index = store.load_index(&session_id).unwrap();
        assert!(index.is_empty());
    }

    #[test]
    fn identical_tool_failures_stop_at_bound() {
        let tool_events = || {
            response(vec![
                ModelEvent::ToolCallCompleted {
                    id: ToolCallId::new(),
                    name: "fail".into(),
                    arguments: json!({}),
                },
                ModelEvent::Finished {
                    reason: StopReason::ToolUse,
                },
            ])
        };
        let provider = Arc::new(ScriptedProvider::new(vec![
            tool_events(),
            tool_events(),
            tool_events(),
        ])) as Arc<_>;
        let (_directory, store, session_id, profile_id, mut writer) = session();
        let registry = registry(provider, &profile_id);
        let artifacts = tempfile::tempdir().unwrap();
        let spill = test_spill(artifacts.path());
        let config = AgentLoopConfig {
            identical_failure_limit: 3,
            ..AgentLoopConfig::default()
        };
        let mut loop_ = AgentLoop::new(
            &registry,
            &profile_id,
            &credential,
            &ProcessTool,
            &spill,
            &NoCompaction,
            &NoSteering,
            &mut writer,
            config,
        );
        let result = loop_.run(
            request(vec![tool_definition("fail", ToolBehavior::StateChanging)]),
            &CancellationToken::default(),
        );
        assert!(matches!(result, Err(AgentLoopError::RepeatedFailure(_))));
        drop(loop_);
        drop(writer);
        let manifest = store.manifest(&session_id).unwrap();
        let ancestry = store
            .load_index(&session_id)
            .unwrap()
            .ancestry(manifest.active_leaf.as_ref().unwrap())
            .unwrap();
        assert!(
            !ancestry
                .iter()
                .any(|entry| matches!(entry.payload, SessionEntryPayload::UserMessage { .. }))
        );
        assert!(ancestry.iter().any(|entry| matches!(
            &entry.payload,
            SessionEntryPayload::ControllerGuidance { source_id, .. }
                if source_id == "repeated_tool_failure"
        )));
        assert!(ancestry.iter().any(|entry| matches!(
            &entry.payload,
            SessionEntryPayload::ToolResult {
                failure: Some(failure),
                ..
            } if failure.effect_state == keith_agent_types::ToolEffectState::Unknown
                && failure.error.code == "TOOL_EXECUTION_FAILED"
        )));
    }

    #[test]
    fn tool_worker_crash_pairs_the_call_with_unknown_outcome_and_closes_the_step() {
        let call_id = ToolCallId::new();
        let provider = Arc::new(ScriptedProvider::new(vec![response(vec![
            ModelEvent::ToolCallCompleted {
                id: call_id.clone(),
                name: "panic".into(),
                arguments: json!({"write": true}),
            },
            ModelEvent::Finished {
                reason: StopReason::ToolUse,
            },
        ])])) as Arc<_>;
        let (_directory, store, session_id, profile_id, mut writer) = session();
        let registry = registry(provider, &profile_id);
        let artifacts = tempfile::tempdir().unwrap();
        let spill = test_spill(artifacts.path());
        let mut loop_ = AgentLoop::new(
            &registry,
            &profile_id,
            &credential,
            &PanickingTool,
            &spill,
            &NoCompaction,
            &NoSteering,
            &mut writer,
            AgentLoopConfig::default(),
        );
        assert!(matches!(
            loop_.run(
                request(vec![tool_definition("panic", ToolBehavior::StateChanging)]),
                &CancellationToken::default(),
            ),
            Err(AgentLoopError::ToolWorkerPanicked)
        ));
        drop(loop_);
        drop(writer);
        let manifest = store.manifest(&session_id).unwrap();
        let ancestry = store
            .load_index(&session_id)
            .unwrap()
            .ancestry(manifest.active_leaf.as_ref().unwrap())
            .unwrap();
        assert_eq!(
            ancestry
                .iter()
                .filter(|entry| matches!(
                    &entry.payload,
                    SessionEntryPayload::ToolResult {
                        call_id: result_call,
                        is_error: true,
                        failure: Some(failure),
                        ..
                    } if result_call == &call_id
                        && failure.error.code == "TOOL_OUTCOME_UNKNOWN"
                        && failure.effect_state == ToolEffectState::Unknown
                ))
                .count(),
            1
        );
        assert!(ancestry.iter().any(|entry| matches!(
            entry.payload,
            SessionEntryPayload::StepBoundary {
                state: StepBoundaryState::Failed,
                ..
            }
        )));
    }

    #[test]
    fn twenty_seven_call_thirteen_error_replay_has_no_synthetic_user_or_retry_narration() {
        let mut responses = (0_u32..27)
            .map(|index| {
                let name = if index < 13 { "fail" } else { "process" };
                response(vec![
                    ModelEvent::ToolCallCompleted {
                        id: ToolCallId::new(),
                        name: name.into(),
                        arguments: json!({"replay_index": index}),
                    },
                    ModelEvent::Finished {
                        reason: StopReason::ToolUse,
                    },
                ])
            })
            .collect::<Vec<_>>();
        responses.push(text_response("one terminal replay answer"));
        let provider = Arc::new(ScriptedProvider::new(responses)) as Arc<_>;
        let (_directory, store, session_id, profile_id, mut writer) = session();
        let registry = registry(provider, &profile_id);
        let artifacts = tempfile::tempdir().unwrap();
        let spill = test_spill(artifacts.path());
        let mut loop_ = AgentLoop::new(
            &registry,
            &profile_id,
            &credential,
            &ProcessTool,
            &spill,
            &NoCompaction,
            &NoSteering,
            &mut writer,
            AgentLoopConfig::default(),
        );
        let result = loop_
            .run(
                request(vec![
                    tool_definition("fail", ToolBehavior::StateChanging),
                    tool_definition("process", ToolBehavior::ReadOnly),
                ]),
                &CancellationToken::default(),
            )
            .unwrap();
        assert_eq!(result.turns, 28);
        assert_eq!(
            final_candidate_text(&store, &session_id, &result.final_candidate_id),
            "one terminal replay answer"
        );
        drop(loop_);
        drop(writer);

        let manifest = store.manifest(&session_id).unwrap();
        let ancestry = store
            .load_index(&session_id)
            .unwrap()
            .ancestry(manifest.active_leaf.as_ref().unwrap())
            .unwrap();
        assert_eq!(
            ancestry
                .iter()
                .filter(|entry| matches!(entry.payload, SessionEntryPayload::ToolCall { .. }))
                .count(),
            27
        );
        assert_eq!(
            ancestry
                .iter()
                .filter(|entry| matches!(entry.payload, SessionEntryPayload::ToolResult { .. }))
                .count(),
            27
        );
        assert_eq!(
            ancestry
                .iter()
                .filter(|entry| matches!(
                    &entry.payload,
                    SessionEntryPayload::ToolResult {
                        is_error: true,
                        failure: Some(failure),
                        ..
                    } if !failure.success
                        && failure.status == keith_agent_types::ToolFailureStatus::Error
                        && failure.effect_state == keith_agent_types::ToolEffectState::Unknown
                ))
                .count(),
            13
        );
        assert!(!ancestry.iter().any(|entry| matches!(
            entry.payload,
            SessionEntryPayload::UserMessage { .. }
                | SessionEntryPayload::ControllerGuidance { .. }
                | SessionEntryPayload::AssistantFinal { .. }
        )));
    }

    struct TestServer {
        base_url: String,
        thread: Option<thread::JoinHandle<()>>,
    }

    impl TestServer {
        fn start(responses: Vec<String>) -> Self {
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let address = listener.local_addr().unwrap();
            let thread = thread::spawn(move || {
                for response in responses {
                    let (mut stream, _) = listener.accept().unwrap();
                    read_request(&mut stream);
                    stream.write_all(response.as_bytes()).unwrap();
                    stream.flush().unwrap();
                }
            });
            Self {
                base_url: format!("http://{address}"),
                thread: Some(thread),
            }
        }
    }

    impl Drop for TestServer {
        fn drop(&mut self) {
            if let Some(thread) = self.thread.take() {
                thread.join().unwrap();
            }
        }
    }

    fn read_request(stream: &mut TcpStream) {
        stream
            .set_read_timeout(Some(Duration::from_secs(5)))
            .unwrap();
        let mut bytes = Vec::new();
        let mut buffer = [0_u8; 4096];
        loop {
            let read = stream.read(&mut buffer).unwrap();
            if read == 0 {
                break;
            }
            bytes.extend_from_slice(&buffer[..read]);
            if let Some(end) = bytes.windows(4).position(|window| window == b"\r\n\r\n") {
                let headers = String::from_utf8_lossy(&bytes[..end + 4]);
                let length = headers
                    .lines()
                    .find_map(|line| {
                        line.to_ascii_lowercase()
                            .strip_prefix("content-length:")
                            .and_then(|value| value.trim().parse::<usize>().ok())
                    })
                    .unwrap_or(0);
                if bytes.len() >= end + 4 + length {
                    break;
                }
            }
        }
    }

    fn http_response(content_type: &str, body: &str) -> String {
        format!(
            "HTTP/1.1 200 OK\r\nContent-Type: {content_type}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
            body.len()
        )
    }

    #[test]
    fn real_openai_http_adapter_runs_through_the_loop() {
        let stream = concat!(
            "data: {\"choices\":[{\"delta\":{\"content\":\"from adapter\"},\"finish_reason\":null}]}\n\n",
            "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\n\n",
            "data: [DONE]\n\n"
        );
        let server = TestServer::start(vec![
            http_response("application/json", r#"{"data":[{"id":"model-a"}]}"#),
            http_response("text/event-stream", stream),
        ]);
        let provider = Arc::new(
            OpenAiProvider::new(ProviderHttpConfig::new(&server.base_url).unwrap()).unwrap(),
        ) as Arc<_>;
        let (_directory, store, session_id, profile_id, mut writer) = session();
        let registry = registry(provider, &profile_id);
        let artifacts = tempfile::tempdir().unwrap();
        let spill = test_spill(artifacts.path());
        let mut model_request = request(Vec::new());
        model_request.model = "model-a".into();
        let mut loop_ = AgentLoop::new(
            &registry,
            &profile_id,
            &credential,
            &ProcessTool,
            &spill,
            &NoCompaction,
            &NoSteering,
            &mut writer,
            AgentLoopConfig::default(),
        );
        let result = loop_
            .run(model_request, &CancellationToken::default())
            .unwrap();
        drop(loop_);
        drop(writer);
        assert_eq!(
            final_candidate_text(&store, &session_id, &result.final_candidate_id),
            "from adapter"
        );
    }
}
