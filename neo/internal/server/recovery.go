// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"

	"matrix/neo/internal/codingruntime"
	"matrix/neo/internal/conversation"
	"matrix/neo/internal/sessionjournal"
)

const (
	recoveryConfirmClean    = "CLEAN"
	recoveryConfirmRecenter = "RECENTER"
	recoveryHandoffLimit    = 12_000
)

var recoverySecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bbearer\s+\S+`),
	regexp.MustCompile(`(?i)\b(?:[A-Z][A-Z0-9_]*_(?:TOKEN|SECRET|PASSWORD|KEY)|TOKEN|SECRET|PASSWORD|KEY)\s*=\s*\S+`),
	regexp.MustCompile(`(?i)([?&](?:token|api_key|access_token|key|secret|password)=)[^\s&#]+`),
}

type recoveryRequest struct {
	Confirm        string `json:"confirm"`
	ConversationID string `json:"conversation_id,omitempty"`
}

type execMaintenanceResult struct {
	OK                       bool                   `json:"ok"`
	StateDir                 string                 `json:"state_dir"`
	ServicesCleared          int                    `json:"services_cleared,omitempty"`
	ProcessesStopped         int                    `json:"processes_stopped,omitempty"`
	ShellProcessesStopped    int                    `json:"shell_processes_stopped,omitempty"`
	DetachedPIDAliases       int                    `json:"detached_pid_aliases,omitempty"`
	DetachedShellAliases     int                    `json:"detached_shell_aliases,omitempty"`
	FailedServices           []string               `json:"failed_services,omitempty"`
	FailedShellProcesses     []string               `json:"failed_shell_processes,omitempty"`
	ReconciledStaleProcesses int                    `json:"reconciled_stale_processes,omitempty"`
	ReconciledShellProcesses int                    `json:"reconciled_shell_processes,omitempty"`
	LogsRemoved              int                    `json:"logs_removed,omitempty"`
	TempFilesRemoved         int                    `json:"temp_files_removed,omitempty"`
	CacheEntriesRemoved      int                    `json:"cache_entries_removed,omitempty"`
	Junk                     *execMaintenanceResult `json:"junk,omitempty"`
}

type buildRecoveryResult struct {
	Interrupted int      `json:"interrupted"`
	Failed      []string `json:"failed,omitempty"`
}

func (s *Server) handleRecovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	switch strings.TrimPrefix(r.URL.Path, "/recovery/") {
	case "clean-environment":
		s.handleCleanEnvironment(w, r)
	case "recenter":
		s.handleRecenter(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "recovery action not found"})
	}
}

func decodeRecoveryRequest(w http.ResponseWriter, r *http.Request) (recoveryRequest, error) {
	var request recoveryRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return recoveryRequest{}, errors.New("invalid recovery request")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return recoveryRequest{}, errors.New("invalid recovery request")
	}
	return request, nil
}

func (s *Server) handleCleanEnvironment(w http.ResponseWriter, r *http.Request) {
	request, err := decodeRecoveryRequest(w, r)
	if err != nil || request.Confirm != recoveryConfirmClean {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "confirm must equal CLEAN"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	results, err := runExecMaintenance(ctx, "--clean-junk")
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{
			"ok": false, "error": err.Error(), "registries": results,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "registries": results})
}

func (s *Server) handleRecenter(w http.ResponseWriter, r *http.Request) {
	request, err := decodeRecoveryRequest(w, r)
	if err != nil || request.Confirm != recoveryConfirmRecenter {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "confirm must equal RECENTER"})
		return
	}
	if s.engine == nil || s.engine.conv == nil || !s.engine.conv.Enabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "conversation persistence is required for recovery"})
		return
	}
	sourceID := strings.TrimSpace(request.ConversationID)
	if strings.ContainsAny(sourceID, "/\\") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid conversation_id"})
		return
	}
	if sourceID == "" {
		if conversations := s.engine.conv.List(); len(conversations) > 0 {
			sourceID = conversations[0].ConversationID
		}
	}
	if sourceID != "" && !s.engine.conv.Exists(sourceID) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "source conversation not found"})
		return
	}

	handoff := s.engine.buildRecoveryHandoff(r.Context(), sourceID)
	halted := s.engine.haltAll()
	builds := s.engine.interruptRecoveryBuilds(r.Context())
	desktopStopped, desktopErr := s.engine.stopRecoveryDesktop(r.Context(), sourceID)

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	registries, maintenanceErr := runExecMaintenance(ctx, "--recenter-registry")
	cancel()
	if maintenanceErr != nil || len(builds.Failed) > 0 || desktopErr != nil {
		body := map[string]interface{}{
			"ok": false, "halted_runs": halted, "build_jobs": builds,
			"desktop_stopped": desktopStopped, "registries": registries,
		}
		var problems []string
		if maintenanceErr != nil {
			problems = append(problems, maintenanceErr.Error())
		}
		if len(builds.Failed) > 0 {
			problems = append(problems, "one or more Build jobs could not be confirmed interrupted")
		}
		if desktopErr != nil {
			problems = append(problems, "the disposable desktop could not be confirmed stopped")
		}
		body["error"] = strings.Join(problems, "; ")
		writeJSON(w, http.StatusBadGateway, body)
		return
	}

	newConversationID := synthConvID("recovery:" + sourceID)
	if !s.engine.conv.SetRecoveryHandoff(newConversationID, sourceID, handoff) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "recovery handoff could not be persisted"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":                  true,
		"conversation_id":     newConversationID,
		"source_conversation": sourceID,
		"halted_runs":         halted,
		"build_jobs":          builds,
		"desktop_stopped":     desktopStopped,
		"registries":          registries,
		"handoff": map[string]interface{}{
			"primary":  "latest user request and recent thread context",
			"failures": "secondary diagnostic context",
		},
	})
}

