#![forbid(unsafe_code)]

mod connection;
mod render;

pub use connection::*;
pub use render::{render, settled_transcript_lines};

use std::collections::VecDeque;
use std::fmt::Write as _;
use std::time::{Duration, Instant};

use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
use keith_agent_types::{
    ChildId, ClientId, CommandId, EntityId, GoalId, JobId, SessionId, UtcTimestamp,
};
use keith_platform_contracts::{
    ActionRisk, ApprovalEnvelope, ApprovalState, AuditCorrelationId, Capability, ExternalAction,
    ExternalEffect, ExternalPrincipalId, RedactedText,
};
use keith_protocol::{
    AttachSession, BackgroundControl, BackgroundMode, CancelTarget, ChildMessageRequest,
    ChildWorkspaceMode, ClientCommand, CommandEnvelope, CommandResult, CreateChild, CreateGoal,
    CreateSchedule, CreateSession, DaemonEvent, DeliveryPolicy, EventAcknowledgement,
    EvolutionCommand, EvolutionProjection, ExportFormat, ExportRequest, GoalLimits,
    IntegrationCommand, IntegrationMutation, IntegrationOperation, IntegrationService, MemoryQuery,
    PresenceState, ProfileIntegrationsProjection, ProfileSummary, ResponsePayload,
    ScheduleExpression, SessionFilter, SessionSummary, SteerAction, SubmitPrompt, UpdateSchedule,
    WireMessage,
};
use keith_ui_model::{
    ClientParity, OperatorCommand, OperatorSurface, ProjectionReducer, ReductionOutcome,
    VirtualizationConfig, project_evolution, project_personal_intelligence,
};
use unicode_width::UnicodeWidthStr;

pub use keith_ui_model::OperatorSurface as Surface;

pub const MAX_COMPOSER_BYTES: usize = 64 * 1_024;
pub const MAX_LOG_LINES: usize = 512;
pub const MAX_PENDING_COMMANDS: usize = 128;
pub const MAX_INPUT_HISTORY: usize = 200;

const OVERLAY_COMMANDS: [(&str, &str); 30] = [
    ("Start a new conversation", "/new"),
    ("Continue this conversation", "/resume"),
    ("Choose a conversation", "/sessions"),
    ("Choose a model", "/models"),
    ("Review decisions", "/approvals"),
    ("See current work", "/work"),
    ("Search saved context", "/memory "),
    ("Create a goal", "/goal "),
    ("Delegate work", "/child "),
    ("Create a schedule", "/schedule "),
    ("Export this conversation", "/export markdown"),
    ("Stop the current turn", "/stop"),
    ("Open diagnostics", "/diagnostics"),
    ("Review Keith's evolution", "/evolution"),
    ("Request self-evolution enablement", "/evolution-enable"),
    ("Disable self-evolution", "/evolution-disable"),
    ("Restore the human-approved baseline", "/evolution-restore"),
    ("Review all external services", "/services"),
    ("Review channel accounts", "/channels"),
    ("Review connected apps", "/apps"),
    ("Review plugins", "/plugins"),
    ("Review ACP connections", "/acp"),
    ("Review computers", "/computers"),
    ("Review recordings", "/recordings"),
    ("Review task recipes", "/recipes"),
    ("Review harness repairs", "/harness"),
    ("Toggle tool details", "/details"),
    ("Show keyboard shortcuts", "/help"),
    ("Exit Keith", "/exit"),
    ("Exit Keith", "/quit"),
];

