// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package worker is Cody's fresh-context worker runtime: it receives exactly
// ONE task sheet plus a minimal grounding bundle, executes it with the full
// coding tool surface (fs, exec incl. supervised services, edit engine,
// verification runner), and returns a structured TurnInReport. The
// constitution is enforced STRUCTURALLY, not prompted: a done claim is
// refused unless the engine's own verification ran green after the last
// workspace mutation, truncated tool output must be read in full before the
// worker may turn in, and the sheet's do-not-touch set is enforced at
// mutation time. The worker dies after turn-in; its transcript is never the
// orchestrator's problem.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"matrix/cody/internal/contract"
	"matrix/cody/internal/edit"
	"matrix/cody/internal/llm"
	"matrix/cody/internal/policy"
	"matrix/cody/internal/verify"
)

// Options configures one worker run.
type Options struct {
	Sheet *contract.TaskSheet
	// Grounding is the minimal grounding bundle (workspace summary, seam
	// notes) — the ONLY context beyond the sheet itself.
	Grounding string
	// Root is the workspace root the worker operates in.
	Root string
	// Client is the LLM client (cody gateway slot).
	Client *llm.Client
	// MaxSteps bounds the tool loop; default 48.
	MaxSteps int
	// ExecTimeout bounds one exec tool call; default 5m.
	ExecTimeout time.Duration
	// VerifyTimeout bounds one verification command; default verify.DefaultTimeout.
	VerifyTimeout time.Duration
	// ExtraTools extends the tool surface (e.g. the MCP-backed browser,
	// fetch, and web-search bridges wired in by codyd).
	ExtraTools []llm.Tool
	// ExtraDispatch executes an ExtraTools call by function name.
	ExtraDispatch func(ctx context.Context, name string, args map[string]interface{}) (string, error)
	// Skills, when set, surfaces the skills library to the worker: the index
	// (names + one-line descriptions) rides the system prompt, bodies are
	// pulled on demand with the skill_load tool.
	Skills *policy.Skills
	// ScaffoldDir, when set, points at the scaffolder suite
	// (scaffold-<stack>.sh). Greenfield structure comes from a scaffolder run
	// via exec, never hand-written boilerplate.
	ScaffoldDir string
}

// execOutputCap is the inline byte cap for exec output before it spills to an
// overflow file and a read-full obligation is registered.
const execOutputCap = 24 * 1024

// fsReadCap is the max bytes one fs_read call returns.
const fsReadCap = 48 * 1024

// maxNudges bounds how many times the worker is reminded to turn in before
// the engine synthesizes an honest blocked report.
const maxNudges = 3

// Worker executes one task sheet.
type Worker struct {
	opts   Options
	edits  *edit.Engine
	runner *verify.Runner

	messages []llm.Message
	// changes is the ENGINE's record of workspace mutations (path -> kind);
	// the model's turn_in supplies the why, never the facts.
	changes   map[string]string
	changeWhy map[string]string
	// lastVerify holds the most recent verify_run results; verifySeq/mutSeq
	// order verification against mutations so stale green can never carry a
	// done claim.
	lastVerify []verify.Result
	verifySeq  int
	mutSeq     int
	// overflows tracks read-full obligations: overflow file -> read cursor.
	overflows map[string]*overflowState
	// services are supervised background processes, killed at run end.
	services map[string]*exec.Cmd
	// jobs are auto-promoted background commands (long execs), killed at run end.
	jobs map[string]*job
	// loops is the stuck-worker guard: hashed (tool, input, output) signatures
	// over a sliding window; a repeated identical call stops the run honestly
	// instead of burning the step budget.
	loops loopDetector
	// usage accumulates the worker's LLM token spend across the run.
	usage contract.Usage
}

type overflowState struct {
	size   int64
	cursor int64
}

// New builds a worker for one sheet.
func New(opts Options) (*Worker, error) {
	if opts.Sheet == nil {
		return nil, errors.New("worker: nil sheet")
	}
	if err := opts.Sheet.Validate(); err != nil {
		return nil, err
	}
	if opts.Client == nil {
		return nil, errors.New("worker: nil llm client")
	}
	eng, err := edit.New(opts.Root)
	if err != nil {
		return nil, err
	}
	runner, err := verify.NewRunner(opts.Root)
	if err != nil {
		return nil, err
	}
	if opts.VerifyTimeout > 0 {
		runner.Timeout = opts.VerifyTimeout
	}
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = 48
	}
	if opts.ExecTimeout <= 0 {
		opts.ExecTimeout = 5 * time.Minute
	}
	return &Worker{
		opts:      opts,
		edits:     eng,
		runner:    runner,
		changes:   map[string]string{},
		changeWhy: map[string]string{},
		overflows: map[string]*overflowState{},
		services:  map[string]*exec.Cmd{},
		jobs:      map[string]*job{},
	}, nil
}

