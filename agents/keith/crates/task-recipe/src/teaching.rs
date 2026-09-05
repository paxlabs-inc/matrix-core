use std::collections::{BTreeMap, BTreeSet};

use keith_agent_types::{ProfileId, UtcTimestamp};
use keith_cua::{
    AccessibilityNode, ActionTarget as ComputerActionTarget, ComputerAction, ComputerObservation,
    DownloadState, MouseButton, Point, SemanticTarget as ComputerSemanticTarget,
};
use keith_platform_contracts::{ActionRisk, Capability, ComputerSessionId, ControlOwner};
use keith_skills::{SkillError, SkillPackage, SkillRegistry};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::{
    ApprovalRequirement, CaptureLimits, CaptureSanitizer, CapturedValue, ClipboardOperation,
    Demonstration, DemonstrationEvent, DemonstrationEventKind, DemonstrationState, FieldMetadata,
    FileOperation, FrameReference, KeyPhase, MediaSanitization, ObservationKind,
    ObservationMatcher, ParameterReference, ParameterSource, PointerAction, PointerButton,
    PointerInput, RawCaptureContext, RawDemonstrationEvent, RawDemonstrationEventKind,
    RawSemanticTarget, RawValue, RecipeAction, RecipeCheckpoint, RecipeInput, RecipeInputKind,
    RecipeStep, RecipeTarget, RecoveryBranch, Rectangle, RetentionPolicy, SemanticTargetSelector,
    SkillPublicationOptions, TaskRecipe, TaskRecipeError, TaskRecipeHistory, TaskRecipeStore,
    TemplateValue, VisualFallback,
};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct RecipeCompilerLimits {
    pub max_steps: usize,
    pub observation_timeout_ms: u64,
}

impl Default for RecipeCompilerLimits {
    fn default() -> Self {
        Self {
            max_steps: 2_048,
            observation_timeout_ms: 15_000,
        }
    }
}

pub struct DemonstrationRecorder {
    demonstration: Demonstration,
    sanitizer: CaptureSanitizer,
}

impl DemonstrationRecorder {
    #[allow(clippy::too_many_arguments)]
    pub fn start(
        store: &TaskRecipeStore,
        observation: &ComputerObservation,
        sanitized_frame_bytes: &[u8],
        frame_sanitization: MediaSanitization,
        control_owner: ControlOwner,
        title: impl Into<String>,
        retention: RetentionPolicy,
        limits: CaptureLimits,
        sanitizer: CaptureSanitizer,
    ) -> Result<Self, TaskRecipeError> {
        validate_observation(observation)?;
        let mut recorder = Self {
            demonstration: Demonstration::new(
                observation.profile_id.clone(),
                observation.computer_session_id.clone(),
                title,
                observation.captured_at,
                retention,
                limits,
            )?,
            sanitizer,
        };
        recorder.record_observation(
            store,
            observation,
            sanitized_frame_bytes,
            frame_sanitization,
            control_owner,
        )?;
        Ok(recorder)
    }

    pub fn demonstration(&self) -> &Demonstration {
        &self.demonstration
    }

    pub fn into_demonstration(self) -> Demonstration {
        self.demonstration
    }

    pub fn record_observation(
        &mut self,
        store: &TaskRecipeStore,
        observation: &ComputerObservation,
        sanitized_frame_bytes: &[u8],
        frame_sanitization: MediaSanitization,
        control_owner: ControlOwner,
    ) -> Result<(), TaskRecipeError> {
        let frame = self.prepare_frame(
            store,
            observation,
            sanitized_frame_bytes,
            frame_sanitization,
        )?;
        self.record_batch(
            store,
            vec![RawDemonstrationEvent {
                captured_at: observation.captured_at,
                context: raw_context(observation, frame.clone(), None, control_owner),
                kind: RawDemonstrationEventKind::FrameCaptured(frame),
            }],
        )
    }

    pub fn record_pointer(
        &mut self,
        store: &TaskRecipeStore,
        observation: &ComputerObservation,
        frame: FrameReference,
        input: PointerInput,
        semantic_target: Option<RawSemanticTarget>,
        control_owner: ControlOwner,
    ) -> Result<(), TaskRecipeError> {
        self.ensure_observation(observation)?;
        self.record_batch(
            store,
            vec![RawDemonstrationEvent {
                captured_at: observation.captured_at,
                context: raw_context(observation, frame, semantic_target, control_owner),
                kind: RawDemonstrationEventKind::Pointer(input),
            }],
        )
    }

    #[allow(clippy::too_many_arguments)]
    pub fn record_keyboard(
        &mut self,
        store: &TaskRecipeStore,
        observation: &ComputerObservation,
        frame: FrameReference,
        phase: KeyPhase,
        key: RawValue,
        code: impl Into<String>,
        modifiers: Vec<String>,
        field: FieldMetadata,
        semantic_target: Option<RawSemanticTarget>,
        control_owner: ControlOwner,
    ) -> Result<(), TaskRecipeError> {
        self.ensure_observation(observation)?;
        self.record_batch(
            store,
            vec![RawDemonstrationEvent {
                captured_at: observation.captured_at,
                context: raw_context(observation, frame, semantic_target, control_owner),
                kind: RawDemonstrationEventKind::Keyboard {
                    phase,
                    key,
                    code: code.into(),
                    modifiers,
                    field,
                },
            }],
        )
    }

    pub fn record_cua_action(
        &mut self,
        store: &TaskRecipeStore,
        observation: &ComputerObservation,
        sanitized_frame_bytes: &[u8],
        frame_sanitization: MediaSanitization,
        action: &ComputerAction,
        control_owner: ControlOwner,
    ) -> Result<(), TaskRecipeError> {
        validate_capturable_action(action)?;
        let frame = self.prepare_frame(
            store,
            observation,
            sanitized_frame_bytes,
            frame_sanitization,
        )?;
        let mut events = vec![RawDemonstrationEvent {
            captured_at: observation.captured_at,
            context: raw_context(observation, frame.clone(), None, control_owner),
            kind: RawDemonstrationEventKind::FrameCaptured(frame.clone()),
        }];
        events.extend(self.action_events(observation, &frame, action, control_owner)?);
        self.record_batch(store, events)
    }

    pub fn narrate(
        &mut self,
        store: &TaskRecipeStore,
        observation: &ComputerObservation,
        frame: FrameReference,
        narration: RawValue,
        control_owner: ControlOwner,
    ) -> Result<(), TaskRecipeError> {
        self.ensure_observation(observation)?;
        self.record_batch(
            store,
            vec![RawDemonstrationEvent {
                captured_at: observation.captured_at,
                context: raw_context(observation, frame, None, control_owner),
                kind: RawDemonstrationEventKind::Narration(narration),
            }],
        )
    }

    pub fn pause(
        &mut self,
        store: &TaskRecipeStore,
        observation: &ComputerObservation,
        frame: FrameReference,
        reason: RawValue,
        control_owner: ControlOwner,
    ) -> Result<(), TaskRecipeError> {
        self.ensure_observation(observation)?;
        self.record_batch(
            store,
            vec![RawDemonstrationEvent {
                captured_at: observation.captured_at,
                context: raw_context(observation, frame, None, control_owner),
                kind: RawDemonstrationEventKind::Pause { reason },
            }],
        )
    }