pub fn client_parity() -> ClientParity {
    ClientParity {
        surfaces: OperatorSurface::ALL.into_iter().collect(),
        commands: OperatorCommand::ALL.into_iter().collect(),
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ColorMode {
    TrueColor,
    Ansi256,
    NoColor,
    HighContrast,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Accessibility {
    pub color_mode: ColorMode,
    pub reduced_motion: bool,
}

impl Default for Accessibility {
    fn default() -> Self {
        Self {
            color_mode: ColorMode::TrueColor,
            reduced_motion: false,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum AppAction {
    None,
    Redraw,
    Quit,
    OpenExternalEditor,
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum ToolDetailMode {
    #[default]
    Compact,
    Expanded,
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub enum SessionTransition {
    #[default]
    Stable,
    Creating {
        previous_session: Option<SessionId>,
        command_id: Option<CommandId>,
    },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum PendingPromptState {
    Sending,
    Submitted,
    Failed,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct PendingPrompt {
    pub(crate) session_id: SessionId,
    pub(crate) text: String,
    pub(crate) state: PendingPromptState,
    command_id: Option<CommandId>,
    authoritative_occurrence: usize,
    message_anchor: usize,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TuiOverlay {
    Sessions,
    Commands,
    Models,
    Approvals,
    Work,
    Memory,
    Diagnostics,
    Evolution,
    Services,
    Help,
}

impl TuiOverlay {
    pub const ALL: [Self; 10] = [
        Self::Sessions,
        Self::Commands,
        Self::Models,
        Self::Approvals,
        Self::Work,
        Self::Memory,
        Self::Diagnostics,
        Self::Evolution,
        Self::Services,
        Self::Help,
    ];

    pub const fn label(self) -> &'static str {
        match self {
            Self::Sessions => "Conversations",
            Self::Commands => "Commands",
            Self::Models => "Models",
            Self::Approvals => "Needs your decision",
            Self::Work => "Work",
            Self::Memory => "Saved context",
            Self::Diagnostics => "Diagnostics",
            Self::Evolution => "Keith evolution",
            Self::Services => "Keith everywhere",
            Self::Help => "Keyboard shortcuts",
        }
    }
}

pub struct TuiApp {
    pub client_id: ClientId,
    pub overlay: Option<TuiOverlay>,
    pub overlay_query: String,
    pub overlay_selection: usize,
    pub accessibility: Accessibility,
    pub composer: String,
    pub cursor_byte: usize,
    pub profiles: Vec<ProfileSummary>,
    pub sessions: Vec<SessionSummary>,
    pub attached_session: Option<SessionId>,
    pub reducer: Option<ProjectionReducer>,
    pub evolution: Option<EvolutionProjection>,
    pub integrations: Option<ProfileIntegrationsProjection>,
    pub evolution_notice: Option<String>,
    pub connected: bool,
    pub reconnecting: bool,
    pub quit: bool,
    pub scroll_from_end: usize,
    pub last_prompt: Option<String>,
    pub tool_detail_mode: ToolDetailMode,
    pub completion_selection: usize,
    pub session_transition: SessionTransition,
    input_history: VecDeque<String>,
    history_index: Option<usize>,
    history_draft: String,
    in_flight_commands: usize,
    pending_commands: VecDeque<ClientCommand>,
    pending_prompts: VecDeque<PendingPrompt>,
    turn_session: Option<SessionId>,
    turn_started_at: Option<Instant>,
    logs: VecDeque<String>,
}

impl TuiApp {
    pub fn new(accessibility: Accessibility) -> Self {
        Self {
            client_id: ClientId::new(),
            overlay: None,
            overlay_query: String::new(),
            overlay_selection: 0,
            accessibility,
            composer: String::new(),
            cursor_byte: 0,
            profiles: Vec::new(),
            sessions: Vec::new(),
            attached_session: None,
            reducer: None,
            evolution: None,
            integrations: None,
            evolution_notice: None,
            connected: false,
            reconnecting: false,
            quit: false,
            scroll_from_end: 0,
            last_prompt: None,
            tool_detail_mode: ToolDetailMode::Compact,
            completion_selection: 0,
            session_transition: SessionTransition::Stable,
            input_history: VecDeque::new(),
            history_index: None,
            history_draft: String::new(),
            in_flight_commands: 0,
            pending_commands: VecDeque::new(),
            pending_prompts: VecDeque::new(),
            turn_session: None,
            turn_started_at: None,
            logs: VecDeque::new(),
        }
    }

    pub fn logs(&self) -> &VecDeque<String> {
        &self.logs
    }

    pub fn open_overlay(&mut self, overlay: TuiOverlay) {
        self.overlay = Some(overlay);
        self.overlay_query.clear();
        self.overlay_selection = 0;
        if overlay == TuiOverlay::Sessions {
            self.list_sessions();
        } else if overlay == TuiOverlay::Evolution {
            self.enqueue(ClientCommand::Evolution(EvolutionCommand::Status));
            self.enqueue(ClientCommand::Evolution(EvolutionCommand::BrowseLedger {
                before_sequence: None,
                limit: 50,
            }));
        } else if overlay == TuiOverlay::Services {
            self.list_integrations();
        }
    }

    pub fn list_profiles(&mut self) {
        self.enqueue(ClientCommand::ListProfiles);
    }

    pub fn start_new_session(&mut self) {
        let profile = self
            .reducer
            .as_ref()
            .and_then(|reducer| {
                let profile_id = &reducer.snapshot().session.profile_id;
                self.profiles
                    .iter()
                    .find(|profile| &profile.id == profile_id)
            })
            .or_else(|| self.profiles.iter().find(|profile| profile.enabled))
            .cloned();
        let Some(profile) = profile else {
            self.log("No enabled profile is available. Refreshing profiles…");
            self.list_profiles();
            return;
        };
        let command = ClientCommand::CreateSession(CreateSession {
            profile_id: profile.id,
            workspace_id: profile.workspace_id,
            title: None,
        });
        if self.enqueue(command) {
            self.session_transition = SessionTransition::Creating {
                previous_session: self.attached_session.take(),
                command_id: None,
            };
            self.reducer = None;
            self.integrations = None;
            self.scroll_from_end = 0;
            self.turn_session = None;
            self.turn_started_at = None;
        }
    }

    pub fn current_session_label(&self) -> String {
        if self.is_starting_new_session() {
            return "Starting new conversation…".into();
        }
        let Some(session_id) = &self.attached_session else {
            return "No conversation".into();
        };
        if let Some(title) = self
            .sessions
            .iter()
            .find(|session| &session.session_id == session_id)
            .and_then(|session| session.title.as_deref())
            .or_else(|| {
                self.reducer
                    .as_ref()
                    .and_then(|reducer| reducer.snapshot().session.title.as_deref())
            })
            .filter(|title| {
                !title.trim().is_empty() && !title.eq_ignore_ascii_case("new conversation")
            })
        {
            return title.into();
        }
        self.reducer
            .as_ref()
            .and_then(|reducer| {
                reducer
                    .snapshot()
                    .messages
                    .iter()
                    .find(|message| message.role == keith_protocol::MessageRole::User)
            })
            .map_or_else(
                || "New conversation".into(),
                |message| compact_conversation_title(&message.text),
            )
    }

    pub fn is_starting_new_session(&self) -> bool {
        matches!(self.session_transition, SessionTransition::Creating { .. })
    }

    pub fn empty_conversation_message(&self) -> &'static str {
        if self.is_starting_new_session() {
            "  Starting a new conversation…"
        } else if self.attached_session.is_some() {
            "  Loading conversation…"
        } else {
            "  Choose a conversation to begin."
        }
    }

    pub fn turn_is_active(&self) -> bool {
        let local_turn_is_active = self.turn_started_at.is_some()
            && self
                .attached_session
                .as_ref()
                .zip(self.turn_session.as_ref())
                .is_some_and(|(attached, active)| attached == active);
        local_turn_is_active
            || self.reducer.as_ref().is_some_and(|reducer| {
                matches!(
                    reducer.snapshot().presence.state,
                    PresenceState::Thinking
                        | PresenceState::UsingTools
                        | PresenceState::WaitingChild
                        | PresenceState::WaitingExternal
                )
            })
    }

    pub fn turn_elapsed(&self) -> Option<Duration> {
        self.turn_is_active().then(|| {
            self.turn_started_at
                .map_or(Duration::ZERO, |started| started.elapsed())
        })
    }

    pub const fn tool_details_expanded(&self) -> bool {
        matches!(self.tool_detail_mode, ToolDetailMode::Expanded)
    }

    fn toggle_tool_details(&mut self) {
        self.tool_detail_mode = match self.tool_detail_mode {
            ToolDetailMode::Compact => ToolDetailMode::Expanded,
            ToolDetailMode::Expanded => ToolDetailMode::Compact,
        };
    }

    pub fn slash_suggestions(&self) -> Vec<(&'static str, &'static str)> {
        let Some(query) = self.composer.strip_prefix('/') else {
            return Vec::new();
        };
        if query.contains(char::is_whitespace) {
            return Vec::new();
        }
        let query = query.to_lowercase();
        OVERLAY_COMMANDS
            .iter()
            .filter(|(_, command)| command[1..].starts_with(&query))
            .map(|(label, command)| (*command, *label))
            .collect()
    }

    pub fn latest_log(&self) -> Option<&str> {
        self.logs.back().map(String::as_str)
    }

    pub(crate) fn pending_prompts(&self) -> impl DoubleEndedIterator<Item = &PendingPrompt> {
        self.pending_prompts.iter().filter(|prompt| {
            self.attached_session
                .as_ref()
                .is_some_and(|session_id| session_id == &prompt.session_id)
        })
    }

    pub(crate) fn pending_prompt_anchor(prompt: &PendingPrompt) -> usize {
        prompt.message_anchor
    }

    pub fn pending_len(&self) -> usize {
        self.pending_commands.len()
    }

    pub const fn in_flight_len(&self) -> usize {
        self.in_flight_commands
    }

    pub fn command_dispatched(&mut self, command_id: &CommandId) {
        self.in_flight_commands = self.in_flight_commands.saturating_add(1);
        self.mark_pending_prompt(Some(command_id), PendingPromptState::Submitted);
    }

    pub fn command_finished(&mut self) {
        self.in_flight_commands = self.in_flight_commands.saturating_sub(1);
    }

    pub fn report_command_failure(
        &mut self,
        command_id: Option<&CommandId>,
        error: impl Into<String>,
    ) {
        self.mark_pending_prompt(command_id, PendingPromptState::Failed);
        self.recover_new_session(command_id);
        self.log(format!("Command transport failed: {}", error.into()));
        self.sync_turn_activity();
    }

    pub fn report_reconnecting(&mut self) {
        self.connected = false;
        self.reconnecting = true;
    }

    pub fn report_reconnected(&mut self) {
        self.connected = true;
        self.reconnecting = false;
        self.log("Reconnected");
    }

    pub fn report_reconnect_failure(&mut self, error: impl Into<String>) {
        self.connected = false;
        self.reconnecting = false;
        self.log(format!("Reconnect failed: {}", error.into()));
    }

    pub fn next_command(&mut self) -> Option<ClientCommand> {
        self.pending_commands.pop_front()
    }

    pub fn list_sessions(&mut self) {
        self.enqueue(ClientCommand::ListSessions(SessionFilter::default()));
    }

    pub fn list_integrations(&mut self) {
        let Some(profile_id) = self
            .reducer
            .as_ref()
            .map(|reducer| reducer.snapshot().session.profile_id.clone())
        else {
            self.log("Attach a conversation before reviewing external services");
            return;
        };
        self.enqueue(ClientCommand::Integration(IntegrationCommand::List {
            profile_id,
            service: None,
        }));
    }

    fn open_integrations(&mut self, filter: &str) {
        self.open_overlay(TuiOverlay::Services);
        self.overlay_query = filter.into();
    }

    pub fn select_model(&mut self, provider: String, model: String) {
        let Some(session_id) = self.attached_session.clone() else {
            self.log("Select a session before changing models");
            return;
        };
        self.enqueue(ClientCommand::SelectModel(keith_protocol::ModelSelection {
            session_id,
            provider,
            model,
        }));
    }

    pub fn resolve_confirmation(
        &mut self,
        confirmation_id: EntityId,
        decision: keith_protocol::ConfirmationDecision,
    ) {
        self.enqueue(ClientCommand::ResolveConfirmation(
            keith_protocol::ConfirmationResolution {
                confirmation_id,
                decision,
            },
        ));
    }

    pub fn attach(&mut self, session_id: SessionId) {
        if self.attached_session.as_ref() != Some(&session_id) {
            self.turn_session = None;
            self.turn_started_at = None;
        }
        let resume = self.resume_cursor();
        self.attached_session = Some(session_id.clone());
        self.enqueue(ClientCommand::AttachSession(AttachSession {
            session_id,
            resume,
        }));
    }

    pub fn resume_attached_session(&mut self) {
        if let Some(session_id) = self.attached_session.clone() {
            self.attach(session_id);
        }
    }

    pub fn resume_cursor(&self) -> Option<keith_protocol::ResumeCursor> {
        self.reducer.as_ref().map(|reducer| {
            let snapshot = reducer.snapshot();
            keith_protocol::ResumeCursor {
                root_tree_id: snapshot.session.root_tree_id.clone(),
                generation: snapshot.generation,
                last_sequence: snapshot.through_sequence,
            }
        })
    }

    pub fn handle_key(&mut self, key: KeyEvent) -> AppAction {
        if self.overlay.is_some() {
            return self.handle_overlay_key(key);
        }
        if let Some(action) = self.handle_completion_key(key.code) {
            return action;
        }
        if key.modifiers.contains(KeyModifiers::CONTROL) {
            return self.handle_control_key(key.code);
        }
        match key.code {
            KeyCode::Tab => {
                self.open_overlay(TuiOverlay::Commands);
                AppAction::Redraw
            }
            KeyCode::BackTab => {
                self.open_overlay(TuiOverlay::Sessions);
                AppAction::Redraw
            }
            KeyCode::Esc => self.handle_escape(),
            KeyCode::Enter
                if key
                    .modifiers
                    .intersects(KeyModifiers::ALT | KeyModifiers::SHIFT) =>
            {
                self.insert_text("\n");
                AppAction::Redraw
            }
            KeyCode::Enter => {
                self.submit_prompt(self.prompt_delivery());
                AppAction::Redraw
            }
            KeyCode::Backspace => {
                self.backspace();
                AppAction::Redraw
            }
            KeyCode::Delete => {
                self.delete_forward();
                AppAction::Redraw
            }
            KeyCode::Left => {
                if key.modifiers.contains(KeyModifiers::ALT) {
                    self.move_word_left();
                } else {
                    self.move_left();
                }
                AppAction::Redraw
            }
            KeyCode::Right => {
                if key.modifiers.contains(KeyModifiers::ALT) {
                    self.move_word_right();
                } else {
                    self.move_right();
                }
                AppAction::Redraw
            }
            KeyCode::Home => {
                self.cursor_byte = 0;
                AppAction::Redraw
            }
            KeyCode::End => {
                self.cursor_byte = self.composer.len();
                AppAction::Redraw
            }
            KeyCode::PageUp => {
                self.scroll_from_end = self.scroll_from_end.saturating_add(10);
                AppAction::Redraw
            }
            KeyCode::PageDown => {
                self.scroll_from_end = self.scroll_from_end.saturating_sub(10);
                AppAction::Redraw
            }
            KeyCode::Up => {
                self.history_previous();
                AppAction::Redraw
            }
            KeyCode::Down => {
                self.history_next();
                AppAction::Redraw
            }
            KeyCode::Char('?') if self.composer.is_empty() => {
                self.open_overlay(TuiOverlay::Help);
                AppAction::Redraw
            }
            KeyCode::Char(character)
                if !key
                    .modifiers
                    .intersects(KeyModifiers::ALT | KeyModifiers::SUPER) =>
            {
                let mut encoded = [0_u8; 4];
                self.insert_text(character.encode_utf8(&mut encoded));
                AppAction::Redraw
            }
            _ => AppAction::None,
        }
    }

    fn handle_completion_key(&mut self, code: KeyCode) -> Option<AppAction> {
        let suggestions = self.slash_suggestions();
        if suggestions.is_empty() {
            return None;
        }
        match code {
            KeyCode::Tab => {
                let selected = self.completion_selection.min(suggestions.len() - 1);
                self.replace_composer(suggestions[selected].0.to_owned());
                Some(AppAction::Redraw)
            }
            KeyCode::Up => {
                self.completion_selection = self.completion_selection.saturating_sub(1);
                Some(AppAction::Redraw)
            }
            KeyCode::Down => {
                self.completion_selection = self
                    .completion_selection
                    .saturating_add(1)
                    .min(suggestions.len() - 1);
                Some(AppAction::Redraw)
            }
            _ => None,
        }
    }

    fn prompt_delivery(&self) -> DeliveryPolicy {
        if self.turn_is_active() {
            DeliveryPolicy::NextTurnBoundary
        } else {
            DeliveryPolicy::Immediate
        }
    }

    fn clear_draft_or_scroll(&mut self) {
        if self.composer.is_empty() {
            self.scroll_from_end = 0;
        } else {
            self.clear_composer();
        }
    }

    fn handle_escape(&mut self) -> AppAction {
        if self.composer.is_empty() && self.turn_is_active() {
            if let Some(session_id) = self.attached_session.clone() {
                self.enqueue(ClientCommand::Cancel(CancelTarget::Session(session_id)));
                self.log("Cancellation requested for the active turn");
            }
        } else {
            self.clear_draft_or_scroll();
        }
        AppAction::Redraw
    }

    pub fn handle_paste(&mut self, pasted: &str) -> AppAction {
        let normalized = pasted.replace("\r\n", "\n").replace('\r', "\n");
        self.insert_text(&normalized);
        AppAction::Redraw
    }

    pub fn overlay_rows(&self) -> Vec<String> {
        let Some(overlay) = self.overlay else {
            return Vec::new();
        };
        let query = self.overlay_query.to_lowercase();
        let matches = |value: &str| query.is_empty() || value.to_lowercase().contains(&query);
        match overlay {
            TuiOverlay::Sessions => self
                .sessions
                .iter()
                .filter_map(|session| {
                    let title = session.title.as_deref().unwrap_or("New conversation");
                    matches(title).then(|| title.to_owned())
                })
                .collect(),
            TuiOverlay::Commands => OVERLAY_COMMANDS
                .iter()
                .filter(|(label, command)| matches(label) || matches(command))
                .map(|(label, command)| format!("{label}  {command}"))
                .collect(),
            TuiOverlay::Models => keith_provider_catalog::BUILTIN_PROVIDERS
                .iter()
                .filter(|provider| {
                    matches(provider.display_name)
                        || matches(provider.id)
                        || matches(provider.default_model)
                })
                .map(|provider| format!("{}  {}", provider.display_name, provider.default_model))
                .collect(),
            TuiOverlay::Approvals => self.reducer.as_ref().map_or_else(Vec::new, |reducer| {
                reducer
                    .snapshot()
                    .confirmations
                    .iter()
                    .filter(|confirmation| matches(&confirmation.summary))
                    .map(|confirmation| confirmation.summary.clone())
                    .collect()
            }),
            TuiOverlay::Work => self.reducer.as_ref().map_or_else(Vec::new, |reducer| {
                let personal = project_personal_intelligence(reducer.snapshot());
                personal
                    .needs_you
                    .iter()
                    .chain(&personal.work)
                    .chain(&personal.upcoming)
                    .chain(&personal.completed)
                    .filter(|item| matches(&item.title) || matches(&item.state_label))
                    .map(|item| format!("{}  {}", item.state_label, item.title))
                    .collect()
            }),
            TuiOverlay::Memory => self.reducer.as_ref().map_or_else(Vec::new, |reducer| {
                project_personal_intelligence(reducer.snapshot())
                    .saved_context
                    .into_iter()
                    .filter(|item| matches(&item.title) || matches(&item.state_label))
                    .map(|item| format!("{}  {}", item.state_label, item.title))
                    .collect()
            }),
            TuiOverlay::Diagnostics => self
                .diagnostic_rows()
                .into_iter()
                .filter(|row| matches(row))
                .collect(),
            TuiOverlay::Evolution => self
                .evolution_rows()
                .into_iter()
                .filter(|row| matches(row))
                .collect(),
            TuiOverlay::Services => self
                .integration_rows()
                .into_iter()
                .filter(|row| matches(row))
                .collect(),
            TuiOverlay::Help => [
                "Enter  Send now, or queue while Keith is working",
                "Alt-Enter  Insert a newline",
                "Up / Down  Browse prompt history",
                "Tab  Complete a slash command",
                "Ctrl-P  Open the command palette",
                "Ctrl-S  Switch conversations",
                "Ctrl-L  Start a new conversation",
                "Ctrl-E  Edit the draft in $VISUAL or $EDITOR",
                "Ctrl-K  Steer the active turn with this draft",
                "Ctrl-X  Stop the active turn",
                "Ctrl-T  Toggle tool details",
                "PageUp / PageDown  Scroll the transcript",
                "Ctrl-C  Clear draft, stop active turn, then exit",
                "Ctrl-D  Exit",
            ]
            .into_iter()
            .filter(|row| matches(row))
            .map(str::to_owned)
            .collect(),
        }
    }

    fn integration_rows(&self) -> Vec<String> {
        let Some(projection) = &self.integrations else {
            return vec!["Loading profile-scoped service state…".into()];
        };
        let mut rows = projection
            .services
            .iter()
            .map(|service| match &service.availability {
                keith_protocol::IntegrationAvailabilityProjection::Available => {
                    format!("{} — enabled", integration_service_label(service.service))
                }
                keith_protocol::IntegrationAvailabilityProjection::Disabled => {
                    format!(
                        "{} — disabled by installation policy",
                        integration_service_label(service.service)
                    )
                }
                keith_protocol::IntegrationAvailabilityProjection::Unavailable { safe_reason } => {
                    format!(
                        "{} — unavailable: {safe_reason}",
                        integration_service_label(service.service)
                    )
                }
            })
            .collect::<Vec<_>>();
        rows.extend(projection.resources.iter().map(|resource| {
            let controls = resource
                .controls
                .iter()
                .map(|control| format!("{control:?}").to_lowercase())
                .collect::<Vec<_>>()
                .join(", ");
            let mut row = format!(
                "{} · {} · {} · revision {}",
                integration_service_label(resource.service),
                resource.display_label,
                format!("{:?}", resource.lifecycle).to_lowercase(),
                resource.revision.get(),
            );
            if !controls.is_empty() {
                let _ = write!(row, " · controls: {controls}");
            }
            if let Some(error) = &resource.safe_error {
                let _ = write!(row, " · safe error: {error}");
            }
            row
        }));
        rows
    }

    fn diagnostic_rows(&self) -> Vec<String> {
        let Some(reducer) = &self.reducer else {
            return vec!["No conversation is attached".into()];
        };
        let snapshot = reducer.snapshot();
        vec![
            format!("Generation {}", snapshot.generation.get()),
            format!("Sequence {}", snapshot.through_sequence.get()),
            format!("Projection revision {}", snapshot.revision.get()),
            format!("Stream {:?}", reducer.stream_state()),
            format!("Composer width {}", self.composer_display_width()),
            format!("Queued commands {}", self.pending_len()),
            format!("In-flight commands {}", self.in_flight_len()),
        ]
    }

    fn evolution_rows(&self) -> Vec<String> {
        let Some(evolution) = &self.evolution else {
            let mut rows = vec!["Loading self-evolution status…".into()];
            rows.extend(
                self.evolution_notice
                    .as_ref()
                    .map(|notice| format!("Notice: {notice}")),
            );
            return rows;
        };
        let view = project_evolution(evolution);
        let mut rows = vec![view.status, view.availability];
        rows.extend(
            self.evolution_notice
                .as_ref()
                .map(|notice| format!("Notice: {notice}")),
        );
        rows.extend(
            view.guidance
                .map(|guidance| format!("Guidance: {guidance}")),
        );
        rows.extend(
            view.disclosure
                .into_iter()
                .map(|(label, detail)| format!("{label}: {detail}")),
        );
        if evolution.enabled {
            rows.push("Press Enter to disable self-evolution".into());
        } else {
            rows.push("Press Enter to request installation-owner enablement".into());
        }
        rows.push("Press Enter to restore the human-approved baseline".into());
        if let Some(title) = view.active_title {
            rows.push(format!(
                "Active: {title} — {}",
                view.active_state.unwrap_or_else(|| "In progress".into())
            ));
            rows.extend(
                view.evidence
                    .into_iter()
                    .map(|item| format!("Evidence: {item}")),
            );
            rows.extend(view.readable_diff.map(|item| format!("Change: {item}")));
            rows.extend(
                view.measured_result
                    .map(|item| format!("Measured result: {item}")),
            );
            if view.approval_hypothesis_id.is_some() {
                rows.push("Press Enter to approve this change".into());
            }
        }
        rows.extend(view.ledger.into_iter().map(|item| {
            let mut row = format!("{} — {}", item.state, item.title);
            if !item.evidence.is_empty() {
                let _ = write!(row, "\nEvidence: {}", item.evidence.join("; "));
            }
            if let Some(diff) = item.readable_diff {
                let _ = write!(row, "\nChange: {diff}");
            }
            if let Some(result) = item.measured_result {
                let _ = write!(row, "\nMeasured result: {result}");
            }
            if item.reversal_promotion_id.is_some() {
                row.push_str("\nPress Enter to revert");
            }
            row
        }));
        if view.has_more_ledger {
            rows.push("More history is available".into());
        }
        rows
    }

    fn handle_overlay_key(&mut self, key: KeyEvent) -> AppAction {
        if key.modifiers.contains(KeyModifiers::CONTROL) {
            if key.code == KeyCode::Char('c') {
                self.overlay = None;
                self.overlay_query.clear();
                return AppAction::Redraw;
            }
            if key.code == KeyCode::Char('d') {
                self.quit = true;
                return AppAction::Quit;
            }
        }
        if key.modifiers.contains(KeyModifiers::ALT)
            && matches!(key.code, KeyCode::Char('a' | 'd'))
            && self.overlay == Some(TuiOverlay::Approvals)
        {
            let allow = key.code == KeyCode::Char('a');
            self.resolve_selected_confirmation(allow);
            self.overlay = None;
            return AppAction::Redraw;
        }
        match key.code {
            KeyCode::Esc => {
                self.overlay = None;
                self.overlay_query.clear();
            }
            KeyCode::Tab | KeyCode::BackTab => {
                let current = self.overlay.unwrap_or(TuiOverlay::Commands);
                let index = TuiOverlay::ALL
                    .iter()
                    .position(|candidate| *candidate == current)
                    .unwrap_or(0);
                let step = if key.code == KeyCode::Tab {
                    1
                } else {
                    TuiOverlay::ALL.len() - 1
                };
                self.open_overlay(TuiOverlay::ALL[(index + step) % TuiOverlay::ALL.len()]);
            }
            KeyCode::Up => self.overlay_selection = self.overlay_selection.saturating_sub(1),
            KeyCode::Down => {
                self.overlay_selection = self
                    .overlay_selection
                    .saturating_add(1)
                    .min(self.overlay_rows().len().saturating_sub(1));
            }
            KeyCode::Backspace => {
                self.overlay_query.pop();
                self.overlay_selection = 0;
            }
            KeyCode::Enter => self.activate_overlay_selection(),
            KeyCode::Char(character)
                if !key
                    .modifiers
                    .intersects(KeyModifiers::ALT | KeyModifiers::SUPER) =>
            {
                self.overlay_query.push(character);
                self.overlay_selection = 0;
            }
            _ => return AppAction::None,
        }
        AppAction::Redraw
    }

    fn activate_overlay_selection(&mut self) {
        match self.overlay {
            Some(TuiOverlay::Sessions) => {
                let query = self.overlay_query.to_lowercase();
                let selected = self
                    .sessions
                    .iter()
                    .filter(|session| {
                        query.is_empty()
                            || session
                                .title
                                .as_deref()
                                .unwrap_or("New conversation")
                                .to_lowercase()
                                .contains(&query)
                    })
                    .nth(self.overlay_selection)
                    .map(|session| session.session_id.clone());
                if let Some(session_id) = selected {
                    self.attach(session_id);
                    self.overlay = None;
                }
            }
            Some(TuiOverlay::Commands) => {
                let query = self.overlay_query.to_lowercase();
                let selected = OVERLAY_COMMANDS
                    .iter()
                    .filter(|(label, command)| {
                        query.is_empty()
                            || label.to_lowercase().contains(&query)
                            || command.contains(&query)
                    })
                    .nth(self.overlay_selection)
                    .map(|(_, command)| *command);
                if let Some(command) = selected {
                    self.apply_palette_command(command);
                }
            }
            Some(TuiOverlay::Models) => {
                let query = self.overlay_query.to_lowercase();
                let selected = keith_provider_catalog::BUILTIN_PROVIDERS
                    .iter()
                    .filter(|provider| {
                        query.is_empty()
                            || provider.display_name.to_lowercase().contains(&query)
                            || provider.id.contains(&query)
                            || provider.default_model.contains(&query)
                    })
                    .nth(self.overlay_selection)
                    .map(|provider| (provider.id.to_owned(), provider.default_model.to_owned()));
                if let Some((provider, model)) = selected {
                    self.select_model(provider, model);
                    self.overlay = None;
                }
            }
            Some(
                TuiOverlay::Approvals
                | TuiOverlay::Work
                | TuiOverlay::Memory
                | TuiOverlay::Diagnostics
                | TuiOverlay::Services
                | TuiOverlay::Help,
            )
            | None => {}
            Some(TuiOverlay::Evolution) => self.activate_evolution_selection(),
        }
    }

    fn activate_evolution_selection(&mut self) {
        let Some(evolution) = &self.evolution else {
            return;
        };
        let view = project_evolution(evolution);
        let selected = self.overlay_rows().get(self.overlay_selection).cloned();
        match selected.as_deref() {
            Some("Press Enter to request installation-owner enablement") => {
                self.request_evolution_enable();
                return;
            }
            Some("Press Enter to disable self-evolution") => {
                self.request_evolution_disable();
                return;
            }
            Some("Press Enter to restore the human-approved baseline") => {
                self.request_evolution_restore();
                return;
            }
            _ => {}
        }
        if selected.as_deref() == Some("Press Enter to approve this change") {
            if let Some(hypothesis_id) = view.approval_hypothesis_id {
                self.enqueue(ClientCommand::Evolution(EvolutionCommand::Approve {
                    hypothesis_id,
                }));
            }
            return;
        }
        let Some(selected) = selected else { return };
        let promotion_id = view
            .ledger
            .into_iter()
            .find(|item| selected.contains(&item.title) && item.reversal_promotion_id.is_some())
            .and_then(|item| item.reversal_promotion_id);
        if let Some(promotion_id) = promotion_id {
            self.enqueue(ClientCommand::Evolution(EvolutionCommand::Revert {
                promotion_id,
                reason: "Requested from the terminal evolution history".into(),
            }));
        }
    }

    fn apply_palette_command(&mut self, command: &str) {
        match command {
            "/new" => {
                self.start_new_session();
                self.overlay = None;
            }
            "/sessions" => self.open_overlay(TuiOverlay::Sessions),
            "/models" => self.open_overlay(TuiOverlay::Models),
            "/approvals" => self.open_overlay(TuiOverlay::Approvals),
            "/work" => self.open_overlay(TuiOverlay::Work),
            "/diagnostics" => self.open_overlay(TuiOverlay::Diagnostics),
            "/evolution" => self.open_overlay(TuiOverlay::Evolution),
            "/services" => self.open_integrations(""),
            "/channels" => self.open_integrations("Channel account"),
            "/apps" => self.open_integrations("Connected app"),
            "/plugins" => self.open_integrations("Plugin"),
            "/acp" => self.open_integrations("ACP connection"),
            "/computers" => self.open_integrations("Computer"),
            "/recordings" => self.open_integrations("Recording"),
            "/recipes" => self.open_integrations("Recipe"),
            "/harness" => self.open_integrations("Harness repair"),
            "/evolution-enable" => self.request_evolution_enable(),
            "/evolution-disable" => self.request_evolution_disable(),
            "/evolution-restore" => self.request_evolution_restore(),
            "/details" => {
                self.toggle_tool_details();
                self.overlay = None;
            }
            "/help" => self.open_overlay(TuiOverlay::Help),
            "/exit" | "/quit" => {
                self.quit = true;
                self.overlay = None;
            }
            "/stop" => {
                if let Some(session_id) = self.attached_session.clone() {
                    self.enqueue(ClientCommand::Cancel(CancelTarget::Session(session_id)));
                }
                self.overlay = None;
            }
            "/resume" => {
                if let Some(session_id) = self.attached_session.clone() {
                    self.enqueue(ClientCommand::ResumeSession { session_id });
                }
                self.overlay = None;
            }
            command => {
                self.replace_composer(command.to_owned());
                self.overlay = None;
            }
        }
    }

    fn resolve_selected_confirmation(&mut self, allow: bool) {
        let query = self.overlay_query.to_lowercase();
        let confirmation = self.reducer.as_ref().and_then(|reducer| {
            reducer
                .snapshot()
                .confirmations
                .iter()
                .filter(|confirmation| {
                    query.is_empty() || confirmation.summary.to_lowercase().contains(&query)
                })
                .nth(self.overlay_selection)
                .map(|confirmation| confirmation.confirmation_id.clone())
        });
        if let Some(confirmation_id) = confirmation {
            self.resolve_confirmation(
                confirmation_id,
                if allow {
                    keith_protocol::ConfirmationDecision::AllowOnce
                } else {
                    keith_protocol::ConfirmationDecision::Deny
                },
            );
        }
    }

    pub fn replace_composer(&mut self, content: String) {
        self.composer = bounded_text(content, MAX_COMPOSER_BYTES);
        self.cursor_byte = self.composer.len();
        self.history_index = None;
        self.history_draft.clear();
        self.completion_selection = 0;
    }

    pub fn apply_wire_message(&mut self, message: WireMessage) {
        match message {
            WireMessage::ServerHello(_) => {
                self.connected = true;
                self.reconnecting = false;
                self.log("Connected");
            }
            WireMessage::CommandResult(result) => {
                let command_id = result.command_id.clone();
                let rejected = matches!(&result.result, CommandResult::Rejected(_));
                self.mark_pending_prompt(
                    Some(&command_id),
                    if rejected {
                        PendingPromptState::Failed
                    } else {
                        PendingPromptState::Submitted
                    },
                );
                if rejected {
                    self.recover_new_session(Some(&command_id));
                }
                match result.result {
                    CommandResult::Data(payload) => match *payload {
                        ResponsePayload::Profiles(profiles) => self.profiles = profiles,
                        ResponsePayload::Sessions(sessions) => {
                            let first = sessions.first().map(|session| session.session_id.clone());
                            self.sessions = sessions;
                            if !self.is_starting_new_session()
                                && self.attached_session.is_none()
                                && let Some(session_id) = first
                            {
                                self.attach(session_id);
                            }
                        }
                        ResponsePayload::Snapshot(snapshot) => {
                            self.apply_command_snapshot(*snapshot, &command_id);
                        }
                        ResponsePayload::Evolution(evolution) => self.evolution = Some(*evolution),
                        ResponsePayload::ProfileIntegrations(integrations) => {
                            self.integrations = Some(*integrations);
                        }
                        ResponsePayload::IntegrationResource(_)
                        | ResponsePayload::IntegrationDeletion(_) => self.list_integrations(),
                        other => self.log(format!("Received {} projection", payload_label(&other))),
                    },
                    CommandResult::Accepted { .. } => self.log("Command accepted"),
                    CommandResult::Rejected(error) => {
                        if self.overlay == Some(TuiOverlay::Evolution) {
                            self.evolution_notice = Some(error.error.message.clone());
                        }
                        self.log(format!("Command rejected: {}", error.error.message));
                    }
                }
                self.sync_turn_activity();
            }
            WireMessage::Event(envelope) => {
                if let DaemonEvent::EvolutionChanged(evolution) = &envelope.event {
                    self.evolution = Some((**evolution).clone());
                }
                let acknowledgement = EventAcknowledgement {
                    root_tree_id: envelope.root_tree_id.clone(),
                    generation: envelope.generation,
                    through_sequence: envelope.sequence,
                };
                if let Some(reducer) = &mut self.reducer {
                    match reducer.apply_event(&envelope) {
                        Ok(
                            ReductionOutcome::Applied | ReductionOutcome::AppliedCoalesced { .. },
                        ) => {
                            self.enqueue(ClientCommand::AcknowledgeEvents(acknowledgement));
                        }
                        Ok(ReductionOutcome::Gap) => {
                            self.reconnecting = true;
                            if let Some(session_id) = self.attached_session.clone() {
                                self.attach(session_id);
                            }
                        }
                        Ok(
                            ReductionOutcome::SnapshotReplaced
                            | ReductionOutcome::Duplicate
                            | ReductionOutcome::StaleGeneration,
                        ) => {}
                        Err(error) => self.log(format!("Projection error: {error}")),
                    }
                } else if let DaemonEvent::Snapshot(snapshot) = envelope.event {
                    self.apply_snapshot(*snapshot);
                }
                self.reconcile_pending_prompts();
                self.sync_turn_activity();
            }
            message @ (WireMessage::Snapshot(_) | WireMessage::Terminal(_)) => {
                if let Some(envelope) = message.into_event() {
                    self.apply_wire_message(WireMessage::Event(envelope));
                }
            }
            WireMessage::ClientHello(_) | WireMessage::Command(_) => {}
        }
    }

    pub fn command_envelope(&mut self, command: ClientCommand) -> CommandEnvelope {
        let installation_scoped = matches!(&command, ClientCommand::Evolution(_));
        let command_id = CommandId::new();
        match &command {
            ClientCommand::CreateSession(_) => {
                if let SessionTransition::Creating {
                    command_id: transition_command,
                    ..
                } = &mut self.session_transition
                {
                    *transition_command = Some(command_id.clone());
                }
            }
            ClientCommand::SubmitPrompt(prompt) => self.assign_pending_prompt_command(
                &prompt.session_id,
                &prompt.text,
                command_id.clone(),
            ),
            ClientCommand::Steer(action) => self.assign_pending_prompt_command(
                &action.session_id,
                &action.text,
                command_id.clone(),
            ),
            _ => {}
        }
        CommandEnvelope {
            protocol: keith_agent_types::CURRENT_PROTOCOL_VERSION,
            command_id,
            client_id: self.client_id.clone(),
            sent_at: UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH),
            session_id: if installation_scoped {
                None
            } else {
                self.attached_session.clone()
            },
            command,
        }
    }

    pub fn composer_display_width(&self) -> usize {
        UnicodeWidthStr::width(self.composer.as_str())
    }

    fn handle_control_key(&mut self, code: KeyCode) -> AppAction {
        match code {
            KeyCode::Char('d') => {
                self.quit = true;
                AppAction::Quit
            }
            KeyCode::Char('c') => {
                if !self.composer.is_empty() {
                    self.clear_composer();
                    return AppAction::Redraw;
                }
                if self.turn_is_active() {
                    if let Some(session_id) = self.attached_session.clone() {
                        self.enqueue(ClientCommand::Cancel(CancelTarget::Session(session_id)));
                        self.log("Cancellation requested for the active turn");
                    }
                    return AppAction::Redraw;
                }
                self.quit = true;
                AppAction::Quit
            }
            KeyCode::Char('a') => {
                self.cursor_byte = 0;
                AppAction::Redraw
            }
            KeyCode::Char('e' | 'g') => AppAction::OpenExternalEditor,
            KeyCode::Char('l') => {
                self.start_new_session();
                AppAction::Redraw
            }
            KeyCode::Char('t') => {
                self.toggle_tool_details();
                AppAction::Redraw
            }
            KeyCode::Char('w') => {
                self.delete_word_left();
                AppAction::Redraw
            }
            KeyCode::Char('u') => {
                if let Some(session_id) = self.attached_session.clone() {
                    self.enqueue(ClientCommand::ResumeSession { session_id });
                }
                AppAction::Redraw
            }
            KeyCode::Char('k') => {
                self.steer();
                AppAction::Redraw
            }
            KeyCode::Char('x') => {
                if let Some(session_id) = self.attached_session.clone() {
                    self.enqueue(ClientCommand::Cancel(CancelTarget::Session(session_id)));
                    self.log("Cancellation requested for the active turn");
                }
                AppAction::Redraw
            }
            KeyCode::Char('r') => {
                if let Some(prompt) = self.last_prompt.clone() {
                    self.composer = prompt;
                    self.cursor_byte = self.composer.len();
                    self.submit_prompt(DeliveryPolicy::Immediate);
                }
                AppAction::Redraw
            }
            KeyCode::Char('b') => {
                self.branch_from_latest();
                AppAction::Redraw
            }
            KeyCode::Char('s') => {
                self.open_overlay(TuiOverlay::Sessions);
                AppAction::Redraw
            }
            KeyCode::Char('p') => {
                self.open_overlay(TuiOverlay::Commands);
                AppAction::Redraw
            }
            KeyCode::Char('m') => {
                self.open_overlay(TuiOverlay::Models);
                AppAction::Redraw
            }
            KeyCode::Char('y') => {
                self.open_overlay(TuiOverlay::Memory);
                AppAction::Redraw
            }
            _ => AppAction::None,
        }
    }

    fn submit_prompt(&mut self, delivery: DeliveryPolicy) {
        let text = self.composer.trim().to_owned();
        if text.is_empty() {
            return;
        }
        if matches!(text.as_str(), "/exit" | "/quit") {
            self.quit = true;
            self.record_history(text);
            self.clear_composer();
            return;
        }
        if text == "/new" {
            self.start_new_session();
            self.record_history(text);
            self.clear_composer();
            return;
        }
        let Some(session_id) = self.attached_session.clone() else {
            self.log("Select a session before sending a prompt");
            return;
        };
        if let Some(selection) = text.strip_prefix("/model ") {
            let mut parts = selection.split_whitespace();
            let Some(provider) = parts.next() else {
                self.log("Usage: /model <provider> [model]");
                return;
            };
            let Some(provider_spec) = keith_provider_catalog::provider(provider) else {
                self.log("Unknown provider. Open the Models view for supported provider IDs");
                return;
            };
            let model = parts.next().unwrap_or(provider_spec.default_model);
            if parts.next().is_some() {
                self.log("Usage: /model <provider> [model]");
                return;
            }
            self.select_model(provider.to_owned(), model.to_owned());
            self.record_history(text);
            self.clear_composer();
            return;
        }
        if text.starts_with('/') && self.handle_slash_command(&session_id, &text) {
            self.record_history(text);
            self.clear_composer();
            return;
        }
        let command = ClientCommand::SubmitPrompt(SubmitPrompt {
            session_id: session_id.clone(),
            text: text.clone(),
            artifacts: Vec::new(),
            delivery,
            reply_route: None,
        });
        if !self.enqueue(command) {
            return;
        }
        self.track_pending_prompt(session_id, text.clone());
        if delivery == DeliveryPolicy::NextTurnBoundary {
            self.log("Prompt queued for the next turn boundary");
        }
        self.last_prompt = Some(text.clone());
        self.record_history(text);
        self.clear_composer();
    }

    #[allow(clippy::too_many_lines)]
    fn handle_slash_command(&mut self, session_id: &SessionId, input: &str) -> bool {
        let (command, argument) = input
            .split_once(' ')
            .map_or((input, ""), |(command, argument)| {
                (command, argument.trim())
            });
        let limits = || GoalLimits {
            max_turns: Some(100),
            max_tokens: Some(1_000_000),
            deadline: None,
        };
        match command {
            "/new" => self.start_new_session(),
            "/sessions" => self.open_overlay(TuiOverlay::Sessions),
            "/commands" => self.open_overlay(TuiOverlay::Commands),
            "/help" => self.open_overlay(TuiOverlay::Help),
            "/exit" | "/quit" => self.quit = true,
            "/models" => self.open_overlay(TuiOverlay::Models),
            "/approvals" => self.open_overlay(TuiOverlay::Approvals),
            "/work" => self.open_overlay(TuiOverlay::Work),
            "/memory" if argument.is_empty() => self.open_overlay(TuiOverlay::Memory),
            "/diagnostics" => self.open_overlay(TuiOverlay::Diagnostics),
            "/evolution" => self.open_overlay(TuiOverlay::Evolution),
            "/services" => self.open_integrations(""),
            "/channels" => self.open_integrations("Channel account"),
            "/apps" => self.open_integrations("Connected app"),
            "/plugins" => self.open_integrations("Plugin"),
            "/acp" => self.open_integrations("ACP connection"),
            "/computers" => self.open_integrations("Computer"),
            "/recordings" => self.open_integrations("Recording"),
            "/recipes" => self.open_integrations("Recipe"),
            "/harness" => self.open_integrations("Harness repair"),
            "/evolution-enable" => self.request_evolution_enable(),
            "/evolution-disable" => self.request_evolution_disable(),
            "/evolution-restore" => self.request_evolution_restore(),
            "/details" => self.toggle_tool_details(),
            "/service-cancel" => {
                self.enqueue_integration_operation(argument, IntegrationOperation::Cancel);
            }
            "/service-export" => {
                self.enqueue_integration_operation(argument, IntegrationOperation::Export);
            }
            "/service-restart" => {
                self.enqueue_integration_operation(argument, IntegrationOperation::Resume);
            }
            "/service-test" => {
                self.enqueue_integration_operation(argument, IntegrationOperation::Test);
            }
            "/computer-release" => {
                self.enqueue_integration_operation(
                    &format!("control_lease {argument}"),
                    IntegrationOperation::ReleaseControl,
                );
            }
            "/recording-stop" => {
                self.enqueue_integration_operation(
                    &format!("recording {argument}"),
                    IntegrationOperation::StopRecording,
                );
            }
            "/harness-reverse" => {
                self.enqueue_integration_operation(
                    &format!("harness_repair {argument}"),
                    IntegrationOperation::Reverse,
                );
            }
            "/stop" => {
                self.enqueue(ClientCommand::Cancel(CancelTarget::Session(
                    session_id.clone(),
                )));
            }
            "/resume" => {
                self.enqueue(ClientCommand::ResumeSession {
                    session_id: session_id.clone(),
                });
            }
            "/goal" if !argument.is_empty() => {
                self.enqueue(ClientCommand::CreateGoal(CreateGoal {
                    session_id: session_id.clone(),
                    objective: argument.into(),
                    limits: limits(),
                }));
            }
            "/goals" => {
                self.enqueue(ClientCommand::ListGoals {
                    session_id: session_id.clone(),
                });
            }
            "/child" if !argument.is_empty() => {
                self.enqueue(ClientCommand::CreateChild(CreateChild {
                    parent_session_id: session_id.clone(),
                    objective: argument.into(),
                    workspace_mode: ChildWorkspaceMode::SharedWorkspace,
                    limits: limits(),
                }));
            }
            "/children" => {
                self.enqueue(ClientCommand::ListChildren {
                    session_id: session_id.clone(),
                });
            }
            "/child-message" => {
                let Some((id, text)) = argument.split_once(' ') else {
                    self.log("Usage: /child-message <child-id> <text>");
                    return true;
                };
                let Ok(child_id) = id.parse::<ChildId>() else {
                    self.log("Child identifier is invalid");
                    return true;
                };
                self.enqueue(ClientCommand::SendChildMessage(ChildMessageRequest {
                    child_id,
                    text: text.trim().into(),
                    artifact_ids: Vec::new(),
                }));
            }
            "/archive-child" => {
                let Ok(child_id) = argument.parse::<ChildId>() else {
                    self.log("Usage: /archive-child <child-id>");
                    return true;
                };
                self.enqueue(ClientCommand::ArchiveChild { child_id });
            }
            "/memory" if !argument.is_empty() => {
                let Some(profile_id) = self
                    .reducer
                    .as_ref()
                    .map(|reducer| reducer.snapshot().session.profile_id.clone())
                else {
                    self.log("Attach a session before querying memory");
                    return true;
                };
                self.enqueue(ClientCommand::QueryMemory(MemoryQuery {
                    profile_id,
                    query: argument.into(),
                    limit: 20,
                }));
            }
            "/schedule" => {
                let Some((seconds, prompt)) = argument.split_once(' ') else {
                    self.log("Usage: /schedule <interval-seconds> <prompt>");
                    return true;
                };
                let Ok(seconds) = seconds.parse::<u64>() else {
                    self.log("Schedule interval must be an integer number of seconds");
                    return true;
                };
                let Some(profile_id) = self
                    .reducer
                    .as_ref()
                    .map(|reducer| reducer.snapshot().session.profile_id.clone())
                else {
                    self.log("Attach a session before creating a schedule");
                    return true;
                };
                self.enqueue(ClientCommand::CreateSchedule(CreateSchedule {
                    profile_id,
                    session_id: Some(session_id.clone()),
                    expression: ScheduleExpression::IntervalSeconds(seconds),
                    time_zone: "UTC".into(),
                    prompt: prompt.trim().into(),
                    reply_route: None,
                }));
            }
            "/pause-schedule" | "/resume-schedule" => {
                let Ok(job_id) = argument.parse::<JobId>() else {
                    self.log("Usage: /pause-schedule <job-id> or /resume-schedule <job-id>");
                    return true;
                };
                self.enqueue(ClientCommand::UpdateSchedule(UpdateSchedule {
                    job_id,
                    expression: None,
                    prompt: None,
                    paused: Some(command == "/pause-schedule"),
                }));
            }
            "/delete-schedule" => {
                let Ok(job_id) = argument.parse::<JobId>() else {
                    self.log("Usage: /delete-schedule <job-id>");
                    return true;
                };
                self.enqueue(ClientCommand::DeleteSchedule { job_id });
            }
            "/export" => {
                self.enqueue(ClientCommand::Export(ExportRequest {
                    session_id: session_id.clone(),
                    format: match argument {
                        "jsonl" => ExportFormat::JsonLines,
                        "markdown" | "md" => ExportFormat::Markdown,
                        "" | "bundle" => ExportFormat::PortableBundle,
                        _ => {
                            self.log("Usage: /export [jsonl|markdown|bundle]");
                            return true;
                        }
                    },
                    include_artifacts: true,
                }));
            }
            "/select-branch" => {
                let Ok(leaf_entry_id) = argument.parse::<EntityId>() else {
                    self.log("Usage: /select-branch <entry-id>");
                    return true;
                };
                self.enqueue(ClientCommand::SelectBranch(keith_protocol::SelectBranch {
                    session_id: session_id.clone(),
                    leaf_entry_id,
                }));
            }
            "/cancel-goal" => {
                let Ok(goal_id) = argument.parse::<GoalId>() else {
                    self.log("Usage: /cancel-goal <goal-id>");
                    return true;
                };
                self.enqueue(ClientCommand::Cancel(CancelTarget::Goal(goal_id)));
            }
            "/cancel-child" => {
                let Ok(child_id) = argument.parse::<ChildId>() else {
                    self.log("Usage: /cancel-child <child-id>");
                    return true;
                };
                self.enqueue(ClientCommand::Cancel(CancelTarget::Child(child_id)));
            }
            "/background" => {
                let mode = match argument {
                    "disabled" | "off" => BackgroundMode::Disabled,
                    "suggest" => BackgroundMode::Suggest,
                    "confirm" => BackgroundMode::ConfirmSelected,
                    "bounded" => BackgroundMode::Bounded,
                    _ => {
                        self.log("Usage: /background <disabled|suggest|confirm|bounded>");
                        return true;
                    }
                };
                let Some(profile_id) = self
                    .reducer
                    .as_ref()
                    .map(|reducer| reducer.snapshot().session.profile_id.clone())
                else {
                    self.log("Attach a session before changing background control");
                    return true;
                };
                self.enqueue(ClientCommand::SetBackgroundControl(BackgroundControl {
                    profile_id,
                    mode,
                    pause_until: None,
                }));
            }
            _ if input.starts_with('/') => {
                self.log("Unknown command. Use /help for available commands");
            }
            _ => return false,
        }
        true
    }

    fn enqueue_integration_operation(&mut self, argument: &str, operation: IntegrationOperation) {
        let mut parts = argument.split_whitespace();
        let Some(service) = parts.next().and_then(parse_integration_service) else {
            self.log("Usage: /service-<action> <service> <resource-id>");
            return;
        };
        let Some(resource_id) = parts
            .next()
            .and_then(|value| value.parse::<EntityId>().ok())
        else {
            self.log("Integration resource identifier is invalid");
            return;
        };
        if parts.next().is_some() {
            self.log("Usage: /service-<action> <service> <resource-id>");
            return;
        }
        let Some(resource) = self.integrations.as_ref().and_then(|projection| {
            projection
                .resources
                .iter()
                .find(|resource| resource.service == service && resource.id == resource_id)
                .cloned()
        }) else {
            self.log("Refresh /services before acting on that exact resource");
            return;
        };
        let Some(session_id) = self.attached_session.clone() else {
            self.log("Attach a conversation before changing an external service");
            return;
        };
        let idempotency_key = EntityId::new().to_string();
        let Ok(target) = RedactedText::parse(resource.native_resource_key.clone()) else {
            self.log("Integration resource identity is unsafe");
            return;
        };
        let Ok(target_digest) = RedactedText::parse(format!(
            "integration:{service:?}:{}:{}",
            resource.id,
            resource.revision.get()
        )) else {
            self.log("Integration target digest is unsafe");
            return;
        };
        let (requested_capability, risk) = integration_operation_authority(operation);
        let external_effect = if matches!(
            operation,
            IntegrationOperation::Test | IntegrationOperation::Export
        ) {
            ExternalEffect::Repeatable
        } else {
            let Ok(delivery_key) = RedactedText::parse(idempotency_key.clone()) else {
                self.log("Integration idempotency identity is unsafe");
                return;
            };
            ExternalEffect::Idempotent { delivery_key }
        };
        let authority = ExternalAction {
            profile_id: resource.profile_id.clone(),
            session_id,
            acting_principal: ExternalPrincipalId::new(),
            requested_capability,
            risk,
            approval: ApprovalEnvelope {
                risk,
                state: ApprovalState::NotRequired,
            },
            target,
            target_digest,
            cancellation_id: if matches!(operation, IntegrationOperation::Cancel) {
                resource.cancellation_id.clone()
            } else {
                keith_platform_contracts::CancellationId::new()
            },
            reply_route: None,
            audit_correlation: AuditCorrelationId::new(),
            external_effect,
        };
        self.enqueue(ClientCommand::Integration(IntegrationCommand::Mutate(
            Box::new(IntegrationMutation {
                profile_id: resource.profile_id,
                service,
                resource_id: Some(resource.id),
                native_resource_key: resource.native_resource_key,
                display_label: resource.display_label,
                expected_revision: Some(resource.revision),
                idempotency_key,
                operation,
                authority,
            }),
        )));
    }

    fn request_evolution_enable(&mut self) {
        self.enqueue(ClientCommand::Evolution(EvolutionCommand::Enable {
            disclosure_acknowledged: true,
        }));
        self.log("Enablement is installation-owner only. This client carries no authority or identity; the installation control surface will provide guidance if required.");
    }

    fn request_evolution_disable(&mut self) {
        self.enqueue(ClientCommand::Evolution(EvolutionCommand::Disable {
            reason: "Requested from the terminal evolution surface".into(),
        }));
    }

    fn request_evolution_restore(&mut self) {
        self.enqueue(ClientCommand::Evolution(
            EvolutionCommand::RestoreBaseline {
                reason: "Requested from the terminal evolution surface".into(),
            },
        ));
    }

    fn steer(&mut self) {
        let text = self.composer.trim().to_owned();
        let Some(session_id) = self.attached_session.clone() else {
            return;
        };
        if text.is_empty() {
            return;
        }
        let command = ClientCommand::Steer(SteerAction {
            session_id: session_id.clone(),
            text: text.clone(),
            delivery: DeliveryPolicy::NextTurnBoundary,
        });
        if !self.enqueue(command) {
            return;
        }
        self.track_pending_prompt(session_id, text.clone());
        self.record_history(text);
        self.clear_composer();
    }

    fn branch_from_latest(&mut self) {
        let Some(reducer) = &self.reducer else {
            return;
        };
        let Some(message) = reducer.snapshot().messages.last() else {
            return;
        };
        let Some(parent_entry_id) = message.final_id.clone() else {
            self.log("A completed Keith reply is required before branching");
            return;
        };
        let Some(session_id) = self.attached_session.clone() else {
            return;
        };
        self.enqueue(ClientCommand::BranchSession(
            keith_protocol::BranchRequest {
                session_id,
                parent_entry_id: parent_entry_id.0,
                label: None,
            },
        ));
    }

    fn track_pending_prompt(&mut self, session_id: SessionId, text: String) {
        let (authoritative, message_anchor) = self.reducer.as_ref().map_or((0, 0), |reducer| {
            let messages = &reducer.snapshot().messages;
            let occurrences = messages
                .iter()
                .filter(|message| {
                    message.role == keith_protocol::MessageRole::User && message.text == text
                })
                .count();
            (occurrences, messages.len())
        });
        let already_pending = self
            .pending_prompts
            .iter()
            .filter(|prompt| {
                prompt.session_id == session_id
                    && prompt.text == text
                    && prompt.state != PendingPromptState::Failed
            })
            .count();
        self.pending_prompts.push_back(PendingPrompt {
            session_id: session_id.clone(),
            text,
            state: PendingPromptState::Sending,
            command_id: None,
            authoritative_occurrence: authoritative
                .saturating_add(already_pending)
                .saturating_add(1),
            message_anchor,
        });
        if self.turn_session.as_ref() != Some(&session_id) || self.turn_started_at.is_none() {
            self.turn_session = Some(session_id);
            self.turn_started_at = Some(Instant::now());
        }
    }

    fn assign_pending_prompt_command(
        &mut self,
        session_id: &SessionId,
        text: &str,
        command_id: CommandId,
    ) {
        if let Some(prompt) = self.pending_prompts.iter_mut().find(|prompt| {
            &prompt.session_id == session_id && prompt.text == text && prompt.command_id.is_none()
        }) {
            prompt.command_id = Some(command_id);
        }
    }

    fn mark_pending_prompt(&mut self, command_id: Option<&CommandId>, state: PendingPromptState) {
        let prompt = match command_id {
            Some(command_id) => self
                .pending_prompts
                .iter_mut()
                .find(|prompt| prompt.command_id.as_ref() == Some(command_id)),
            None => self
                .pending_prompts
                .iter_mut()
                .find(|prompt| prompt.state == PendingPromptState::Sending),
        };
        if let Some(prompt) = prompt {
            prompt.state = state;
        }
    }

    fn recover_new_session(&mut self, command_id: Option<&CommandId>) {
        let should_recover = match (&self.session_transition, command_id) {
            (SessionTransition::Creating { .. }, None) => true,
            (
                SessionTransition::Creating {
                    command_id: Some(expected),
                    ..
                },
                Some(command_id),
            ) => expected == command_id,
            (SessionTransition::Stable, _)
            | (
                SessionTransition::Creating {
                    command_id: None, ..
                },
                Some(_),
            ) => false,
        };
        if !should_recover {
            return;
        }
        let previous_session = match std::mem::take(&mut self.session_transition) {
            SessionTransition::Creating {
                previous_session, ..
            } => previous_session,
            SessionTransition::Stable => None,
        };
        if let Some(session_id) = previous_session {
            self.attach(session_id);
        } else {
            self.list_sessions();
        }
    }

    fn reconcile_pending_prompts(&mut self) {
        let Some(reducer) = &self.reducer else {
            return;
        };
        let session_id = &reducer.snapshot().session.session_id;
        let authoritative = reducer
            .snapshot()
            .messages
            .iter()
            .filter(|message| message.role == keith_protocol::MessageRole::User)
            .map(|message| message.text.clone())
            .collect::<Vec<_>>();
        self.pending_prompts.retain(|prompt| {
            if &prompt.session_id != session_id || prompt.state == PendingPromptState::Failed {
                return true;
            }
            let occurrences = authoritative
                .iter()
                .filter(|text| text.as_str() == prompt.text.as_str())
                .count();
            occurrences < prompt.authoritative_occurrence
        });
    }

    fn sync_turn_activity(&mut self) {
        let local_pending = self.turn_session.as_ref().is_some_and(|session_id| {
            self.pending_prompts.iter().any(|prompt| {
                &prompt.session_id == session_id && prompt.state != PendingPromptState::Failed
            })
        });
        let Some(reducer) = &self.reducer else {
            if !local_pending {
                self.turn_session = None;
                self.turn_started_at = None;
            }
            return;
        };
        let snapshot = reducer.snapshot();
        let session_id = &snapshot.session.session_id;
        let terminal = snapshot.terminal.is_some()
            || matches!(
                snapshot.presence.state,
                PresenceState::Completed | PresenceState::Failed
            );
        if terminal {
            self.turn_session = None;
            self.turn_started_at = None;
            return;
        }
        let authoritative_active = matches!(
            snapshot.presence.state,
            PresenceState::Thinking
                | PresenceState::UsingTools
                | PresenceState::WaitingChild
                | PresenceState::WaitingExternal
        );
        if authoritative_active {
            if self.turn_session.as_ref() != Some(session_id) || self.turn_started_at.is_none() {
                self.turn_session = Some(session_id.clone());
                self.turn_started_at = Some(Instant::now());
            }
        } else if !local_pending {
            self.turn_session = None;
            self.turn_started_at = None;
        }
    }

    fn apply_command_snapshot(
        &mut self,
        snapshot: keith_protocol::SessionSnapshot,
        command_id: &CommandId,
    ) {
        let created_session = matches!(
            &self.session_transition,
            SessionTransition::Creating {
                command_id: Some(expected),
                ..
            } if expected == command_id
        );
        let accepts = match &self.session_transition {
            SessionTransition::Stable => true,
            SessionTransition::Creating {
                command_id: Some(expected),
                ..
            } => expected == command_id,
            SessionTransition::Creating {
                command_id: None, ..
            } => false,
        };
        if !accepts {
            return;
        }
        let session_id = snapshot.session.session_id.clone();
        self.session_transition = SessionTransition::Stable;
        self.apply_snapshot_unchecked(snapshot);
        if created_session {
            self.attach(session_id);
        }
    }

    fn apply_snapshot(&mut self, snapshot: keith_protocol::SessionSnapshot) {
        if self.is_starting_new_session() {
            return;
        }
        self.apply_snapshot_unchecked(snapshot);
    }

    fn apply_snapshot_unchecked(&mut self, snapshot: keith_protocol::SessionSnapshot) {
        self.ingest_projected_prompt_history(&snapshot.messages);
        let session_id = snapshot.session.session_id.clone();
        let session_is_new = !self
            .sessions
            .iter()
            .any(|session| session.session_id == session_id);
        self.attached_session = Some(session_id);
        self.scroll_from_end = 0;
        let virtualization = VirtualizationConfig::new(2_048, 256, 16)
            .expect("fixed virtualization limits are valid");
        let replace = self.reducer.as_ref().is_some_and(|reducer| {
            reducer.snapshot().session.session_id != snapshot.session.session_id
                || reducer.snapshot().session.root_tree_id != snapshot.session.root_tree_id
        });
        if replace {
            self.reducer = None;
        }
        match &mut self.reducer {
            Some(reducer) => {
                if let Err(error) = reducer.apply_snapshot(snapshot) {
                    self.log(format!("Snapshot rejected: {error}"));
                }
            }
            None => match ProjectionReducer::new(snapshot, virtualization) {
                Ok(reducer) => self.reducer = Some(reducer),
                Err(error) => self.log(format!("Snapshot rejected: {error}")),
            },
        }
        if session_is_new {
            self.list_sessions();
        }
        self.reconcile_pending_prompts();
        self.sync_turn_activity();
    }

    fn enqueue(&mut self, command: ClientCommand) -> bool {
        if self.pending_commands.len() == MAX_PENDING_COMMANDS {
            self.log("Command queue is full");
            return false;
        }
        self.pending_commands.push_back(command);
        true
    }

    fn log(&mut self, message: impl Into<String>) {
        self.logs.push_back(message.into());
        while self.logs.len() > MAX_LOG_LINES {
            self.logs.pop_front();
        }
    }

    fn insert_text(&mut self, text: &str) {
        self.reset_history_navigation();
        let available = MAX_COMPOSER_BYTES.saturating_sub(self.composer.len());
        let text = bounded_str(text, available);
        self.composer.insert_str(self.cursor_byte, text);
        self.cursor_byte += text.len();
        self.completion_selection = 0;
    }

    fn backspace(&mut self) {
        if self.cursor_byte == 0 {
            return;
        }
        let previous = self.composer[..self.cursor_byte]
            .char_indices()
            .next_back()
            .map_or(0, |(index, _)| index);
        self.composer.drain(previous..self.cursor_byte);
        self.cursor_byte = previous;
        self.reset_history_navigation();
        self.completion_selection = 0;
    }

    fn delete_forward(&mut self) {
        if self.cursor_byte == self.composer.len() {
            return;
        }
        let width = self.composer[self.cursor_byte..]
            .chars()
            .next()
            .map_or(0, char::len_utf8);
        self.composer
            .drain(self.cursor_byte..self.cursor_byte + width);
        self.reset_history_navigation();
        self.completion_selection = 0;
    }

    fn move_left(&mut self) {
        if self.cursor_byte > 0 {
            self.cursor_byte = self.composer[..self.cursor_byte]
                .char_indices()
                .next_back()
                .map_or(0, |(index, _)| index);
        }
    }

    fn move_right(&mut self) {
        if self.cursor_byte < self.composer.len() {
            self.cursor_byte += self.composer[self.cursor_byte..]
                .chars()
                .next()
                .map_or(0, char::len_utf8);
        }
    }

    fn clear_composer(&mut self) {
        self.composer.clear();
        self.cursor_byte = 0;
        self.history_index = None;
        self.history_draft.clear();
        self.completion_selection = 0;
    }

    fn reset_history_navigation(&mut self) {
        self.history_index = None;
        self.history_draft.clear();
    }

    fn record_history(&mut self, text: String) {
        if self.input_history.back() != Some(&text) {
            self.input_history.push_back(text);
        }
        while self.input_history.len() > MAX_INPUT_HISTORY {
            self.input_history.pop_front();
        }
        self.history_index = None;
        self.history_draft.clear();
    }

    fn ingest_projected_prompt_history(&mut self, messages: &[keith_protocol::MessageProjection]) {
        for message in messages {
            if message.role != keith_protocol::MessageRole::User {
                continue;
            }
            let text = message.text.trim();
            if text.is_empty() || self.input_history.iter().any(|entry| entry == text) {
                continue;
            }
            self.input_history.push_back(text.to_owned());
        }
        while self.input_history.len() > MAX_INPUT_HISTORY {
            self.input_history.pop_front();
        }
    }

    fn history_previous(&mut self) {
        if self.input_history.is_empty() {
            return;
        }
        let index = if let Some(index) = self.history_index {
            index.saturating_sub(1)
        } else {
            self.history_draft.clone_from(&self.composer);
            self.input_history.len() - 1
        };
        self.history_index = Some(index);
        self.composer.clone_from(&self.input_history[index]);
        self.cursor_byte = self.composer.len();
    }

    fn history_next(&mut self) {
        let Some(index) = self.history_index else {
            return;
        };
        if index + 1 < self.input_history.len() {
            let next = index + 1;
            self.history_index = Some(next);
            self.composer.clone_from(&self.input_history[next]);
        } else {
            self.history_index = None;
            self.composer = std::mem::take(&mut self.history_draft);
        }
        self.cursor_byte = self.composer.len();
    }

    fn move_word_left(&mut self) {
        while self.cursor_byte > 0 {
            let previous = self.composer[..self.cursor_byte]
                .char_indices()
                .next_back()
                .map_or(0, |(index, _)| index);
            let character = self.composer[previous..self.cursor_byte]
                .chars()
                .next()
                .unwrap_or(' ');
            self.cursor_byte = previous;
            if !character.is_whitespace() {
                break;
            }
        }
        while self.cursor_byte > 0 {
            let previous = self.composer[..self.cursor_byte]
                .char_indices()
                .next_back()
                .map_or(0, |(index, _)| index);
            let character = self.composer[previous..self.cursor_byte]
                .chars()
                .next()
                .unwrap_or(' ');
            if character.is_whitespace() {
                break;
            }
            self.cursor_byte = previous;
        }
    }

    fn move_word_right(&mut self) {
        while self.cursor_byte < self.composer.len() {
            let character = self.composer[self.cursor_byte..]
                .chars()
                .next()
                .unwrap_or(' ');
            if !character.is_whitespace() {
                break;
            }
            self.cursor_byte += character.len_utf8();
        }
        while self.cursor_byte < self.composer.len() {
            let character = self.composer[self.cursor_byte..]
                .chars()
                .next()
                .unwrap_or(' ');
            if character.is_whitespace() {
                break;
            }
            self.cursor_byte += character.len_utf8();
        }
    }

    fn delete_word_left(&mut self) {
        let original = self.cursor_byte;
        self.move_word_left();
        self.composer.drain(self.cursor_byte..original);
        self.reset_history_navigation();
        self.completion_selection = 0;
    }
}

fn payload_label(payload: &ResponsePayload) -> &'static str {
    match payload {
        ResponsePayload::Profiles(_) => "profile",
        ResponsePayload::Sessions(_) => "session",
        ResponsePayload::Snapshot(_) => "snapshot",
        ResponsePayload::Goal(_) => "goal",
        ResponsePayload::Child(_) => "child",
        ResponsePayload::Schedule(_) => "schedule",
        ResponsePayload::Memory(_) => "memory",
        ResponsePayload::Export(_) => "export",
        ResponsePayload::Background(_) => "background",
        ResponsePayload::Artifact(_) => "artifact",
        ResponsePayload::DeliveryClaim(_) => "delivery",
        ResponsePayload::ChannelAccounts(_) | ResponsePayload::ChannelAccount(_) => "channel",
        ResponsePayload::ProfileIntegrations(_) => "services",
        ResponsePayload::IntegrationResource(_) => "service resource",
        ResponsePayload::IntegrationDeletion(_) => "service deletion",
        ResponsePayload::HarnessRepairs(_) => "harness repairs",
        ResponsePayload::Evolution(_) => "evolution",
    }
}

fn parse_integration_service(value: &str) -> Option<IntegrationService> {
    match value {
        "channel" | "channels" | "channel_account" => Some(IntegrationService::ChannelAccount),
        "acp" | "acp_connection" => Some(IntegrationService::AcpConnection),
        "plugin" | "plugins" => Some(IntegrationService::Plugin),
        "app" | "apps" | "connected_app" => Some(IntegrationService::ConnectedApp),
        "computer" | "computers" | "computer_session" => Some(IntegrationService::ComputerSession),
        "control" | "control_lease" => Some(IntegrationService::ControlLease),
        "recording" | "recordings" => Some(IntegrationService::Recording),
        "recipe" | "recipes" => Some(IntegrationService::Recipe),
        "harness" | "harness_repair" => Some(IntegrationService::HarnessRepair),
        _ => None,
    }
}

const fn integration_service_label(service: IntegrationService) -> &'static str {
    match service {
        IntegrationService::ChannelAccount => "Channel account",
        IntegrationService::AcpConnection => "ACP connection",
        IntegrationService::Plugin => "Plugin",
        IntegrationService::ConnectedApp => "Connected app",
        IntegrationService::ComputerSession => "Computer session",
        IntegrationService::ControlLease => "Computer control",
        IntegrationService::Recording => "Recording",
        IntegrationService::Recipe => "Recipe",
        IntegrationService::HarnessRepair => "Harness repair",
    }
}

const fn integration_operation_authority(
    operation: IntegrationOperation,
) -> (Capability, ActionRisk) {
    match operation {
        IntegrationOperation::Test | IntegrationOperation::Export => {
            (Capability::Read, ActionRisk::ReadOnly)
        }
        IntegrationOperation::StopRecording | IntegrationOperation::StartRecording => (
            Capability::DemonstrationRecord,
            ActionRisk::ReversibleLocalWrite,
        ),
        IntegrationOperation::Reverse => {
            (Capability::HarnessReverse, ActionRisk::ReversibleLocalWrite)
        }
        IntegrationOperation::Delete => (Capability::Delete, ActionRisk::Delete),
        IntegrationOperation::Connect | IntegrationOperation::Configure => {
            (Capability::AccountChange, ActionRisk::AccountChange)
        }
        IntegrationOperation::Install => (Capability::PluginInstall, ActionRisk::AccountChange),
        IntegrationOperation::TakeControl => (
            Capability::ComputerControl,
            ActionRisk::IrreversibleComputerInput,
        ),
        IntegrationOperation::Publish => {
            (Capability::RecipePublish, ActionRisk::ExternalCommunication)
        }
        IntegrationOperation::Start
        | IntegrationOperation::Pause
        | IntegrationOperation::Resume
        | IntegrationOperation::Stop
        | IntegrationOperation::Cancel
        | IntegrationOperation::ReleaseControl => {
            (Capability::LocalWrite, ActionRisk::ReversibleLocalWrite)
        }
    }
}

fn bounded_text(text: String, limit: usize) -> String {
    if text.len() <= limit {
        text
    } else {
        bounded_str(&text, limit).to_owned()
    }
}

fn bounded_str(text: &str, limit: usize) -> &str {
    let mut boundary = limit.min(text.len());
    while !text.is_char_boundary(boundary) {
        boundary -= 1;
    }
    &text[..boundary]
}

fn compact_conversation_title(text: &str) -> String {
    let title = text.split_whitespace().collect::<Vec<_>>().join(" ");
    let mut characters = title.chars();
    let compact = characters.by_ref().take(48).collect::<String>();
    if characters.next().is_some() {
        format!("{compact}…")
    } else if compact.is_empty() {
        "New conversation".into()
    } else {
        compact
    }
}

pub fn selected_entry_id(app: &TuiApp) -> Option<EntityId> {
    app.reducer
        .as_ref()?
        .snapshot()
        .messages
        .last()
        .map(|message| message.message_id.as_entity_id().clone())
}

#[cfg(test)]
mod tests {
    use crossterm::event::{KeyEventKind, KeyEventState};
    use keith_agent_types::{MessageId, ProfileId, Revision, Sequence, WorkspaceId};
    use keith_platform_contracts::{CancellationId, LifecycleState, ResourceBounds};
    use ratatui::Terminal;
    use ratatui::backend::TestBackend;

    use super::*;

    fn key(code: KeyCode, modifiers: KeyModifiers) -> KeyEvent {
        KeyEvent {
            code,
            modifiers,
            kind: KeyEventKind::Press,
            state: KeyEventState::NONE,
        }
    }

    fn rendered(app: &TuiApp, width: u16, height: u16) -> String {
        let backend = TestBackend::new(width, height);
        let mut terminal = Terminal::new(backend).unwrap();
        terminal.draw(|frame| render(frame, app)).unwrap();
        let buffer = terminal.backend().buffer();
        let mut output = String::new();
        for y in 0..height {
            for x in 0..width {
                output.push_str(buffer.cell((x, y)).unwrap().symbol());
            }
            output.push('\n');
        }
        output
    }

    #[test]
    fn keyboard_paste_unicode_and_command_intents_are_bounded() {
        let mut app = TuiApp::new(Accessibility::default());
        app.attach(SessionId::new());
        assert!(matches!(
            app.next_command(),
            Some(ClientCommand::AttachSession(_))
        ));
        for character in ['H', 'é', '界'] {
            app.handle_key(key(KeyCode::Char(character), KeyModifiers::NONE));
        }
        app.handle_key(key(KeyCode::Enter, KeyModifiers::ALT));
        app.handle_paste("line\r\nsecond");
        assert_eq!(app.composer, "Hé界\nline\nsecond");
        assert_eq!(app.cursor_byte, app.composer.len());
        assert!(app.composer_display_width() >= 14);
        app.handle_key(key(KeyCode::Enter, KeyModifiers::NONE));
        let command = app.next_command().expect("submitted prompt command");
        assert!(matches!(
            &command,
            ClientCommand::SubmitPrompt(SubmitPrompt { text, .. })
                if text == "Hé界\nline\nsecond"
        ));
        let envelope = app.command_envelope(command);
        let command_id = envelope.command_id.clone();
        app.command_dispatched(&command_id);
        let optimistic = rendered(&app, 96, 20);
        assert!(optimistic.contains("› Hé界"));
        assert!(!optimistic.contains("sending"));
        assert!(optimistic.contains("Working (0s · Esc to interrupt)"));
        assert!(app.composer.is_empty());

        app.apply_wire_message(WireMessage::CommandResult(
            keith_protocol::CommandResultEnvelope {
                protocol: keith_agent_types::CURRENT_PROTOCOL_VERSION,
                command_id,
                completed_at: UtcTimestamp::UNIX_EPOCH,
                result: CommandResult::Accepted { action_id: None },
            },
        ));
        let accepted = rendered(&app, 96, 20);
        assert!(accepted.contains("› Hé界"));
        assert!(!accepted.contains("sending"));

        app.handle_paste(&"x".repeat(MAX_COMPOSER_BYTES + 10));
        assert_eq!(app.composer.len(), MAX_COMPOSER_BYTES);
        app.handle_key(key(KeyCode::Backspace, KeyModifiers::NONE));
        assert_eq!(app.composer.len(), MAX_COMPOSER_BYTES - 1);
        assert_eq!(
            app.handle_key(key(KeyCode::Char('e'), KeyModifiers::CONTROL)),
            AppAction::OpenExternalEditor
        );
    }

    #[test]
    fn command_completion_history_interrupt_and_new_session_match_agent_cli_conventions() {
        let mut app = TuiApp::new(Accessibility::default());
        let session_id = SessionId::new();
        app.attach(session_id);
        app.next_command();

        app.handle_paste("first prompt");
        app.handle_key(key(KeyCode::Enter, KeyModifiers::NONE));
        assert!(matches!(
            app.next_command(),
            Some(ClientCommand::SubmitPrompt(SubmitPrompt {
                text,
                delivery: DeliveryPolicy::Immediate,
                ..
            })) if text == "first prompt"
        ));
        app.handle_paste("unsent draft");
        app.handle_key(key(KeyCode::Up, KeyModifiers::NONE));
        assert_eq!(app.composer, "first prompt");
        app.handle_key(key(KeyCode::Down, KeyModifiers::NONE));
        assert_eq!(app.composer, "unsent draft");

        app.replace_composer("/mo".into());
        assert!(
            app.slash_suggestions()
                .iter()
                .any(|(command, _)| *command == "/models")
        );
        app.handle_key(key(KeyCode::Tab, KeyModifiers::NONE));
        assert_eq!(app.composer, "/models");

        app.handle_key(key(KeyCode::Char('c'), KeyModifiers::CONTROL));
        assert!(app.composer.is_empty());
        assert!(!app.quit);

        app.ingest_projected_prompt_history(&[keith_protocol::MessageProjection {
            message_id: MessageId::new(),
            final_id: Some(keith_agent_types::EntryId::new()),
            role: keith_protocol::MessageRole::User,
            text: "prompt recovered from the attached session".into(),
            committed: true,
        }]);
        app.handle_key(key(KeyCode::Up, KeyModifiers::NONE));
        assert_eq!(app.composer, "prompt recovered from the attached session");
        app.handle_key(key(KeyCode::Char('c'), KeyModifiers::CONTROL));

        let profile_id = ProfileId::new();
        let workspace_id = WorkspaceId::new();
        app.profiles.push(ProfileSummary {
            id: profile_id.clone(),
            workspace_id: workspace_id.clone(),
            display_name: "Keith".into(),
            enabled: true,
        });
        app.start_new_session();
        assert!(app.attached_session.is_none());
        assert!(app.reducer.is_none());
        assert!(app.is_starting_new_session());
        let starting = rendered(&app, 96, 20);
        assert!(starting.contains("Starting a new conversation"));
        assert!(!starting.contains("first prompt"));
        let create = app.next_command().expect("create session command");
        assert!(matches!(
            &create,
            ClientCommand::CreateSession(CreateSession {
                profile_id: selected_profile,
                workspace_id: selected_workspace,
                title: None,
            }) if selected_profile == &profile_id && selected_workspace == &workspace_id
        ));
        app.command_envelope(create);
        assert!(matches!(
            app.session_transition,
            SessionTransition::Creating {
                command_id: Some(_),
                ..
            }
        ));

        assert_eq!(
            app.handle_key(key(KeyCode::Char('q'), KeyModifiers::CONTROL)),
            AppAction::None
        );
        assert!(!app.quit);
        assert_eq!(
            app.handle_key(key(KeyCode::Char('d'), KeyModifiers::CONTROL)),
            AppAction::Quit
        );
        assert!(app.quit);

        let mut detached = TuiApp::new(Accessibility::default());
        detached.replace_composer("/quit".into());
        detached.handle_key(key(KeyCode::Enter, KeyModifiers::NONE));
        assert!(detached.quit);

        let main_source = include_str!("main.rs");
        assert!(main_source.contains("ratatui::try_init()"));
        assert!(main_source.contains("terminal.clear()"));
        assert!(!main_source.contains("insert_before"));
    }

    #[test]
    fn every_temporary_overlay_is_searchable_and_conversation_survives_all_widths() {
        assert!(client_parity().is_full());
        for mode in [
            ColorMode::TrueColor,
            ColorMode::Ansi256,
            ColorMode::NoColor,
            ColorMode::HighContrast,
        ] {
            let mut app = TuiApp::new(Accessibility {
                color_mode: mode,
                reduced_motion: true,
            });
            let mut visited = Vec::new();
            for overlay in TuiOverlay::ALL {
                app.open_overlay(overlay);
                visited.push(app.overlay.unwrap());
                let wide = rendered(&app, 120, 32);
                let narrow = rendered(&app, 60, 20);
                let tiny = rendered(&app, 36, 8);
                assert!(wide.contains(overlay.label()));
                assert!(narrow.contains(overlay.label()));
                assert!(tiny.contains("Keith"));
                for forbidden in ["┌", "┐", "└", "┘", "│", "─"] {
                    assert!(!wide.contains(forbidden));
                }
                app.handle_key(key(KeyCode::Char('q'), KeyModifiers::NONE));
                assert_eq!(app.overlay_query, "q");
                app.handle_key(key(KeyCode::Esc, KeyModifiers::NONE));
                assert!(app.overlay.is_none());
            }
            assert_eq!(visited, TuiOverlay::ALL);
        }
        let source = include_str!("render.rs").to_ascii_lowercase();
        assert!(!source.contains("purple"));
        assert!(!source.contains("glow"));
        assert!(!source.contains(".borders("));
    }

    #[test]
    fn terminal_control_sequences_are_neutralized_before_rendering() {
        let output = render::terminal_safe("safe\u{1b}[2J\u{7}still visible");
        assert_eq!(output, "safe�[2J�still visible");
    }

    #[test]
    fn service_surface_emits_an_exact_low_risk_mutation() {
        let mut app = TuiApp::new(Accessibility::default());
        let session_id = SessionId::new();
        let profile_id = ProfileId::new();
        let resource_id = EntityId::new();
        let cancellation_id = CancellationId::new();
        app.attach(session_id.clone());
        assert!(matches!(
            app.next_command(),
            Some(ClientCommand::AttachSession(_))
        ));
        app.integrations = Some(ProfileIntegrationsProjection {
            profile_id: profile_id.clone(),
            through_sequence: Sequence::new(7),
            services: vec![keith_protocol::IntegrationServiceProjection {
                service: IntegrationService::ConnectedApp,
                availability: keith_protocol::IntegrationAvailabilityProjection::Available,
            }],
            resources: vec![keith_protocol::IntegrationResourceProjection {
                id: resource_id.clone(),
                profile_id: profile_id.clone(),
                owning_session_id: Some(session_id.clone()),
                service: IntegrationService::ConnectedApp,
                native_resource_key: "github-primary".into(),
                display_label: "GitHub".into(),
                lifecycle: LifecycleState::Active,
                cancellation_id: cancellation_id.clone(),
                audit_correlation: AuditCorrelationId::new(),
                bounds: ResourceBounds {
                    max_concurrency: 1,
                    max_duration_ms: 60_000,
                    max_cpu_time_ms: 30_000,
                    max_retries: 1,
                    max_input_bytes: 1_024,
                    max_output_bytes: 1_024,
                    max_memory_bytes: 1_048_576,
                    max_disk_bytes: 1_048_576,
                    max_events_per_minute: 60,
                },
                controls: [keith_protocol::IntegrationControl::Cancel]
                    .into_iter()
                    .collect(),
                safe_error: None,
                revision: Revision::new(4),
                created_at: UtcTimestamp::UNIX_EPOCH,
                updated_at: UtcTimestamp::UNIX_EPOCH,
            }],
        });

        app.open_overlay(TuiOverlay::Services);
        let rows = app.overlay_rows();
        assert!(
            rows.iter()
                .any(|row| row.contains("Connected app — enabled"))
        );
        assert!(rows.iter().any(|row| {
            row.contains("GitHub") && row.contains("active") && row.contains("revision 4")
        }));
        app.next_command();
        app.handle_key(key(KeyCode::Esc, KeyModifiers::NONE));
        app.replace_composer(format!("/service-cancel connected_app {resource_id}"));
        app.handle_key(key(KeyCode::Enter, KeyModifiers::NONE));

        let Some(ClientCommand::Integration(IntegrationCommand::Mutate(mutation))) =
            app.next_command()
        else {
            panic!("expected an integration mutation");
        };
        assert_eq!(mutation.profile_id, profile_id);
        assert_eq!(mutation.resource_id, Some(resource_id));
        assert_eq!(mutation.expected_revision, Some(Revision::new(4)));
        assert_eq!(mutation.operation, IntegrationOperation::Cancel);
        assert_eq!(mutation.authority.session_id, session_id);
        assert_eq!(mutation.authority.cancellation_id, cancellation_id);
        assert_eq!(mutation.authority.risk, ActionRisk::ReversibleLocalWrite);
        assert_eq!(
            mutation.authority.approval.state,
            ApprovalState::NotRequired
        );
    }

    #[test]
    fn settled_transcript_wraps_unicode_and_preserves_code_indentation() {
        let message = keith_protocol::MessageProjection {
            message_id: MessageId::new(),
            final_id: Some(keith_agent_types::EntryId::new()),
            role: keith_protocol::MessageRole::Assistant,
            text: "result\n    let value = \"界界界界界\";\u{1b}[2J".into(),
            committed: true,
        };
        let rendered = settled_transcript_lines(&message, 18, ColorMode::NoColor)
            .into_iter()
            .map(|line| line.to_string())
            .collect::<Vec<_>>();
        assert_eq!(rendered.first().map(String::as_str), Some("• result"));
        assert!(rendered.iter().any(|line| line.starts_with("      let")));
        assert!(rendered.iter().any(|line| line.contains('�')));
        assert!(rendered.len() >= 4);
    }

    #[test]
    fn queue_retry_branch_resume_and_navigation_remain_protocol_commands() {
        let mut app = TuiApp::new(Accessibility::default());
        let session_id = SessionId::new();
        app.attach(session_id.clone());
        app.next_command();
        app.replace_composer("first prompt".into());
        app.handle_key(key(KeyCode::Enter, KeyModifiers::NONE));
        assert!(matches!(
            app.next_command(),
            Some(ClientCommand::SubmitPrompt(_))
        ));
        app.handle_key(key(KeyCode::Char('r'), KeyModifiers::CONTROL));
        assert!(matches!(
            app.next_command(),
            Some(ClientCommand::SubmitPrompt(_))
        ));
        app.handle_key(key(KeyCode::Char('x'), KeyModifiers::CONTROL));
        assert!(matches!(
            app.next_command(),
            Some(ClientCommand::Cancel(CancelTarget::Session(id))) if id == session_id
        ));
        app.handle_key(key(KeyCode::Char('u'), KeyModifiers::CONTROL));
        assert!(matches!(
            app.next_command(),
            Some(ClientCommand::ResumeSession { session_id: id }) if id == session_id
        ));
        app.select_model("provider".into(), "model".into());
        assert!(matches!(
            app.next_command(),
            Some(ClientCommand::SelectModel(selection))
                if selection.provider == "provider" && selection.model == "model"
        ));
        app.replace_composer("/model openai gpt-4.1-mini".into());
        app.handle_key(key(KeyCode::Enter, KeyModifiers::NONE));
        assert!(matches!(
            app.next_command(),
            Some(ClientCommand::SelectModel(selection))
                if selection.provider == "openai" && selection.model == "gpt-4.1-mini"
        ));
        app.replace_composer("/model deepseek".into());
        app.handle_key(key(KeyCode::Enter, KeyModifiers::NONE));
        assert!(matches!(
            app.next_command(),
            Some(ClientCommand::SelectModel(selection))
                if selection.provider == "deepseek" && selection.model == "deepseek-chat"
        ));
        app.replace_composer("/goal Ship the integration".into());
        app.handle_key(key(KeyCode::Enter, KeyModifiers::NONE));
        assert!(matches!(
            app.next_command(),
            Some(ClientCommand::CreateGoal(CreateGoal { objective, .. }))
                if objective == "Ship the integration"
        ));
        app.replace_composer("/child Verify the integration".into());
        app.handle_key(key(KeyCode::Enter, KeyModifiers::NONE));
        assert!(matches!(
            app.next_command(),
            Some(ClientCommand::CreateChild(CreateChild { objective, .. }))
                if objective == "Verify the integration"
        ));
        app.replace_composer("/export markdown".into());
        app.handle_key(key(KeyCode::Enter, KeyModifiers::NONE));
        assert!(matches!(
            app.next_command(),
            Some(ClientCommand::Export(ExportRequest {
                format: ExportFormat::Markdown,
                ..
            }))
        ));
        let goal_id = GoalId::new();
        app.replace_composer(format!("/cancel-goal {goal_id}"));
        app.handle_key(key(KeyCode::Enter, KeyModifiers::NONE));
        assert!(matches!(
            app.next_command(),
            Some(ClientCommand::Cancel(CancelTarget::Goal(id))) if id == goal_id
        ));
        let confirmation_id = EntityId::new();
        app.resolve_confirmation(
            confirmation_id.clone(),
            keith_protocol::ConfirmationDecision::AllowOnce,
        );
        assert!(matches!(
            app.next_command(),
            Some(ClientCommand::ResolveConfirmation(resolution))
                if resolution.confirmation_id == confirmation_id
        ));
        app.handle_key(key(KeyCode::Char('s'), KeyModifiers::CONTROL));
        assert_eq!(app.overlay, Some(TuiOverlay::Sessions));
        assert!(matches!(
            app.next_command(),
            Some(ClientCommand::ListSessions(_))
        ));
        app.handle_key(key(KeyCode::Esc, KeyModifiers::NONE));
        assert!(app.overlay.is_none());
        app.handle_key(key(KeyCode::Tab, KeyModifiers::NONE));
        assert_eq!(app.overlay, Some(TuiOverlay::Commands));
    }

    #[test]
    fn evolution_parity_has_real_installation_scoped_emit_paths() {
        let mut app = TuiApp::new(Accessibility::default());
        app.attach(SessionId::new());
        app.next_command();

        for input in [
            "/evolution-enable",
            "/evolution-disable",
            "/evolution-restore",
        ] {
            app.replace_composer(input.into());
            app.handle_key(key(KeyCode::Enter, KeyModifiers::NONE));
        }
        let commands = [
            app.next_command().unwrap(),
            app.next_command().unwrap(),
            app.next_command().unwrap(),
        ];
        assert!(matches!(
            commands[0],
            ClientCommand::Evolution(EvolutionCommand::Enable { .. })
        ));
        assert!(matches!(
            commands[1],
            ClientCommand::Evolution(EvolutionCommand::Disable { .. })
        ));
        assert!(matches!(
            commands[2],
            ClientCommand::Evolution(EvolutionCommand::RestoreBaseline { .. })
        ));
        for command in commands {
            assert!(app.command_envelope(command).session_id.is_none());
        }

        app.open_overlay(TuiOverlay::Evolution);
        assert!(matches!(
            app.next_command(),
            Some(ClientCommand::Evolution(EvolutionCommand::Status))
        ));
        assert!(matches!(
            app.next_command(),
            Some(ClientCommand::Evolution(
                EvolutionCommand::BrowseLedger { .. }
            ))
        ));
        assert!(
            app.logs()
                .iter()
                .any(|line| line.contains("installation-owner only")
                    && line.contains("no authority or identity"))
        );

        let refusal =
            "Only the installation owner can enable self-evolution. Open installation settings.";
        app.apply_wire_message(WireMessage::CommandResult(
            keith_protocol::CommandResultEnvelope {
                protocol: keith_agent_types::CURRENT_PROTOCOL_VERSION,
                command_id: CommandId::new(),
                completed_at: UtcTimestamp::UNIX_EPOCH,
                result: CommandResult::Rejected(keith_protocol::CommandError {
                    error: keith_agent_types::CommonError::new(
                        keith_agent_types::ErrorCode::Unauthorized,
                        refusal,
                        false,
                    ),
                    unsupported_feature: None,
                }),
            },
        ));
        assert!(app.overlay_rows().iter().any(|row| row.contains(refusal)));
    }
}
