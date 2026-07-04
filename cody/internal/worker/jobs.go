// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package worker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// autoBackgroundAfter is how long an exec command may block a step before it
// is promoted to a background job: the step returns immediately with the job
// id and the output so far, and the command keeps running (npm install,
// builds, test suites) instead of eating the step against the exec timeout.
// A package var so tests can compress time.
var autoBackgroundAfter = 60 * time.Second

// maxJobs bounds concurrently tracked background jobs.
const maxJobs = 20

// jobTailBytes is how much of the current output a promotion notice and a
// job_output poll return inline.
const jobTailBytes = 8 * 1024

// job is one auto-promoted background command.
type job struct {
	id      string
	cmd     *exec.Cmd
	outPath string
	done    chan struct{}

	mu       sync.Mutex
	exitNote string
	folded   bool // completion already folded into the worker's mutation seq
}

// finished reports whether the job's process has exited.
func (j *job) finished() bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

func (j *job) setExit(note string) {
	j.mu.Lock()
	j.exitNote = note
	j.mu.Unlock()
}

func (j *job) exit() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.exitNote
}

// foldFinishedJobs advances the mutation sequence once per completed job — a
// background command may have mutated the workspace right up to its exit, so
// verification that predates the completion is stale. Called at the
// single-threaded touch points (dispatch loop, turn-in).
func (w *Worker) foldFinishedJobs() {
	for _, j := range w.jobs {
		if !j.finished() {
			continue
		}
		j.mu.Lock()
		if !j.folded {
			j.folded = true
			w.mutSeq++
		}
		j.mu.Unlock()
	}
}

// runningJobs lists the ids of jobs still running.
func (w *Worker) runningJobs() []string {
	var out []string
	for id, j := range w.jobs {
		if !j.finished() {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// startJob launches cmdStr with its combined output streaming to a log file
// and returns the tracked job. The process is killed when the worker ends.
func (w *Worker) startJob(cmdStr string) (*job, error) {
	if len(w.jobs) >= maxJobs {
		return nil, fmt.Errorf("too many background jobs (%d); stop one with job_kill first", len(w.jobs))
	}
	dir := filepath.Join(w.opts.Root, ".cody", "jobs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	id := fmt.Sprintf("job-%d", time.Now().UnixNano())
	outPath := filepath.Join(dir, id+".log")
	f, err := os.Create(outPath)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("sh", "-c", cmdStr)
	cmd.Dir = w.opts.Root
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		f.Close()
		return nil, err
	}
	j := &job{id: id, cmd: cmd, outPath: outPath, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		f.Close()
		note := "exit 0"
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				note = fmt.Sprintf("exit %d", ee.ExitCode())
			} else {
				note = "error: " + err.Error()
			}
		}
		j.setExit(note)
		close(j.done)
	}()
	w.jobs[id] = j
	return j, nil
}

// jobTail reads the last jobTailBytes of a job's output file.
func jobTail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(data) > jobTailBytes {
		return "..." + string(data[len(data)-jobTailBytes:])
	}
	return string(data)
}

// toolJobOutput reports a background job's status and current output tail.
// The FULL output stays on disk at the job's log path, readable with fs_read.
func (w *Worker) toolJobOutput(args map[string]interface{}) string {
	id := str(args, "id")
	j, ok := w.jobs[id]
	if !ok {
		return "error: no background job " + id
	}
	w.foldFinishedJobs()
	state := "RUNNING"
	if j.finished() {
		state = "FINISHED (" + j.exit() + ")"
	}
	tail := jobTail(j.outPath)
	return fmt.Sprintf("[%s %s]\noutput so far (tail; full log at %s):\n%s", id, state, j.outPath, tail)
}

// toolJobKill terminates a background job.
func (w *Worker) toolJobKill(args map[string]interface{}) string {
	id := str(args, "id")
	j, ok := w.jobs[id]
	if !ok {
		return "error: no background job " + id
	}
	if !j.finished() {
		_ = j.cmd.Process.Kill()
		select {
		case <-j.done:
		case <-time.After(5 * time.Second):
		}
	}
	w.foldFinishedJobs()
	return fmt.Sprintf("%s killed (%s); full log at %s", id, j.exit(), j.outPath)
}

// stopAllJobs kills every still-running background job at worker end.
func (w *Worker) stopAllJobs() {
	for _, j := range w.jobs {
		if !j.finished() {
			_ = j.cmd.Process.Kill()
			select {
			case <-j.done:
			case <-time.After(2 * time.Second):
			}
		}
	}
}

// promotionNotice renders the step result for an exec that was auto-promoted.
func promotionNotice(j *job, elapsed time.Duration) string {
	tail := strings.TrimSpace(jobTail(j.outPath))
	var b strings.Builder
	fmt.Fprintf(&b, "[still running after %s — promoted to background job %s]\n", elapsed.Round(time.Second), j.id)
	b.WriteString("The command keeps running. Poll it with job_output, stop it with job_kill, ")
	b.WriteString("and continue other work meanwhile. Check job_output before relying on its result.\n")
	if tail != "" {
		fmt.Fprintf(&b, "output so far (tail):\n%s\n", tail)
	}
	return b.String()
}