// Run executes the sheet to a structured turn-in report. The context bounds
// the whole run. Run never returns a fabricated success: if the model stops
// cooperating the report is an honest blocked/partial.
func (w *Worker) Run(ctx context.Context) (*contract.TurnInReport, error) {
	defer w.stopAllServices()
	defer w.stopAllJobs()

	w.messages = []llm.Message{
		llm.SystemMessage(w.systemPrompt()),
		llm.UserMessage(w.userPrompt()),
	}
	tools := w.toolSchemas()
	nudges := 0

	for step := 0; step < w.opts.MaxSteps; step++ {
		w.foldFinishedJobs()
		res, err := w.opts.Client.Chat(ctx, llm.ChatRequest{Messages: w.messages, Tools: tools})
		if err != nil {
			return nil, fmt.Errorf("worker: llm turn failed: %w", err)
		}
		w.usage.PromptTokens += res.Usage.PromptTokens
		w.usage.CompletionTokens += res.Usage.CompletionTokens
		w.usage.TotalTokens += res.Usage.TotalTokens
		w.messages = append(w.messages, res.Message)

		if !res.HasToolCalls() {
			nudges++
			if nudges > maxNudges {
				break
			}
			w.messages = append(w.messages, llm.UserMessage(
				"You must finish by calling the turn_in tool with your honest status "+
					"(done|partial|blocked), never a plain message. If verification has "+
					"not run green since your last change, run verify_run first or report "+
					"partial/blocked with gaps."))
			continue
		}

		for _, tc := range res.Message.ToolCalls {
			if tc.Function.Name == "turn_in" {
				report, refusal := w.tryTurnIn(tc)
				if report != nil {
					return report, nil
				}
				w.messages = append(w.messages, llm.ToolResult(tc.ID, tc.Function.Name, refusal))
				if w.loops.observe(tc.Function.Name, tc.Function.Arguments, refusal) {
					return w.synthesizeReport(loopGap), nil
				}
				continue
			}
			out := w.dispatch(ctx, tc)
			w.messages = append(w.messages, llm.ToolResult(tc.ID, tc.Function.Name, out))
			if w.loops.observe(tc.Function.Name, tc.Function.Arguments, out) {
				return w.synthesizeReport(loopGap), nil
			}
		}
	}

	// Step budget or cooperation exhausted: honest engine-synthesized partial.
	return w.synthesizeReport("worker stopped before turn_in (step budget or non-cooperation)"), nil
}

// synthesizeReport builds an honest non-done report from engine state when
// the model failed to turn in.
func (w *Worker) synthesizeReport(gap string) *contract.TurnInReport {
	status := contract.StatusBlocked
	if len(w.changes) > 0 {
		status = contract.StatusPartial
	}
	r := &contract.TurnInReport{
		TaskID:       w.opts.Sheet.TaskID,
		Status:       status,
		Summary:      "engine-synthesized report: the worker did not turn in",
		Changes:      w.trackedChanges(),
		Verification: w.evidence(),
		Gaps:         []string{gap},
		Attempt:      w.opts.Sheet.Attempt,
		Usage:        w.usage,
	}
	return r
}