    pub fn resume(
        &mut self,
        store: &TaskRecipeStore,
        observation: &ComputerObservation,
        frame: FrameReference,
        control_owner: ControlOwner,
    ) -> Result<(), TaskRecipeError> {
        self.ensure_observation(observation)?;
        self.record_batch(
            store,
            vec![RawDemonstrationEvent {
                captured_at: observation.captured_at,
                context: raw_context(observation, frame, None, control_owner),
                kind: RawDemonstrationEventKind::Resume,
            }],
        )
    }

    pub fn control_changed(
        &mut self,
        store: &TaskRecipeStore,
        observation: &ComputerObservation,
        frame: FrameReference,
        owner: ControlOwner,
    ) -> Result<(), TaskRecipeError> {
        self.ensure_observation(observation)?;
        self.record_batch(
            store,
            vec![RawDemonstrationEvent {
                captured_at: observation.captured_at,
                context: raw_context(observation, frame, None, owner),
                kind: RawDemonstrationEventKind::ControlChanged(owner),
            }],
        )
    }

    pub fn complete(
        &mut self,
        store: &TaskRecipeStore,
        ended_at: UtcTimestamp,
    ) -> Result<(), TaskRecipeError> {
        let mut next = self.demonstration.clone();
        next.complete(ended_at)?;
        store.save_demonstration(&next)?;
        self.demonstration = next;
        Ok(())
    }

    #[allow(clippy::too_many_lines)]
    fn action_events(
        &mut self,
        observation: &ComputerObservation,
        frame: &FrameReference,
        action: &ComputerAction,
        owner: ControlOwner,
    ) -> Result<Vec<RawDemonstrationEvent>, TaskRecipeError> {
        let make_target = || {
            action_target(action)
                .and_then(|target| raw_semantic_target(target, observation))
                .or_else(|| direct_semantic_target(action, observation))
        };
        let event = |kind, semantic_target| RawDemonstrationEvent {
            captured_at: observation.captured_at,
            context: raw_context(observation, (*frame).clone(), semantic_target, owner),
            kind,
        };
        let pointer = |action, button, point: Point| PointerInput {
            action,
            button,
            x: point.x,
            y: point.y,
            delta_x: 0,
            delta_y: 0,
        };
        let point = target_point(action).unwrap_or(observation.cursor);
        let recorded_button = match action {
            ComputerAction::Click { button, .. } | ComputerAction::DoubleClick { button, .. } => {
                pointer_button(*button)
            }
            _ => PointerButton::Primary,
        };
        let events = match action {
            ComputerAction::Move { .. } => vec![event(
                RawDemonstrationEventKind::Pointer(pointer(
                    PointerAction::Move,
                    PointerButton::None,
                    point,
                )),
                make_target(),
            )],
            ComputerAction::Click { .. } => vec![
                event(
                    RawDemonstrationEventKind::Pointer(pointer(
                        PointerAction::ButtonDown,
                        recorded_button,
                        point,
                    )),
                    make_target(),
                ),
                event(
                    RawDemonstrationEventKind::Pointer(pointer(
                        PointerAction::ButtonUp,
                        recorded_button,
                        point,
                    )),
                    make_target(),
                ),
            ],
            ComputerAction::DoubleClick { .. } => (0..2)
                .flat_map(|_| {
                    [
                        event(
                            RawDemonstrationEventKind::Pointer(pointer(
                                PointerAction::ButtonDown,
                                recorded_button,
                                point,
                            )),
                            make_target(),
                        ),
                        event(
                            RawDemonstrationEventKind::Pointer(pointer(
                                PointerAction::ButtonUp,
                                recorded_button,
                                point,
                            )),
                            make_target(),
                        ),
                    ]
                })
                .collect(),
            ComputerAction::Scroll { delta_x, delta_y } => vec![event(
                RawDemonstrationEventKind::Pointer(PointerInput {
                    action: PointerAction::Scroll,
                    button: PointerButton::None,
                    x: observation.cursor.x,
                    y: observation.cursor.y,
                    delta_x: *delta_x,
                    delta_y: *delta_y,
                }),
                None,
            )],
            ComputerAction::Key { key } => vec![event(
                RawDemonstrationEventKind::Keyboard {
                    phase: KeyPhase::Down,
                    key: RawValue::new(key),
                    code: "KeyboardKey".into(),
                    modifiers: Vec::new(),
                    field: focused_field(observation),
                },
                make_target().or_else(|| focused_target(observation)),
            )],
            ComputerAction::Text { text } => vec![event(
                RawDemonstrationEventKind::Keyboard {
                    phase: KeyPhase::Down,
                    key: RawValue::new(text),
                    code: "TextInput".into(),
                    modifiers: Vec::new(),
                    field: focused_field(observation),
                },
                make_target().or_else(|| focused_target(observation)),
            )],
            ComputerAction::Shortcut { keys } => vec![event(
                RawDemonstrationEventKind::Keyboard {
                    phase: KeyPhase::Down,
                    key: RawValue::new(keys.join("+")),
                    code: "Shortcut".into(),
                    modifiers: keys.clone(),
                    field: FieldMetadata::named("shortcut"),
                },
                None,
            )],
            ComputerAction::ClipboardRead => vec![event(
                RawDemonstrationEventKind::Clipboard {
                    operation: ClipboardOperation::Read,
                    value: None,
                    field: FieldMetadata::named("clipboard"),
                },
                None,
            )],
            ComputerAction::ClipboardWrite { text } => vec![event(
                RawDemonstrationEventKind::Clipboard {
                    operation: ClipboardOperation::Write,
                    value: Some(RawValue::new(text)),
                    field: FieldMetadata::named("clipboard"),
                },
                focused_target(observation),
            )],
            ComputerAction::FileUpload { relative_path, .. } => vec![event(
                RawDemonstrationEventKind::File {
                    operation: FileOperation::Upload,
                    path: RawValue::new(relative_path),
                    field: FieldMetadata::named("upload_path"),
                    media: None,
                },
                make_target(),
            )],
            ComputerAction::Download {
                expected_file_name, ..
            } => vec![event(
                RawDemonstrationEventKind::File {
                    operation: FileOperation::Download,
                    path: RawValue::new(expected_file_name.as_deref().unwrap_or("downloaded-file")),
                    field: FieldMetadata::named("download_path"),
                    media: None,
                },
                make_target(),
            )],
            ComputerAction::Navigate { url }
            | ComputerAction::NewTab { url: Some(url) }
            | ComputerAction::NewWindow { url: Some(url) } => vec![event(
                RawDemonstrationEventKind::Navigate {
                    url: RawValue::new(url),
                },
                None,
            )],
            ComputerAction::Wait { duration_ms } => vec![event(
                RawDemonstrationEventKind::Wait {
                    duration_ms: *duration_ms,
                },
                None,
            )],
            ComputerAction::CredentialFill { grant, .. } => {
                let parameter = ParameterReference::new(
                    grant.grant_name.as_str(),
                    ParameterSource::NamedCredential,
                )?;
                let field_name = format!("credential-{}", parameter.name);
                self.sanitizer
                    .bind_parameter(&field_name, parameter.clone())?;
                vec![event(
                    RawDemonstrationEventKind::Keyboard {
                        phase: KeyPhase::Down,
                        key: RawValue::new(grant.grant_name.as_str()),
                        code: "CredentialFill".into(),
                        modifiers: Vec::new(),
                        field: FieldMetadata::named(field_name),
                    },
                    make_target(),
                )]
            }
            ComputerAction::Drag {
                from,
                to,
                duration_ms: _,
            } => {
                let from_point = action_target_point(from).unwrap_or(observation.cursor);
                let to_point = action_target_point(to).unwrap_or(observation.cursor);
                vec![
                    event(
                        RawDemonstrationEventKind::Pointer(pointer(
                            PointerAction::ButtonDown,
                            PointerButton::Primary,
                            from_point,
                        )),
                        raw_semantic_target(from, observation),
                    ),
                    event(
                        RawDemonstrationEventKind::Pointer(pointer(
                            PointerAction::Move,
                            PointerButton::Primary,
                            to_point,
                        )),
                        raw_semantic_target(to, observation),
                    ),
                    event(
                        RawDemonstrationEventKind::Pointer(pointer(
                            PointerAction::ButtonUp,
                            PointerButton::Primary,
                            to_point,
                        )),
                        raw_semantic_target(to, observation),
                    ),
                ]
            }
            ComputerAction::NewTab { url: None }
            | ComputerAction::CloseTab
            | ComputerAction::SwitchTab { .. }
            | ComputerAction::NewWindow { url: None }
            | ComputerAction::CloseWindow
            | ComputerAction::FocusWindow { .. } => {
                return Err(TaskRecipeError::InvalidDemonstration(
                    "CUA action requires an explicit teaching adapter before it can be captured"
                        .into(),
                ));
            }
        };
        Ok(events)
    }