func runExecMaintenance(ctx context.Context, mode string) ([]execMaintenanceResult, error) {
	bridge, err := recoveryExecBridgePath()
	if err != nil {
		return nil, err
	}
	stateDirs := recoveryStateDirs()
	if len(stateDirs) == 0 {
		return nil, errors.New("no supervised service registries are configured")
	}
	results := make([]execMaintenanceResult, 0, len(stateDirs))
	var failures []string
	for _, stateDir := range stateDirs {
		command := exec.CommandContext(ctx, recoveryNodeBinary(), bridge, mode)
		command.Env = append(os.Environ(), "MATRIX_EXEC_STATE_DIR="+stateDir)
		output, runErr := command.CombinedOutput()
		var result execMaintenanceResult
		if err := json.Unmarshal(bytes.TrimSpace(output), &result); err != nil {
			failures = append(failures, fmt.Sprintf("%s returned invalid maintenance output", stateDir))
			continue
		}
		results = append(results, result)
		if runErr != nil || !result.OK {
			failures = append(failures, fmt.Sprintf("%s maintenance did not complete", stateDir))
		}
	}
	if len(failures) > 0 {
		return results, errors.New(strings.Join(failures, "; "))
	}
	return results, nil
}

func recoveryNodeBinary() string {
	if value := strings.TrimSpace(os.Getenv("MATRIX_RECOVERY_NODE_BINARY")); value != "" {
		return value
	}
	return "node"
}

func recoveryExecBridgePath() (string, error) {
	if value := strings.TrimSpace(os.Getenv("MATRIX_EXEC_BRIDGE_PATH")); value != "" {
		if info, err := os.Stat(value); err == nil && !info.IsDir() {
			return value, nil
		}
		return "", errors.New("configured exec bridge is unavailable")
	}
	const packaged = "/opt/matrix/tools/exec/exec.mjs"
	if info, err := os.Stat(packaged); err == nil && !info.IsDir() {
		return packaged, nil
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("exec bridge path is unavailable")
	}
	local := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", "..", "tools", "exec", "exec.mjs"))
	if info, err := os.Stat(local); err == nil && !info.IsDir() {
		return local, nil
	}
	return "", errors.New("exec bridge path is unavailable")
}

func recoveryStateDirs() []string {
	raw := strings.TrimSpace(os.Getenv("MATRIX_RECOVERY_EXEC_STATE_DIRS"))
	if raw == "" {
		dataDir := strings.TrimSpace(os.Getenv("MATRIX_DATA_DIR"))
		if dataDir == "" {
			dataDir = "/data"
		}
		raw = filepath.Join(dataDir, "services") + string(os.PathListSeparator) + filepath.Join(dataDir, "neo", "services")
	}
	seen := map[string]bool{}
	var dirs []string
	for _, value := range filepath.SplitList(raw) {
		value = filepath.Clean(strings.TrimSpace(value))
		if value == "." || value == string(filepath.Separator) || seen[value] {
			continue
		}
		seen[value] = true
		dirs = append(dirs, value)
	}
	return dirs
}

func (e *Engine) interruptRecoveryBuilds(parent context.Context) buildRecoveryResult {
	result := buildRecoveryResult{}
	if e == nil || e.buildJobs == nil {
		return result
	}
	jobs, err := e.buildJobs.List()
	if err != nil {
		result.Failed = append(result.Failed, "build-job-list")
		return result
	}
	for _, job := range jobs {
		if job.Terminal() {
			continue
		}
		if e.buildInterrupt != nil {
			ctx, cancel := context.WithTimeout(parent, 15*time.Second)
			err = e.buildInterrupt(ctx, job)
			cancel()
			if err != nil {
				result.Failed = append(result.Failed, job.ID)
				continue
			}
		}
		terminal := codingruntime.TerminalResult{
			Lifecycle: codingruntime.LifecycleInterrupted,
			Outcome:   codingruntime.OutcomeInterrupted,
			Summary:   "The user recentered the agent and stopped active work.",
		}
		settled := false
		for attempt := 0; attempt < 8; attempt++ {
			current, exists, readErr := e.buildJobs.Get(job.ID)
			if readErr != nil || !exists {
				break
			}
			if current.Terminal() {
				settled = true
				break
			}
			_, _, writeErr := e.buildJobs.CompareAndSetTerminal(job.ID, current.Revision, terminal)
			if errors.Is(writeErr, codingruntime.ErrRevisionConflict) {
				continue
			}
			settled = writeErr == nil
			break
		}
		if settled {
			result.Interrupted++
		} else {
			result.Failed = append(result.Failed, job.ID)
		}
	}
	sort.Strings(result.Failed)
	return result
}