// tryTurnIn adjudicates a turn_in call against the engine's own state. It
// returns either a valid report or a refusal string fed back to the model.
func (w *Worker) tryTurnIn(tc llm.ToolCall) (*contract.TurnInReport, string) {
	var args struct {
		Status      string            `json:"status"`
		Summary     string            `json:"summary"`
		Changes     []contract.Change `json:"changes"`
		Gaps        []string          `json:"gaps"`
		Assumptions []string          `json:"assumptions"`
		Screenshots []string          `json:"screenshots"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return nil, "turn_in arguments are not valid JSON: " + err.Error()
	}
	status := contract.Status(args.Status)

	// Read-full discipline: outstanding truncated output blocks ANY turn-in.
	if pending := w.pendingOverflows(); len(pending) > 0 {
		return nil, "turn_in refused: truncated tool output has not been read in full. " +
			"Read these overflow files to the end with fs_read first: " + strings.Join(pending, ", ")
	}

	// Background jobs must be resolved before a done claim: a still-running
	// command's outcome is unknown, so 'done' over it would be a false success.
	w.foldFinishedJobs()
	if status == contract.StatusDone {
		if running := w.runningJobs(); len(running) > 0 {
			return nil, "turn_in refused: background jobs are still running: " + strings.Join(running, ", ") +
				". Wait for them with job_output (or stop them with job_kill) and check their results first."
		}
	}

	if status == contract.StatusDone {
		if len(w.lastVerify) == 0 {
			return nil, "turn_in refused: done claimed but verification never ran. Call verify_run first."
		}
		if w.verifySeq < w.mutSeq {
			return nil, "turn_in refused: the workspace changed after the last verification. Call verify_run again."
		}
		if !verify.AllGreen(w.lastVerify) {
			return nil, "turn_in refused: verification is red. Fix the failures and re-run verify_run, " +
				"or report partial/blocked with honest gaps."
		}
	}

	// The model supplies the why; the engine supplies the facts.
	for _, ch := range args.Changes {
		if ch.Why != "" {
			w.changeWhy[filepath.ToSlash(ch.Path)] = ch.Why
		}
	}
	report := &contract.TurnInReport{
		TaskID:       w.opts.Sheet.TaskID,
		Status:       status,
		Summary:      args.Summary,
		Changes:      w.trackedChanges(),
		Verification: append(w.evidence(), w.screenshotEvidence(args.Screenshots)...),
		Gaps:         args.Gaps,
		Assumptions:  args.Assumptions,
		Attempt:      w.opts.Sheet.Attempt,
		Usage:        w.usage,
	}
	if err := report.Validate(); err != nil {
		return nil, "turn_in refused: " + err.Error()
	}
	return report, ""
}

// screenshotEvidence turns claimed screenshot paths into evidence — but only
// for artifacts that REALLY exist under the workspace root (no fakes: an
// asserted screenshot that isn't on disk is not evidence). A green exit is
// recorded so the artifact reads as passing evidence in the turn-in.
func (w *Worker) screenshotEvidence(paths []string) []contract.Evidence {
	var out []contract.Evidence
	for _, p := range paths {
		rel := strings.TrimSpace(p)
		if rel == "" {
			continue
		}
		abs := rel
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(w.opts.Root, filepath.FromSlash(rel))
		}
		if info, err := os.Stat(abs); err != nil || info.IsDir() || info.Size() == 0 {
			continue // a screenshot that is not a real, non-empty file is not evidence
		}
		out = append(out, contract.Evidence{
			Command:    "screenshot",
			Exit:       0,
			Screenshot: filepath.ToSlash(rel),
		})
	}
	return out
}

// trackedChanges renders the engine's mutation record.
func (w *Worker) trackedChanges() []contract.Change {
	paths := make([]string, 0, len(w.changes))
	for p := range w.changes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]contract.Change, 0, len(paths))
	for _, p := range paths {
		out = append(out, contract.Change{Path: p, Kind: w.changes[p], Why: w.changeWhy[p]})
	}
	return out
}

// evidence converts the engine's own last verification into report evidence.
func (w *Worker) evidence() []contract.Evidence {
	out := make([]contract.Evidence, 0, len(w.lastVerify))
	for _, res := range w.lastVerify {
		excerpt := res.Output
		if len(excerpt) > 2048 {
			excerpt = excerpt[:2048] + "..."
		}
		out = append(out, contract.Evidence{
			Command:       res.Command.Cmd,
			Exit:          res.Exit,
			OutputExcerpt: excerpt,
			OverflowPath:  res.OverflowPath,
		})
	}
	return out
}

// pendingOverflows lists overflow files not yet read to the end.
func (w *Worker) pendingOverflows() []string {
	var out []string
	for p, st := range w.overflows {
		if st.cursor < st.size {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// --- tool dispatch ---

func (w *Worker) dispatch(ctx context.Context, tc llm.ToolCall) string {
	args := map[string]interface{}{}
	if tc.Function.Arguments != "" {
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return "error: tool arguments are not valid JSON: " + err.Error()
		}
	}
	switch tc.Function.Name {
	case "fs_read":
		return w.toolRead(args)
	case "fs_list":
		return w.toolList(args)
	case "grep":
		return w.toolGrep(args)
	case "glob":
		return w.toolGlob(args)
	case "fs_write":
		return w.toolWrite(args)
	case "fs_edit":
		return w.toolEdit(args)
	case "fs_delete":
		return w.toolDelete(args)
	case "exec":
		return w.toolExec(ctx, args)
	case "job_output":
		return w.toolJobOutput(args)
	case "job_kill":
		return w.toolJobKill(args)
	case "service_start":
		return w.toolServiceStart(args)
	case "service_stop":
		return w.toolServiceStop(args)
	case "skill_load":
		return w.toolSkillLoad(args)
	case "verify_run":
		return w.toolVerifyRun(ctx)
	default:
		if w.opts.ExtraDispatch != nil {
			for _, tool := range w.opts.ExtraTools {
				if tool.Function.Name == tc.Function.Name {
					out, err := w.opts.ExtraDispatch(ctx, tc.Function.Name, args)
					if err != nil {
						return "error: " + err.Error()
					}
					return out
				}
			}
		}
		return "error: unknown tool " + tc.Function.Name
	}
}

func str(args map[string]interface{}, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func num(args map[string]interface{}, key string) int64 {
	if v, ok := args[key].(float64); ok {
		return int64(v)
	}
	return 0
}

// guardTouch enforces the sheet's do-not-touch set at mutation time.
func (w *Worker) guardTouch(rel string) error {
	clean := filepath.ToSlash(filepath.Clean(rel))
	for _, pattern := range w.opts.Sheet.Deliverable.DoNotTouch {
		p := filepath.ToSlash(strings.TrimSpace(pattern))
		if p == "" {
			continue
		}
		if clean == p || strings.HasPrefix(clean, strings.TrimSuffix(p, "/")+"/") {
			return fmt.Errorf("path %q is in the sheet's do-not-touch set (%q)", rel, pattern)
		}
		if ok, _ := path.Match(p, clean); ok {
			return fmt.Errorf("path %q matches do-not-touch pattern %q", rel, pattern)
		}
	}
	return nil
}

func (w *Worker) recordChange(rel, kind string) {
	w.mutSeq++
	key := filepath.ToSlash(filepath.Clean(rel))
	// create followed by edit stays create; anything followed by delete is delete.
	if prev, ok := w.changes[key]; ok && prev == "create" && kind == "edit" {
		return
	}
	w.changes[key] = kind
}

func (w *Worker) toolRead(args map[string]interface{}) string {
	rel := str(args, "path")
	if rel == "" {
		return "error: path is required"
	}
	offset := num(args, "offset")
	abs := rel
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(w.opts.Root, rel)
	}
	data, err := w.edits.Read(rel)
	if err != nil {
		return "error: " + err.Error()
	}
	total := int64(len(data))
	if offset > total {
		offset = total
	}
	end := offset + fsReadCap
	if end > total {
		end = total
	}
	chunk := data[offset:end]

	// Advance a read-full obligation when this is a registered overflow file.
	if st, ok := w.overflows[filepath.ToSlash(abs)]; ok {
		if offset <= st.cursor && end > st.cursor {
			st.cursor = end
		}
	}
	body := numberLines(chunk, 1+strings.Count(data[:offset], "\n"))
	if end < total {
		return fmt.Sprintf("%s\n[fs_read: bytes %d-%d of %d; continue with offset=%d]", body, offset, end, total, end)
	}
	return body
}

// numberLines prefixes each line of a chunk with its 1-based line number in
// the file. The "N| " prefix is display metadata only — it is never part of
// the file content and must never appear in fs_edit anchors.
func numberLines(chunk string, startLine int) string {
	if chunk == "" {
		return chunk
	}
	lines := strings.Split(chunk, "\n")
	var b strings.Builder
	for i, line := range lines {
		if i == len(lines)-1 && line == "" {
			break // the chunk's trailing newline is not an extra line
		}
		fmt.Fprintf(&b, "%6d| %s\n", startLine+i, line)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func (w *Worker) toolList(args map[string]interface{}) string {
	rel := str(args, "path")
	if rel == "" {
		rel = "."
	}
	abs := filepath.Join(w.opts.Root, rel)
	entries, err := os.ReadDir(abs)
	if err != nil {
		return "error: " + err.Error()
	}
	var b strings.Builder
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		b.WriteString(name + "\n")
	}
	return b.String()
}

func (w *Worker) toolWrite(args map[string]interface{}) string {
	rel := str(args, "path")
	content := str(args, "content")
	if rel == "" {
		return "error: path is required"
	}
	if err := w.guardTouch(rel); err != nil {
		return "error: " + err.Error()
	}
	abs := filepath.Join(w.opts.Root, rel)
	if _, err := os.Stat(abs); os.IsNotExist(err) {
		if err := w.edits.Create(rel, content); err != nil {
			return "error: " + err.Error()
		}
		w.recordChange(rel, "create")
		return "created " + rel + w.diagnose(rel)
	}
	if err := w.edits.Overwrite(rel, content); err != nil {
		return "error: " + err.Error()
	}
	w.recordChange(rel, "edit")
	return "overwrote " + rel + w.diagnose(rel)
}

func (w *Worker) toolEdit(args map[string]interface{}) string {
	rel := str(args, "path")
	oldText := str(args, "old_text")
	newText := str(args, "new_text")
	replaceAll, _ := args["replace_all"].(bool)
	if rel == "" {
		return "error: path is required"
	}
	if err := w.guardTouch(rel); err != nil {
		return "error: " + err.Error()
	}
	if err := w.edits.Apply(rel, oldText, newText, replaceAll); err != nil {
		return "error: " + err.Error()
	}
	w.recordChange(rel, "edit")
	return "edited " + rel + w.diagnose(rel)
}

func (w *Worker) toolDelete(args map[string]interface{}) string {
	rel := str(args, "path")
	if rel == "" {
		return "error: path is required"
	}
	if err := w.guardTouch(rel); err != nil {
		return "error: " + err.Error()
	}
	if err := w.edits.Delete(rel); err != nil {
		return "error: " + err.Error()
	}
	w.recordChange(rel, "delete")
	return "deleted " + rel
}

// toolExec runs a shell command. A command that finishes within
// autoBackgroundAfter returns inline (overflow-spilled when oversized); a
// longer one is auto-promoted to a background job so the step returns
// immediately and the worker keeps moving (poll with job_output, stop with
// job_kill). An explicit timeout_secs at or under the promotion threshold is
// honored as a hard kill (the legacy bounded-exec contract).
func (w *Worker) toolExec(ctx context.Context, args map[string]interface{}) string {
	cmdStr := str(args, "cmd")
	if cmdStr == "" {
		return "error: cmd is required"
	}
	timeout := w.opts.ExecTimeout
	explicit := false
	if t := num(args, "timeout_secs"); t > 0 {
		timeout = time.Duration(t) * time.Second
		explicit = true
	}
	j, err := w.startJob(cmdStr)
	if err != nil {
		return "error: " + err.Error()
	}
	// Any exec may mutate the workspace; verification after it is stale.
	w.mutSeq++

	hardKill := explicit && timeout <= autoBackgroundAfter
	wait := autoBackgroundAfter
	if hardKill {
		wait = timeout
	}
	start := time.Now()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-j.done:
		return w.finishInlineExec(j, "")
	case <-timer.C:
		if hardKill {
			_ = j.cmd.Process.Kill()
			<-j.done
			return w.finishInlineExec(j, " (timed out)")
		}
		return promotionNotice(j, time.Since(start))
	case <-ctx.Done():
		_ = j.cmd.Process.Kill()
		<-j.done
		w.reapJob(j)
		return "error: " + ctx.Err().Error()
	}
}

// finishInlineExec renders a completed (non-promoted) exec's result exactly
// like the classic bounded exec: inline output with the overflow-spill +
// read-full obligation for oversized output. The job's log file on disk IS
// the overflow file.
func (w *Worker) finishInlineExec(j *job, timeoutNote string) string {
	w.reapJob(j)
	out, _ := os.ReadFile(j.outPath)
	body := string(out)
	if len(out) > execOutputCap {
		w.overflows[filepath.ToSlash(j.outPath)] = &overflowState{size: int64(len(out))}
		body = string(out[:execOutputCap]) + fmt.Sprintf(
			"\n[output truncated: %d of %d bytes shown; the FULL output is at %s — you MUST read it in full with fs_read before turning in]",
			execOutputCap, len(out), j.outPath)
	}
	return fmt.Sprintf("[%s%s]\n%s", j.exit(), timeoutNote, body)
}

// reapJob removes a finished job from tracking, marking its completion as
// already folded into the mutation sequence (toolExec bumped it at start).
func (w *Worker) reapJob(j *job) {
	j.mu.Lock()
	j.folded = true
	j.mu.Unlock()
	delete(w.jobs, j.id)
}

func (w *Worker) toolServiceStart(args map[string]interface{}) string {
	name := str(args, "name")
	cmdStr := str(args, "cmd")
	if name == "" || cmdStr == "" {
		return "error: name and cmd are required"
	}
	if _, ok := w.services[name]; ok {
		return "error: service " + name + " is already running"
	}
	logDir := filepath.Join(w.opts.Root, ".cody", "services")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return "error: " + err.Error()
	}
	logFile, err := os.Create(filepath.Join(logDir, name+".log"))
	if err != nil {
		return "error: " + err.Error()
	}
	cmd := exec.Command("sh", "-c", cmdStr)
	cmd.Dir = w.opts.Root
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return "error: " + err.Error()
	}
	w.services[name] = cmd
	return fmt.Sprintf("service %s started (pid %d); logs at %s", name, cmd.Process.Pid, logFile.Name())
}

func (w *Worker) toolServiceStop(args map[string]interface{}) string {
	name := str(args, "name")
	cmd, ok := w.services[name]
	if !ok {
		return "error: no running service named " + name
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	delete(w.services, name)
	return "service " + name + " stopped"
}

func (w *Worker) stopAllServices() {
	for name, cmd := range w.services {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		delete(w.services, name)
	}
}

// toolSkillLoad returns a skill playbook's full body on demand (the index of
// names + descriptions rides the system prompt; bodies are never resident).
func (w *Worker) toolSkillLoad(args map[string]interface{}) string {
	if w.opts.Skills == nil || len(w.opts.Skills.Names) == 0 {
		return "error: no skills library is configured"
	}
	name := str(args, "name")
	if name == "" {
		return "error: name is required"
	}
	body, err := w.opts.Skills.Load(name)
	if err != nil {
		return "error: " + err.Error()
	}
	if len(body) > fsReadCap {
		body = body[:fsReadCap] + "\n[skill body truncated]"
	}
	return body
}

func (w *Worker) toolVerifyRun(ctx context.Context) string {
	cmds := verify.FromStrings(w.opts.Sheet.Verify.Commands)
	results, err := w.runner.Run(ctx, cmds)
	if err != nil {
		return "error: " + err.Error()
	}
	w.lastVerify = results
	w.verifySeq = w.mutSeq
	var b strings.Builder
	for _, res := range results {
		state := "GREEN"
		if !res.Green {
			state = "RED"
		}
		fmt.Fprintf(&b, "[%s] %s (exit %d)\n%s\n", state, res.Command.Cmd, res.Exit, res.Output)
	}
	if verify.AllGreen(results) {
		b.WriteString("verification: ALL GREEN\n")
	} else {
		b.WriteString("verification: RED — fix and re-run, or turn in an honest partial\n")
	}
	return b.String()
}

// --- prompts + schemas ---

// defaultConstitution is the worker-side rendering of the engine-enforced
// invariants. The prompt states them; the ENGINE enforces them.
var defaultConstitution = []string{
	"NO FAKES: never introduce stub/mock/fake doubles or placeholder implementations to make verification pass.",
	"VERIFY BEFORE DONE: 'done' requires the sheet's verification commands green after your last change (the engine enforces this).",
	"READ FULL: truncated tool output must be read to the end before you reason over it or turn in (the engine enforces this).",
	"NO FALSE SUCCESS: report partials as partials with honest gaps; never dress up incomplete work.",
	"COMPLETE ARTIFACTS: deliver runnable code with its dependencies, never fragments.",
	"USER DRIVES GIT: never run git commit/push; stage and propose only if asked by the sheet.",
	"RESPECT THE PROJECT: follow existing repo style; no drive-by comments, docs, or refactors outside the sheet's scope; never weaken or delete tests to pass.",
}

func (w *Worker) systemPrompt() string {
	s := w.opts.Sheet
	var b strings.Builder
	b.WriteString("You are a Cody worker: a fresh-context coding agent executing exactly ONE task sheet in the user's workspace. ")
	b.WriteString("You have no conversation history and need none — the sheet is self-contained.\n\n")
	b.WriteString("Constitution (inviolable, engine-enforced):\n")
	clauses := s.Constraints.Constitution
	if len(clauses) == 0 {
		clauses = defaultConstitution
	}
	for _, c := range clauses {
		b.WriteString("- " + c + "\n")
	}
	if s.Constraints.ModePolicy != "" {
		b.WriteString("\nMode policy: " + s.Constraints.ModePolicy + "\n")
	}
	if s.Constraints.DesignLanguage != "" {
		b.WriteString("\nDESIGN LANGUAGE (binding — build EXACTLY to this; the gate rejects drift back to AI-default patterns):\n")
		b.WriteString(s.Constraints.DesignLanguage + "\n")
		b.WriteString("After building UI, capture a screenshot of the rendered result and record it as turn-in evidence (the gate rejects a UI turn-in without one).\n")
	} else if s.UITask {
		b.WriteString("\nThis is a UI task: after building, capture a screenshot of the rendered result and record it as turn-in evidence (the gate rejects a UI turn-in without one).\n")
	}
	if len(s.Constraints.RulesRefs) > 0 {
		b.WriteString("\nApplicable standards (read them with fs_read if you need the details):\n")
		for _, r := range s.Constraints.RulesRefs {
			b.WriteString("- " + r + "\n")
		}
	}
	if w.opts.Skills != nil && len(w.opts.Skills.Names) > 0 {
		b.WriteString("\n" + w.opts.Skills.RenderIndex())
		b.WriteString("When the task matches a skill, load its full playbook with skill_load and follow it.\n")
	}
	if stacks := ScaffoldStacks(w.opts.ScaffoldDir); len(stacks) > 0 {
		b.WriteString("\nScaffolder suite (greenfield projects): when the task creates a NEW project/app, ")
		b.WriteString("run the matching scaffolder via exec instead of hand-writing boilerplate:\n")
		fmt.Fprintf(&b, "  bash %s/scaffold-<stack>.sh <project-name> <parent-dir> --no-git [--pm pnpm] [--force]\n", w.opts.ScaffoldDir)
		b.WriteString("  stacks: " + strings.Join(stacks, ", ") + "\n")
		b.WriteString("Always pass --no-git (the workspace already tracks changes). The scaffolder creates <parent-dir>/<slug> with production structure (CI, Dockerfile, docs, lint/test config).\n")
	}
	b.WriteString("\nWork loop: orient with grep/glob; read before you write (fs_read shows 'N| ' line-number ")
	b.WriteString("prefixes — they are display metadata, NEVER part of the file: never include them in fs_edit ")
	b.WriteString("anchors or fs_write content); make anchored edits; run verify_run after changes; ")
	b.WriteString("finish with the turn_in tool carrying your honest status. A done claim is refused unless ")
	b.WriteString("verification ran green after your last mutation.")
	return b.String()
}

func (w *Worker) userPrompt() string {
	s := w.opts.Sheet
	var b strings.Builder
	fmt.Fprintf(&b, "TASK SHEET %s — %s (attempt %d)\n\n", s.TaskID, s.Title, s.Attempt)
	fmt.Fprintf(&b, "GOAL (what done means):\n%s\n\n", s.Goal)
	b.WriteString("ACCEPTANCE CRITERIA:\n")
	for i, a := range s.Acceptance {
		fmt.Fprintf(&b, "%d. %s\n", i+1, a)
	}
	if len(s.Grounding.Files) > 0 || len(s.Grounding.LineRefs) > 0 || s.Grounding.Notes != "" {
		b.WriteString("\nGROUNDING (read these before writing):\n")
		for _, f := range s.Grounding.Files {
			b.WriteString("- file: " + f + "\n")
		}
		for _, lr := range s.Grounding.LineRefs {
			b.WriteString("- ref: " + lr + "\n")
		}
		if s.Grounding.Notes != "" {
			b.WriteString("- notes: " + s.Grounding.Notes + "\n")
		}
	}
	b.WriteString("\nVERIFICATION (must be green for done):\n")
	for _, c := range s.Verify.Commands {
		b.WriteString("- " + c + "\n")
	}
	fmt.Fprintf(&b, "\nDELIVERABLE SHAPE:\n%s\n", s.Deliverable.Shape)
	if len(s.Deliverable.DoNotTouch) > 0 {
		b.WriteString("\nDO NOT TOUCH (enforced):\n")
		for _, d := range s.Deliverable.DoNotTouch {
			b.WriteString("- " + d + "\n")
		}
	}
	if len(s.Steers) > 0 {
		b.WriteString("\nUSER DIRECTION (mid-run guidance you MUST honor — it overrides earlier assumptions):\n")
		for _, d := range s.Steers {
			b.WriteString("- " + d + "\n")
		}
	}
	if s.Feedback != "" {
		fmt.Fprintf(&b, "\nREJECTION FEEDBACK FROM THE PREVIOUS ATTEMPT (address all of it):\n%s\n", s.Feedback)
	}
	if w.opts.Grounding != "" {
		fmt.Fprintf(&b, "\nWORKSPACE CONTEXT:\n%s\n", w.opts.Grounding)
	}
	return b.String()
}

func (w *Worker) toolSchemas() []llm.Tool {
	obj := func(props map[string]interface{}, required ...string) map[string]interface{} {
		schema := map[string]interface{}{"type": "object", "properties": props}
		if len(required) > 0 {
			schema["required"] = required
		}
		return schema
	}
	strProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	tools := []llm.Tool{
		llm.NewFunctionTool("fs_read", "Read a workspace file (records the freshness anchor for later edits). Output is line-numbered ('N| ' prefixes are display metadata — never include them in fs_edit anchors). Large files return a chunk plus the offset to continue from.",
			obj(map[string]interface{}{
				"path":   strProp("workspace-relative path"),
				"offset": map[string]interface{}{"type": "integer", "description": "byte offset to continue a long read"},
			}, "path")),
		llm.NewFunctionTool("fs_list", "List a workspace directory.",
			obj(map[string]interface{}{"path": strProp("workspace-relative directory (default .)")})),
		llm.NewFunctionTool("grep", "Search file contents across the workspace with a regular expression. Returns path:line: text matches (capped at 100). Far cheaper than exec grep or reading files whole.",
			obj(map[string]interface{}{
				"pattern": strProp("Go regular expression to search for"),
				"path":    strProp("workspace-relative directory to search under (default the whole workspace)"),
				"include": strProp("glob filter on file paths, e.g. *.go or src/**/*.ts"),
			}, "pattern")),
		llm.NewFunctionTool("glob", "Find files by path pattern (supports ** for directories), newest first, capped at 100. Use to orient before reading.",
			obj(map[string]interface{}{
				"pattern": strProp("glob over workspace-relative paths, e.g. **/*.tsx or cmd/**/main.go"),
			}, "pattern")),
		llm.NewFunctionTool("fs_write", "Create a new file, or fully overwrite a file you have READ this session. Overwrites of unread or drifted files are refused.",
			obj(map[string]interface{}{
				"path":    strProp("workspace-relative path"),
				"content": strProp("full file content"),
			}, "path", "content")),
		llm.NewFunctionTool("fs_edit", "Anchored find/replace in a file you have READ. Fails on a missing/ambiguous anchor or if the file drifted since the read.",
			obj(map[string]interface{}{
				"path":        strProp("workspace-relative path"),
				"old_text":    strProp("exact text to replace (must occur exactly once unless replace_all)"),
				"new_text":    strProp("replacement text"),
				"replace_all": map[string]interface{}{"type": "boolean", "description": "replace every occurrence"},
			}, "path", "old_text", "new_text")),
		llm.NewFunctionTool("fs_delete", "Delete a file you have READ (fresh).",
			obj(map[string]interface{}{"path": strProp("workspace-relative path")}, "path")),
		llm.NewFunctionTool("exec", "Run a shell command in the workspace root. Oversized output spills to an overflow file you MUST read in full before turning in. A command still running after 60s is promoted to a background job (poll with job_output, stop with job_kill) so you keep working meanwhile.",
			obj(map[string]interface{}{
				"cmd":          strProp("shell command"),
				"timeout_secs": map[string]interface{}{"type": "integer", "description": "hard timeout in seconds (only enforced as a kill when 60 or less; longer commands background instead)"},
			}, "cmd")),
		llm.NewFunctionTool("job_output", "Check a background job: running/finished state plus the tail of its output. A done claim is refused while jobs are still running.",
			obj(map[string]interface{}{"id": strProp("job id from the promotion notice")}, "id")),
		llm.NewFunctionTool("job_kill", "Stop a background job.",
			obj(map[string]interface{}{"id": strProp("job id from the promotion notice")}, "id")),
		llm.NewFunctionTool("service_start", "Start a supervised background service (e.g. a dev server). It keeps running until service_stop or the end of the task.",
			obj(map[string]interface{}{
				"name": strProp("service name"),
				"cmd":  strProp("shell command to run"),
			}, "name", "cmd")),
		llm.NewFunctionTool("service_stop", "Stop a supervised background service.",
			obj(map[string]interface{}{"name": strProp("service name")}, "name")),
		llm.NewFunctionTool("verify_run", "Run the sheet's verification commands and record the results as your turn-in evidence.",
			obj(map[string]interface{}{})),
		llm.NewFunctionTool("turn_in", "Finish the task with your honest status. done requires green verification after your last change (engine-enforced).",
			obj(map[string]interface{}{
				"status":  strProp("done | partial | blocked"),
				"summary": strProp("one-paragraph summary of what you did"),
				"changes": map[string]interface{}{
					"type": "array", "description": "why each file changed",
					"items": obj(map[string]interface{}{
						"path": strProp("file path"),
						"kind": strProp("create | edit | delete"),
						"why":  strProp("why this change"),
					}),
				},
				"gaps":        map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "honest gaps (required for partial/blocked)"},
				"assumptions": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
				"screenshots": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "workspace-relative paths to screenshot artifacts of the rendered UI (REQUIRED for a UI task; each must be a real file you captured)"},
			}, "status", "summary")),
	}
	if w.opts.Skills != nil && len(w.opts.Skills.Names) > 0 {
		tools = append(tools, llm.NewFunctionTool("skill_load",
			"Load a skill playbook's full body by name (the index of available skills is in your system prompt).",
			obj(map[string]interface{}{"name": strProp("skill name from the index")}, "name")))
	}
	return append(tools, w.opts.ExtraTools...)
}