    fn prepare_frame(
        &self,
        store: &TaskRecipeStore,
        observation: &ComputerObservation,
        sanitized_frame_bytes: &[u8],
        frame_sanitization: MediaSanitization,
    ) -> Result<FrameReference, TaskRecipeError> {
        self.ensure_observation(observation)?;
        if self.demonstration.state != DemonstrationState::Recording {
            return Err(TaskRecipeError::InvalidState);
        }
        let media = store.put_media(
            sanitized_frame_bytes,
            observation.screenshot.media_type.clone(),
            frame_sanitization,
        )?;
        Ok(FrameReference {
            frame_id: observation.screenshot.frame_id.to_string(),
            media,
            width: observation.screenshot.width,
            height: observation.screenshot.height,
        })
    }

    fn ensure_observation(&self, observation: &ComputerObservation) -> Result<(), TaskRecipeError> {
        validate_observation(observation)?;
        if observation.profile_id != self.demonstration.profile_id
            || observation.computer_session_id != self.demonstration.computer_session_id
        {
            return Err(TaskRecipeError::InvalidDemonstration(
                "CUA observation belongs to another profile or computer session".into(),
            ));
        }
        Ok(())
    }

    fn record_batch(
        &mut self,
        store: &TaskRecipeStore,
        events: Vec<RawDemonstrationEvent>,
    ) -> Result<(), TaskRecipeError> {
        let mut next = self.demonstration.clone();
        for event in events {
            next.record(event, &self.sanitizer)?;
        }
        store.save_demonstration(&next)?;
        self.demonstration = next;
        Ok(())
    }
}

#[derive(Default)]
pub struct RecipeCompiler {
    limits: RecipeCompilerLimits,
}

impl RecipeCompiler {
    pub fn new(limits: RecipeCompilerLimits) -> Result<Self, TaskRecipeError> {
        if limits.max_steps == 0 || limits.observation_timeout_ms == 0 {
            return Err(TaskRecipeError::LimitExceeded(
                "recipe compiler limits must be positive".into(),
            ));
        }
        Ok(Self { limits })
    }