func (e *Engine) stopRecoveryDesktop(parent context.Context, conversationID string) (bool, error) {
	if e == nil || e.dojo == nil {
		return false, nil
	}
	session, ok := e.dojo.SessionForConvOrLive(conversationID)
	if !ok {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	if err := e.dojo.Teardown(ctx, session.ID, "user_recenter"); err != nil {
		return false, err
	}
	return true, nil
}

func (e *Engine) buildRecoveryHandoff(ctx context.Context, conversationID string) string {
	history := []conversation.Turn{}
	if e != nil && e.conv != nil && conversationID != "" {
		history = e.conv.History(conversationID)
	}

	var checkpoint sessionjournal.SemanticCheckpoint
	if e != nil && e.journal != nil && conversationID != "" {
		if events, err := e.journal.Read(ctx, conversationID, 1, 0); err == nil {
			checkpoint = sessionjournal.BuildSemanticCheckpoint(events)
		}
	}

	var builder strings.Builder
	builder.WriteString("Latest user request (primary):\n")
	latestUser := latestRecoveryTurn(history, "user")
	if latestUser == "" {
		builder.WriteString("- No prior user turn was active; begin from the user's first message in this new thread.\n")
	} else {
		builder.WriteString("- ")
		builder.WriteString(latestUser)
		builder.WriteString("\n")
	}

	builder.WriteString("\nRecent thread context (primary, chronological):\n")
	recent := history
	if len(recent) > 8 {
		recent = recent[len(recent)-8:]
	}
	if len(recent) == 0 {
		builder.WriteString("- No prior turns were available.\n")
	}
	for _, turn := range recent {
		role := "User"
		if turn.Role == "assistant" {
			role = "Agent"
		}
		text := boundedRecoveryText(turn.Text, 1_200)
		if text != "" {
			fmt.Fprintf(&builder, "- %s: %s\n", role, text)
		}
	}

	if latestAgent := latestRecoveryTurn(history, "assistant"); latestAgent != "" {
		builder.WriteString("\nMost recent agent work state:\n- ")
		builder.WriteString(latestAgent)
		builder.WriteString("\n")
	}

	writeRecoverySection(&builder, "Failure and unresolved context (secondary)", append(checkpoint.FailedApproaches, checkpoint.OpenQuestions...), 6)
	other := append([]string{}, checkpoint.Decisions...)
	other = append(other, checkpoint.Corrections...)
	other = append(other, checkpoint.ToolFindings...)
	other = append(other, checkpoint.Artifacts...)
	other = append(other, checkpoint.WorkState...)
	writeRecoverySection(&builder, "Other durable checkpoint context (secondary)", other, 8)

	handoff := strings.TrimSpace(builder.String())
	if len(handoff) > recoveryHandoffLimit {
		handoff = handoff[:recoveryHandoffLimit]
	}
	return handoff
}

func latestRecoveryTurn(history []conversation.Turn, role string) string {
	for index := len(history) - 1; index >= 0; index-- {
		if history[index].Role == role {
			if text := boundedRecoveryText(history[index].Text, 1_600); text != "" {
				return text
			}
		}
	}
	return ""
}

func writeRecoverySection(builder *strings.Builder, title string, values []string, limit int) {
	if len(values) == 0 {
		return
	}
	if len(values) > limit {
		values = values[len(values)-limit:]
	}
	builder.WriteString("\n")
	builder.WriteString(title)
	builder.WriteString(":\n")
	for _, value := range values {
		if text := boundedRecoveryText(value, 700); text != "" {
			builder.WriteString("- ")
			builder.WriteString(text)
			builder.WriteString("\n")
		}
	}
}

func boundedRecoveryText(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, strings.TrimSpace(value))
	value = strings.Join(strings.Fields(value), " ")
	value = recoverySecretPatterns[0].ReplaceAllString(value, "Bearer [REDACTED]")
	value = recoverySecretPatterns[1].ReplaceAllString(value, "CREDENTIAL=[REDACTED]")
	value = recoverySecretPatterns[2].ReplaceAllString(value, "${1}[REDACTED]")
	runes := []rune(value)
	if len(runes) > limit {
		value = strings.TrimSpace(string(runes[:limit])) + "…"
	}
	return value
}