    pub fn compile(
        &self,
        demonstration: &Demonstration,
        compiled_at: UtcTimestamp,
    ) -> Result<TaskRecipeHistory, TaskRecipeError> {
        demonstration.validate()?;
        if demonstration.state != DemonstrationState::Completed {
            return Err(TaskRecipeError::InvalidState);
        }
        let mut steps = Vec::new();
        for (index, event) in demonstration.events().iter().enumerate() {
            let Some((title, action)) = compile_event(event)? else {
                continue;
            };
            if steps.len() >= self.limits.max_steps {
                return Err(TaskRecipeError::LimitExceeded(
                    "compiled recipe step ceiling exhausted".into(),
                ));
            }
            let step_number = steps.len() + 1;
            let expected = next_context_matcher(
                demonstration.events(),
                index,
                &title,
                self.limits.observation_timeout_ms,
            )?;
            let step_id = format!("step-{step_number}");
            steps.push(RecipeStep {
                id: step_id.clone(),
                title,
                approval: consequential_approval(&action),
                action,
                expected_observations: vec![expected.clone()],
                checkpoint: Some(RecipeCheckpoint {
                    name: format!("step-{step_number}-done"),
                    description: expected.description.clone(),
                    replayable: true,
                }),
                recovery: vec![RecoveryBranch {
                    when: expected,
                    retry_step_id: Some(step_id),
                    resume_checkpoint: None,
                    max_attempts: 2,
                }],
            });
        }
        if steps.is_empty() {
            return Err(TaskRecipeError::InvalidRecipe(
                "demonstration contains no compilable action".into(),
            ));
        }
        let inputs = collect_inputs(demonstration);
        let first = demonstration
            .events()
            .first()
            .ok_or_else(|| TaskRecipeError::InvalidRecipe("demonstration is empty".into()))?;
        let last = demonstration
            .events()
            .last()
            .ok_or_else(|| TaskRecipeError::InvalidRecipe("demonstration is empty".into()))?;
        let preconditions = vec![context_matcher(
            &first.context,
            "The demonstrated starting state is visible",
            self.limits.observation_timeout_ms,
        )?];
        let completion_conditions = vec![context_matcher(
            &last.context,
            "The demonstrated result is visible",
            self.limits.observation_timeout_ms,
        )?];
        let recipe = TaskRecipe::new(
            demonstration.id.clone(),
            demonstration.title.clone(),
            format!(
                "Replay the accepted procedure taught in demonstration {}.",
                demonstration.id
            ),
            inputs,
            preconditions,
            steps,
            completion_conditions,
            BTreeSet::from(["changed-layout-recovery".into(), "shadow-replay".into()]),
            compiled_at,
        )?;
        TaskRecipeHistory::new(recipe)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RecipeReplayMode {
    Shadow,
    ExplicitTest,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RecipeReplayState {
    Ready,
    WaitingForObservation,
    Passed,
    Failed,
    Cancelled,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum ReplayInputValue {
    Text(String),
    Url(String),
    File(String),
    CredentialName(String),
    Choice(String),
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum ReplayComputerCommand {
    Computer(ComputerAction),
    NamedCredentialFill {
        parameter_name: String,
        target: ComputerSemanticTarget,
    },
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ReplayInstruction {
    pub step_id: String,
    pub title: String,
    pub mode: RecipeReplayMode,
    pub commands: Vec<ReplayComputerCommand>,
    pub approval: Option<ApprovalRequirement>,
    pub expected_observations: Vec<ObservationMatcher>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ReplayCheck {
    pub description: String,
    pub passed: bool,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RecipeReplayComparison {
    pub recipe_revision: u64,
    pub step_id: Option<String>,
    pub checks: Vec<ReplayCheck>,
    pub suggested_targets: Vec<SemanticTargetSelector>,
    pub passed: bool,
    pub compared_at: UtcTimestamp,
}

impl RecipeReplayComparison {
    pub fn evidence_digest(&self) -> Result<String, TaskRecipeError> {
        let bytes = serde_json::to_vec(self)?;
        Ok(hex_digest(&bytes))
    }
}

pub struct RecipeReplay {
    recipe: TaskRecipe,
    profile_id: ProfileId,
    computer_session_id: ComputerSessionId,
    mode: RecipeReplayMode,
    inputs: BTreeMap<String, ReplayInputValue>,
    step_index: usize,
    state: RecipeReplayState,
    last_comparison: Option<RecipeReplayComparison>,
}

impl RecipeReplay {
    pub fn start(
        recipe: &TaskRecipe,
        profile_id: ProfileId,
        computer_session_id: ComputerSessionId,
        mode: RecipeReplayMode,
        inputs: BTreeMap<String, ReplayInputValue>,
    ) -> Result<Self, TaskRecipeError> {
        Self::from_step(recipe, profile_id, computer_session_id, mode, inputs, 0)
    }

    pub fn from_checkpoint(
        recipe: &TaskRecipe,
        profile_id: ProfileId,
        computer_session_id: ComputerSessionId,
        mode: RecipeReplayMode,
        inputs: BTreeMap<String, ReplayInputValue>,
        checkpoint: &str,
    ) -> Result<Self, TaskRecipeError> {
        let index = recipe
            .steps
            .iter()
            .position(|step| {
                step.checkpoint
                    .as_ref()
                    .is_some_and(|candidate| candidate.name == checkpoint && candidate.replayable)
            })
            .ok_or(TaskRecipeError::NotFound)?
            .saturating_add(1);
        Self::from_step(recipe, profile_id, computer_session_id, mode, inputs, index)
    }

    pub const fn state(&self) -> RecipeReplayState {
        self.state
    }

    pub fn last_comparison(&self) -> Option<&RecipeReplayComparison> {
        self.last_comparison.as_ref()
    }

    pub fn prepare_next(
        &mut self,
        observation: &ComputerObservation,
    ) -> Result<Option<ReplayInstruction>, TaskRecipeError> {
        self.ensure_observation(observation)?;
        if self.state != RecipeReplayState::Ready {
            return Err(TaskRecipeError::InvalidState);
        }
        if self.step_index >= self.recipe.steps.len() {
            let comparison = compare_matchers(
                self.recipe.revision,
                None,
                &self.recipe.completion_conditions,
                observation,
                &self.inputs,
                observation.captured_at,
            )?;
            self.state = if comparison.passed {
                RecipeReplayState::Passed
            } else {
                RecipeReplayState::Failed
            };
            self.last_comparison = Some(comparison);
            return Ok(None);
        }
        if self.step_index == 0 {
            let starting = compare_matchers(
                self.recipe.revision,
                None,
                &self.recipe.preconditions,
                observation,
                &self.inputs,
                observation.captured_at,
            )?;
            if !starting.passed {
                self.state = RecipeReplayState::Failed;
                self.last_comparison = Some(starting);
                return Ok(None);
            }
        }
        let step = &self.recipe.steps[self.step_index];
        match replay_commands(&step.action, observation, &self.inputs) {
            Ok(commands) => {
                self.state = RecipeReplayState::WaitingForObservation;
                Ok(Some(ReplayInstruction {
                    step_id: step.id.clone(),
                    title: step.title.clone(),
                    mode: self.mode,
                    commands,
                    approval: step.approval.clone(),
                    expected_observations: step.expected_observations.clone(),
                }))
            }
            Err(suggested_targets) => {
                self.state = RecipeReplayState::Failed;
                self.last_comparison = Some(RecipeReplayComparison {
                    recipe_revision: self.recipe.revision,
                    step_id: Some(step.id.clone()),
                    checks: vec![ReplayCheck {
                        description: "The recorded semantic target is available".into(),
                        passed: false,
                    }],
                    suggested_targets,
                    passed: false,
                    compared_at: observation.captured_at,
                });
                Ok(None)
            }
        }
    }

    pub fn observe_result(
        &mut self,
        observation: &ComputerObservation,
    ) -> Result<&RecipeReplayComparison, TaskRecipeError> {
        self.ensure_observation(observation)?;
        if self.state != RecipeReplayState::WaitingForObservation {
            return Err(TaskRecipeError::InvalidState);
        }
        let step = &self.recipe.steps[self.step_index];
        let comparison = compare_matchers(
            self.recipe.revision,
            Some(step.id.clone()),
            &step.expected_observations,
            observation,
            &self.inputs,
            observation.captured_at,
        )?;
        if comparison.passed {
            self.step_index = self.step_index.saturating_add(1);
            self.state = RecipeReplayState::Ready;
        } else {
            self.state = RecipeReplayState::Failed;
        }
        self.last_comparison = Some(comparison);
        self.last_comparison
            .as_ref()
            .ok_or_else(|| TaskRecipeError::InvalidRecipe("replay comparison disappeared".into()))
    }

    pub fn cancel(&mut self) -> Result<(), TaskRecipeError> {
        if matches!(
            self.state,
            RecipeReplayState::Passed | RecipeReplayState::Failed | RecipeReplayState::Cancelled
        ) {
            return Err(TaskRecipeError::InvalidState);
        }
        self.state = RecipeReplayState::Cancelled;
        Ok(())
    }

    pub fn record_qualification(
        &self,
        history: &mut TaskRecipeHistory,
        check: &str,
    ) -> Result<(), TaskRecipeError> {
        if self.state != RecipeReplayState::Passed {
            return Err(TaskRecipeError::PublicationNotReady);
        }
        let comparison = self
            .last_comparison
            .as_ref()
            .ok_or(TaskRecipeError::PublicationNotReady)?;
        let recipe = history.active_mut()?;
        if recipe.id != self.recipe.id || recipe.revision != self.recipe.revision {
            return Err(TaskRecipeError::InvalidRecipe(
                "replay evidence belongs to another recipe revision".into(),
            ));
        }
        recipe.qualification.record(
            check,
            comparison.passed,
            comparison.evidence_digest()?,
            comparison.compared_at,
        )
    }

    fn from_step(
        recipe: &TaskRecipe,
        profile_id: ProfileId,
        computer_session_id: ComputerSessionId,
        mode: RecipeReplayMode,
        inputs: BTreeMap<String, ReplayInputValue>,
        step_index: usize,
    ) -> Result<Self, TaskRecipeError> {
        recipe.validate()?;
        validate_replay_inputs(recipe, &inputs)?;
        if step_index > recipe.steps.len() {
            return Err(TaskRecipeError::InvalidRecipe(
                "checkpoint lies outside the recipe".into(),
            ));
        }
        Ok(Self {
            recipe: recipe.clone(),
            profile_id,
            computer_session_id,
            mode,
            inputs,
            step_index,
            state: RecipeReplayState::Ready,
            last_comparison: None,
        })
    }

    fn ensure_observation(&self, observation: &ComputerObservation) -> Result<(), TaskRecipeError> {
        validate_observation(observation)?;
        if observation.profile_id != self.profile_id
            || observation.computer_session_id != self.computer_session_id
        {
            return Err(TaskRecipeError::InvalidRecipe(
                "replay observation belongs to another profile or computer session".into(),
            ));
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum RecipePublicationMode {
    Install,
    Update { expected_digest: String },
}

pub struct RecipePublisher;

impl RecipePublisher {
    pub fn publish(
        history: &mut TaskRecipeHistory,
        registry: &SkillRegistry,
        options: SkillPublicationOptions,
        mode: RecipePublicationMode,
        now: UtcTimestamp,
    ) -> Result<SkillPackage, TaskRecipeError> {
        let publication = history.active()?.skill_publication(options)?;
        let package = match mode {
            RecipePublicationMode::Install => match publication.install(registry, now) {
                Ok(package) => package,
                Err(TaskRecipeError::Skill(SkillError::AlreadyExists)) => registry
                    .inspect(&publication.manifest.id, now)?
                    .effective
                    .filter(|package| {
                        package.source == publication.source
                            && package.provenance.origin == publication.origin
                    })
                    .ok_or(TaskRecipeError::Skill(SkillError::AlreadyExists))?,
                Err(error) => return Err(error),
            },
            RecipePublicationMode::Update { expected_digest } => {
                publication.update(registry, &expected_digest, now)?
            }
        };
        history.active_mut()?.mark_published(
            package.manifest.id.clone(),
            package.provenance.digest.clone(),
            now,
        )?;
        Ok(package)
    }
}

fn validate_observation(observation: &ComputerObservation) -> Result<(), TaskRecipeError> {
    observation.validate().map_err(|_| {
        TaskRecipeError::InvalidDemonstration("CUA observation is inconsistent".into())
    })
}

fn validate_capturable_action(action: &ComputerAction) -> Result<(), TaskRecipeError> {
    match action {
        ComputerAction::NewTab { url: None }
        | ComputerAction::CloseTab
        | ComputerAction::SwitchTab { .. }
        | ComputerAction::NewWindow { url: None }
        | ComputerAction::CloseWindow
        | ComputerAction::FocusWindow { .. } => Err(TaskRecipeError::InvalidDemonstration(
            "CUA action requires an explicit teaching adapter before it can be captured".into(),
        )),
        ComputerAction::CredentialFill { grant, .. } => {
            ParameterReference::new(grant.grant_name.as_str(), ParameterSource::NamedCredential)?;
            Ok(())
        }
        _ => Ok(()),
    }
}

fn raw_context(
    observation: &ComputerObservation,
    frame: FrameReference,
    semantic_target: Option<RawSemanticTarget>,
    control_owner: ControlOwner,
) -> RawCaptureContext {
    RawCaptureContext {
        frame: Some(frame),
        semantic_target,
        url: observation.url.as_deref().map(RawValue::new),
        window: observation
            .focused_window
            .as_ref()
            .map(|window| RawValue::new(&window.title)),
        application: observation
            .focused_window
            .as_ref()
            .map(|window| RawValue::new(&window.application)),
        control_owner,
    }
}

fn action_target(action: &ComputerAction) -> Option<&ComputerActionTarget> {
    match action {
        ComputerAction::Move { target }
        | ComputerAction::Click { target, .. }
        | ComputerAction::DoubleClick { target, .. } => Some(target),
        ComputerAction::Drag { from, .. } => Some(from),
        _ => None,
    }
}

fn target_point(action: &ComputerAction) -> Option<Point> {
    action_target(action).and_then(action_target_point)
}

fn action_target_point(target: &ComputerActionTarget) -> Option<Point> {
    match target {
        ComputerActionTarget::Coordinate { point, .. } => Some(*point),
        ComputerActionTarget::Semantic { .. } => None,
    }
}

fn direct_semantic_target(
    action: &ComputerAction,
    observation: &ComputerObservation,
) -> Option<RawSemanticTarget> {
    let (ComputerAction::FileUpload { target, .. }
    | ComputerAction::Download { target, .. }
    | ComputerAction::CredentialFill { target, .. }) = action
    else {
        return None;
    };
    Some(raw_cua_semantic_target(target, observation.cursor))
}

const fn pointer_button(button: MouseButton) -> PointerButton {
    match button {
        MouseButton::Left => PointerButton::Primary,
        MouseButton::Middle => PointerButton::Middle,
        MouseButton::Right => PointerButton::Secondary,
    }
}

fn raw_semantic_target(
    target: &ComputerActionTarget,
    observation: &ComputerObservation,
) -> Option<RawSemanticTarget> {
    let ComputerActionTarget::Semantic { target } = target else {
        return None;
    };
    Some(raw_cua_semantic_target(target, observation.cursor))
}

fn raw_cua_semantic_target(target: &ComputerSemanticTarget, cursor: Point) -> RawSemanticTarget {
    let (role, name, stable_attributes) = match target {
        ComputerSemanticTarget::Accessibility { role, name } => {
            (role.clone(), name.clone(), BTreeMap::new())
        }
        ComputerSemanticTarget::Css { selector } => (
            "css-selector".into(),
            selector.clone(),
            BTreeMap::from([("selector".into(), RawValue::new(selector))]),
        ),
        ComputerSemanticTarget::Text { text } => ("text".into(), text.clone(), BTreeMap::new()),
    };
    RawSemanticTarget {
        role: role.clone(),
        accessible_name: RawValue::new(&name),
        stable_attributes,
        bounds: Rectangle {
            x: cursor.x,
            y: cursor.y,
            width: 1,
            height: 1,
        },
        field: FieldMetadata {
            name: None,
            role: Some(role),
            autocomplete: None,
            user_marked_sensitive: false,
        },
    }
}

fn focused_node(observation: &ComputerObservation) -> Option<&AccessibilityNode> {
    observation.accessibility.iter().find(|node| node.focused)
}

fn focused_target(observation: &ComputerObservation) -> Option<RawSemanticTarget> {
    focused_node(observation).map(|node| {
        raw_cua_semantic_target(
            &ComputerSemanticTarget::Accessibility {
                role: node.role.clone(),
                name: node.name.clone(),
            },
            observation.cursor,
        )
    })
}

fn focused_field(observation: &ComputerObservation) -> FieldMetadata {
    focused_node(observation).map_or_else(FieldMetadata::default, |node| FieldMetadata {
        name: Some(node.name.clone()),
        role: Some(node.role.clone()),
        autocomplete: None,
        user_marked_sensitive: node.role.eq_ignore_ascii_case("password"),
    })
}

fn compile_event(
    event: &DemonstrationEvent,
) -> Result<Option<(String, RecipeAction)>, TaskRecipeError> {
    match &event.kind {
        DemonstrationEventKind::Navigate { url } => Ok(Some((
            "Open the demonstrated page".into(),
            RecipeAction::Navigate {
                url: template_value(url)?,
            },
        ))),
        DemonstrationEventKind::Wait { duration_ms } => Ok(Some((
            "Wait for the interface to settle".into(),
            RecipeAction::Wait {
                duration_ms: *duration_ms,
            },
        ))),
        DemonstrationEventKind::Pointer(input)
            if input.action == PointerAction::ButtonUp
                && input.button == PointerButton::Primary =>
        {
            Ok(Some((
                target_title("Activate", &event.context),
                RecipeAction::Activate {
                    target: recipe_target(event)?,
                },
            )))
        }
        DemonstrationEventKind::Keyboard(input)
            if matches!(input.phase, KeyPhase::Down | KeyPhase::Repeat)
                && input.code == "Shortcut" =>
        {
            let keys = input.modifiers.clone();
            if keys.is_empty() {
                return Err(TaskRecipeError::InvalidRecipe(
                    "captured shortcut has no keys".into(),
                ));
            }
            Ok(Some((
                format!("Press {}", keys.join(" + ")),
                RecipeAction::Shortcut { keys },
            )))
        }
        DemonstrationEventKind::Keyboard(input)
            if matches!(input.phase, KeyPhase::Down | KeyPhase::Repeat) =>
        {
            Ok(Some((
                target_title("Enter text in", &event.context),
                RecipeAction::EnterText {
                    target: recipe_target(event)?,
                    value: template_value(&input.key)?,
                },
            )))
        }
        DemonstrationEventKind::Clipboard {
            operation: ClipboardOperation::Write,
            value: Some(value),
            ..
        } if event.context.semantic_target.is_some() => Ok(Some((
            target_title("Paste into", &event.context),
            RecipeAction::EnterText {
                target: recipe_target(event)?,
                value: template_value(value)?,
            },
        ))),
        DemonstrationEventKind::File {
            operation: FileOperation::Upload,
            path,
            ..
        } => Ok(Some((
            target_title("Upload a file using", &event.context),
            RecipeAction::Upload {
                target: recipe_target(event)?,
                file: template_value(path)?,
            },
        ))),
        DemonstrationEventKind::File {
            operation: FileOperation::Download,
            ..
        } => Ok(Some((
            target_title("Download from", &event.context),
            RecipeAction::Download {
                target: recipe_target(event)?,
            },
        ))),
        DemonstrationEventKind::FrameCaptured(_)
        | DemonstrationEventKind::Pointer(_)
        | DemonstrationEventKind::Keyboard(_)
        | DemonstrationEventKind::Clipboard { .. }
        | DemonstrationEventKind::File { .. }
        | DemonstrationEventKind::Pause { .. }
        | DemonstrationEventKind::Resume
        | DemonstrationEventKind::Narration(_)
        | DemonstrationEventKind::ControlChanged(_) => Ok(None),
    }
}

fn template_value(value: &CapturedValue) -> Result<TemplateValue, TaskRecipeError> {
    match value {
        CapturedValue::Literal(value) => Ok(TemplateValue::Literal(value.clone())),
        CapturedValue::Parameter(parameter) => Ok(TemplateValue::Parameter(parameter.clone())),
        CapturedValue::Redacted(_) => Err(TaskRecipeError::InvalidRecipe(
            "a redacted value must be labeled as a runtime or credential parameter before replay"
                .into(),
        )),
    }
}

fn recipe_target(event: &DemonstrationEvent) -> Result<RecipeTarget, TaskRecipeError> {
    let semantic = event.context.semantic_target.as_ref().ok_or_else(|| {
        TaskRecipeError::InvalidRecipe(
            "coordinate-only input must be corrected to a semantic target before replay".into(),
        )
    })?;
    let frame = event.context.frame.as_ref().ok_or_else(|| {
        TaskRecipeError::InvalidRecipe("captured action has no source frame".into())
    })?;
    let stable_attributes = semantic
        .stable_attributes
        .iter()
        .filter_map(|(name, value)| {
            value
                .as_literal()
                .map(|value| (name.clone(), value.to_owned()))
        })
        .collect();
    Ok(RecipeTarget {
        semantic: SemanticTargetSelector {
            role: semantic.role.clone(),
            accessible_name: Some(template_value(&semantic.accessible_name)?),
            stable_attributes,
        },
        visual_fallback: Some(VisualFallback {
            source_frame_digest: frame.media.digest.clone(),
            normalized_bounds: semantic.bounds,
            match_threshold_percent: 85,
        }),
    })
}

fn target_title(prefix: &str, context: &crate::CaptureContext) -> String {
    let name = context
        .semantic_target
        .as_ref()
        .and_then(|target| target.accessible_name.as_literal())
        .unwrap_or("the selected control");
    format!("{prefix} {name}")
}

fn next_context_matcher(
    events: &[DemonstrationEvent],
    index: usize,
    title: &str,
    timeout_ms: u64,
) -> Result<ObservationMatcher, TaskRecipeError> {
    let context = events
        .get(index.saturating_add(1))
        .map_or(&events[index].context, |event| &event.context);
    context_matcher(
        context,
        &format!("The expected state appears after: {title}"),
        timeout_ms,
    )
}

fn context_matcher(
    context: &crate::CaptureContext,
    description: &str,
    timeout_ms: u64,
) -> Result<ObservationMatcher, TaskRecipeError> {
    let (kind, expected) = if let Some(url) = &context.url {
        (ObservationKind::Url, template_value(url)?)
    } else if let Some(window) = &context.window {
        (ObservationKind::Window, template_value(window)?)
    } else if let Some(application) = &context.application {
        (ObservationKind::Application, template_value(application)?)
    } else if let Some(target) = &context.semantic_target {
        (
            ObservationKind::Accessibility,
            template_value(&target.accessible_name)?,
        )
    } else {
        return Err(TaskRecipeError::InvalidRecipe(
            "captured state has no semantic completion evidence".into(),
        ));
    };
    Ok(ObservationMatcher {
        kind,
        description: description.into(),
        expected,
        timeout_ms,
    })
}

fn collect_inputs(demonstration: &Demonstration) -> Vec<RecipeInput> {
    let mut parameters = BTreeMap::<String, ParameterSource>::new();
    for event in demonstration.events() {
        collect_context_parameters(&event.context, &mut parameters);
        match &event.kind {
            DemonstrationEventKind::Keyboard(input) => {
                collect_value_parameter(&input.key, &mut parameters);
            }
            DemonstrationEventKind::Clipboard { value, .. } => {
                if let Some(value) = value {
                    collect_value_parameter(value, &mut parameters);
                }
            }
            DemonstrationEventKind::File { path, .. } => {
                collect_value_parameter(path, &mut parameters);
            }
            DemonstrationEventKind::Pause { reason }
            | DemonstrationEventKind::Narration(reason) => {
                collect_value_parameter(reason, &mut parameters);
            }
            DemonstrationEventKind::Navigate { url } => {
                collect_value_parameter(url, &mut parameters);
            }
            DemonstrationEventKind::FrameCaptured(_)
            | DemonstrationEventKind::Pointer(_)
            | DemonstrationEventKind::Resume
            | DemonstrationEventKind::ControlChanged(_)
            | DemonstrationEventKind::Wait { .. } => {}
        }
    }
    parameters
        .into_iter()
        .map(|(name, source)| RecipeInput {
            label: humanize(&name),
            name,
            kind: if source == ParameterSource::NamedCredential {
                RecipeInputKind::Credential
            } else {
                RecipeInputKind::Text
            },
            required: true,
        })
        .collect()
}

fn collect_context_parameters(
    context: &crate::CaptureContext,
    parameters: &mut BTreeMap<String, ParameterSource>,
) {
    for value in [
        context.url.as_ref(),
        context.window.as_ref(),
        context.application.as_ref(),
    ]
    .into_iter()
    .flatten()
    {
        collect_value_parameter(value, parameters);
    }
    if let Some(target) = &context.semantic_target {
        collect_value_parameter(&target.accessible_name, parameters);
        for value in target.stable_attributes.values() {
            collect_value_parameter(value, parameters);
        }
    }
}

fn collect_value_parameter(
    value: &CapturedValue,
    parameters: &mut BTreeMap<String, ParameterSource>,
) {
    if let CapturedValue::Parameter(parameter) = value {
        parameters
            .entry(parameter.name.clone())
            .and_modify(|source| {
                if parameter.source == ParameterSource::NamedCredential {
                    *source = ParameterSource::NamedCredential;
                }
            })
            .or_insert(parameter.source);
    }
}

fn consequential_approval(action: &RecipeAction) -> Option<ApprovalRequirement> {
    if matches!(action, RecipeAction::Wait { .. }) {
        return None;
    }
    Some(ApprovalRequirement {
        capability: Capability::ComputerControl,
        risk: ActionRisk::IrreversibleComputerInput,
        reason: "Confirm the current target before replaying this computer input".into(),
        invalidate_on_target_change: true,
    })
}

fn validate_replay_inputs(
    recipe: &TaskRecipe,
    inputs: &BTreeMap<String, ReplayInputValue>,
) -> Result<(), TaskRecipeError> {
    if inputs
        .keys()
        .any(|name| !recipe.inputs.iter().any(|input| &input.name == name))
    {
        return Err(TaskRecipeError::InvalidRecipe(
            "replay contains an undeclared input".into(),
        ));
    }
    for input in &recipe.inputs {
        let value = inputs.get(&input.name);
        if input.required && value.is_none() {
            return Err(TaskRecipeError::InvalidRecipe(format!(
                "required replay input {} is missing",
                input.name
            )));
        }
        if let Some(value) = value {
            let compatible = matches!(
                (&input.kind, value),
                (RecipeInputKind::Text, ReplayInputValue::Text(_))
                    | (RecipeInputKind::Url, ReplayInputValue::Url(_))
                    | (RecipeInputKind::File, ReplayInputValue::File(_))
                    | (
                        RecipeInputKind::Credential,
                        ReplayInputValue::CredentialName(_)
                    )
                    | (RecipeInputKind::Choice(_), ReplayInputValue::Choice(_))
            );
            if !compatible || replay_input_text(value).trim().is_empty() {
                return Err(TaskRecipeError::InvalidRecipe(format!(
                    "replay input {} has the wrong type or is empty",
                    input.name
                )));
            }
            if let (RecipeInputKind::Choice(options), ReplayInputValue::Choice(selected)) =
                (&input.kind, value)
                && !options.contains(selected)
            {
                return Err(TaskRecipeError::InvalidRecipe(format!(
                    "replay choice {} is not declared",
                    input.name
                )));
            }
        }
    }
    Ok(())
}

fn replay_commands(
    action: &RecipeAction,
    observation: &ComputerObservation,
    inputs: &BTreeMap<String, ReplayInputValue>,
) -> Result<Vec<ReplayComputerCommand>, Vec<SemanticTargetSelector>> {
    let resolve = |target: &RecipeTarget| resolve_target(target, observation, inputs);
    match action {
        RecipeAction::Navigate { url } => Ok(vec![ReplayComputerCommand::Computer(
            ComputerAction::Navigate {
                url: resolve_template(url, inputs).map_err(|_| Vec::new())?,
            },
        )]),
        RecipeAction::Activate { target } => {
            let target = resolve(target)?;
            Ok(vec![ReplayComputerCommand::Computer(
                ComputerAction::Click {
                    target,
                    button: MouseButton::Left,
                },
            )])
        }
        RecipeAction::EnterText { target, value } => {
            let target = resolve(target)?;
            if let TemplateValue::Parameter(parameter) = value
                && parameter.source == ParameterSource::NamedCredential
            {
                let ComputerActionTarget::Semantic { target } = target else {
                    return Err(Vec::new());
                };
                return Ok(vec![ReplayComputerCommand::NamedCredentialFill {
                    parameter_name: replay_credential(parameter, inputs).map_err(|_| Vec::new())?,
                    target,
                }]);
            }
            Ok(vec![
                ReplayComputerCommand::Computer(ComputerAction::Click {
                    target,
                    button: MouseButton::Left,
                }),
                ReplayComputerCommand::Computer(ComputerAction::Text {
                    text: resolve_template(value, inputs).map_err(|_| Vec::new())?,
                }),
            ])
        }
        RecipeAction::Select { target, value } => {
            let target = resolve(target)?;
            Ok(vec![
                ReplayComputerCommand::Computer(ComputerAction::Click {
                    target,
                    button: MouseButton::Left,
                }),
                ReplayComputerCommand::Computer(ComputerAction::Text {
                    text: resolve_template(value, inputs).map_err(|_| Vec::new())?,
                }),
            ])
        }
        RecipeAction::Shortcut { keys } => Ok(vec![ReplayComputerCommand::Computer(
            ComputerAction::Shortcut { keys: keys.clone() },
        )]),
        RecipeAction::Upload { target, file } => {
            let ComputerActionTarget::Semantic { target } = resolve(target)? else {
                return Err(Vec::new());
            };
            Ok(vec![ReplayComputerCommand::Computer(
                ComputerAction::FileUpload {
                    target,
                    relative_path: resolve_template(file, inputs).map_err(|_| Vec::new())?,
                },
            )])
        }
        RecipeAction::Download { target } => {
            let ComputerActionTarget::Semantic { target } = resolve(target)? else {
                return Err(Vec::new());
            };
            Ok(vec![ReplayComputerCommand::Computer(
                ComputerAction::Download {
                    target,
                    expected_file_name: None,
                },
            )])
        }
        RecipeAction::Wait { duration_ms } => Ok(vec![ReplayComputerCommand::Computer(
            ComputerAction::Wait {
                duration_ms: *duration_ms,
            },
        )]),
    }
}

fn resolve_target(
    target: &RecipeTarget,
    observation: &ComputerObservation,
    inputs: &BTreeMap<String, ReplayInputValue>,
) -> Result<ComputerActionTarget, Vec<SemanticTargetSelector>> {
    let expected_name = target
        .semantic
        .accessible_name
        .as_ref()
        .map(|value| resolve_template(value, inputs))
        .transpose()
        .map_err(|_| Vec::new())?;
    if let Some(node) = observation.accessibility.iter().find(|node| {
        node.role.eq_ignore_ascii_case(&target.semantic.role)
            && expected_name
                .as_ref()
                .is_none_or(|name| node.name.eq_ignore_ascii_case(name))
            && !node.disabled
    }) {
        return Ok(ComputerActionTarget::Semantic {
            target: ComputerSemanticTarget::Accessibility {
                role: node.role.clone(),
                name: node.name.clone(),
            },
        });
    }
    if let Some(fallback) = &target.visual_fallback
        && fallback.source_frame_digest == observation.screenshot.content_digest
    {
        let x = fallback.normalized_bounds.x.saturating_add(
            i32::try_from(fallback.normalized_bounds.width / 2).unwrap_or(i32::MAX),
        );
        let y = fallback.normalized_bounds.y.saturating_add(
            i32::try_from(fallback.normalized_bounds.height / 2).unwrap_or(i32::MAX),
        );
        let point = Point { x, y };
        if observation.viewport.contains(point) {
            return Ok(ComputerActionTarget::Coordinate {
                point,
                source_frame: observation.screenshot.frame_id.clone(),
            });
        }
    }
    let suggestions = observation
        .accessibility
        .iter()
        .filter(|node| node.role.eq_ignore_ascii_case(&target.semantic.role) && !node.disabled)
        .take(8)
        .map(|node| SemanticTargetSelector {
            role: node.role.clone(),
            accessible_name: Some(TemplateValue::Literal(node.name.clone())),
            stable_attributes: BTreeMap::new(),
        })
        .collect();
    Err(suggestions)
}

fn compare_matchers(
    recipe_revision: u64,
    step_id: Option<String>,
    matchers: &[ObservationMatcher],
    observation: &ComputerObservation,
    inputs: &BTreeMap<String, ReplayInputValue>,
    compared_at: UtcTimestamp,
) -> Result<RecipeReplayComparison, TaskRecipeError> {
    let checks = matchers
        .iter()
        .map(|matcher| {
            Ok(ReplayCheck {
                description: matcher.description.clone(),
                passed: matcher_matches(matcher, observation, inputs)?,
            })
        })
        .collect::<Result<Vec<_>, TaskRecipeError>>()?;
    let passed = !checks.is_empty() && checks.iter().all(|check| check.passed);
    Ok(RecipeReplayComparison {
        recipe_revision,
        step_id,
        checks,
        suggested_targets: Vec::new(),
        passed,
        compared_at,
    })
}

fn matcher_matches(
    matcher: &ObservationMatcher,
    observation: &ComputerObservation,
    inputs: &BTreeMap<String, ReplayInputValue>,
) -> Result<bool, TaskRecipeError> {
    let expected = resolve_template(&matcher.expected, inputs)?;
    Ok(match matcher.kind {
        ObservationKind::Url => observation.url.as_deref() == Some(expected.as_str()),
        ObservationKind::Window => observation
            .focused_window
            .as_ref()
            .is_some_and(|window| window.title == expected),
        ObservationKind::Application => observation
            .focused_window
            .as_ref()
            .is_some_and(|window| window.application == expected),
        ObservationKind::Dom => observation
            .dom
            .as_ref()
            .is_some_and(|dom| dom.html.contains(&expected)),
        ObservationKind::Accessibility => observation
            .accessibility
            .iter()
            .any(|node| node.name == expected || node.value.as_deref() == Some(expected.as_str())),
        ObservationKind::VisibleText => {
            observation
                .dom
                .as_ref()
                .is_some_and(|dom| dom.html.contains(&expected))
                || observation.accessibility.iter().any(|node| {
                    node.name.contains(&expected)
                        || node
                            .value
                            .as_ref()
                            .is_some_and(|value| value.contains(&expected))
                })
        }
        ObservationKind::FileExists => observation
            .downloads
            .iter()
            .any(|download| download.file_name == expected),
        ObservationKind::DownloadCompleted => observation.downloads.iter().any(|download| {
            download.file_name == expected && download.state == DownloadState::Completed
        }),
    })
}

fn resolve_template(
    value: &TemplateValue,
    inputs: &BTreeMap<String, ReplayInputValue>,
) -> Result<String, TaskRecipeError> {
    match value {
        TemplateValue::Literal(value) => Ok(value.clone()),
        TemplateValue::Parameter(parameter) => {
            let value = inputs.get(&parameter.name).ok_or_else(|| {
                TaskRecipeError::InvalidRecipe(format!(
                    "replay input {} is missing",
                    parameter.name
                ))
            })?;
            if parameter.source == ParameterSource::NamedCredential {
                return Err(TaskRecipeError::InvalidRecipe(
                    "named credentials may only be resolved by the CUA credential broker".into(),
                ));
            }
            Ok(replay_input_text(value).to_owned())
        }
    }
}

fn replay_credential(
    parameter: &ParameterReference,
    inputs: &BTreeMap<String, ReplayInputValue>,
) -> Result<String, TaskRecipeError> {
    match inputs.get(&parameter.name) {
        Some(ReplayInputValue::CredentialName(name)) => Ok(name.clone()),
        _ => Err(TaskRecipeError::InvalidRecipe(format!(
            "named credential {} is missing",
            parameter.name
        ))),
    }
}

fn replay_input_text(value: &ReplayInputValue) -> &str {
    match value {
        ReplayInputValue::Text(value)
        | ReplayInputValue::Url(value)
        | ReplayInputValue::File(value)
        | ReplayInputValue::CredentialName(value)
        | ReplayInputValue::Choice(value) => value,
    }
}

fn humanize(name: &str) -> String {
    let mut words = name
        .split(['-', '_', '.'])
        .filter(|word| !word.is_empty())
        .collect::<Vec<_>>();
    if words.is_empty() {
        return "Input".into();
    }
    let first = words.remove(0);
    let first = first
        .chars()
        .enumerate()
        .map(|(index, character)| {
            if index == 0 {
                character.to_ascii_uppercase()
            } else {
                character
            }
        })
        .collect::<String>();
    std::iter::once(first)
        .chain(words.into_iter().map(str::to_owned))
        .collect::<Vec<_>>()
        .join(" ")
}

fn hex_digest(bytes: &[u8]) -> String {
    Sha256::digest(bytes)
        .iter()
        .fold(String::with_capacity(64), |mut output, byte| {
            use std::fmt::Write as _;
            write!(output, "{byte:02x}").expect("writing to a String cannot fail");
            output
        })
}
