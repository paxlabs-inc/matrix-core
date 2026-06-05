// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_async_durable.go — durable inbox for async jobs (Leg A).
//
// The asyncRegistry is the daemon's intake ledger. Holding it only in
// memory meant an accepted message (HTTP 202 already returned) could
// silently evaporate on suspend, crash, or redeploy — there was no
// guaranteed receipt. This file persists every job to the Fly volume
// (under <data>/async, which is snapshotted to S3 and restored on fresh
// volumes), so:
//
//   - queued jobs are RESUMED on boot — once accepted, a message is
//     guaranteed to run to a terminal outcome (TCP-style delivery).
//   - terminal outcomes stay PULL-RETRIEVABLE across restarts via
//     GET /messages/async/:id, independent of any live SSE stream.
//   - jobs caught mid-flight by an unclean restart surface a
//     deterministic "interrupted" outcome (never silent, never a
//     blind re-run of possibly-side-effecting work).
//
// Persistence is best-effort: an IO error is logged but never fails the
// request path — the in-memory registry remains the live source of
// truth, with the files as its durable mirror.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// asyncJobDir derives the durable async-job directory from the daemon's
// persistent roots. It co-locates with the snapshotted data tree (the
// parent of cortex-root, e.g. /data in prod, so /data/async), falling
// back to the transcripts parent, then a local default. Returning "" is
// avoided so persistence is on by default wherever the daemon has a
// persistent root.
func asyncJobDir(cortexRoot, transcriptsDir string) string {
	switch {
	case cortexRoot != "":
		return filepath.Join(filepath.Dir(cortexRoot), "async")
	case transcriptsDir != "":
		return filepath.Join(filepath.Dir(transcriptsDir), "async")
	default:
		return ""
	}
}

// jobPathLocked returns the on-disk path for a job, or "" when
// persistence is disabled (no dir configured).
func (r *asyncRegistry) jobPathLocked(intentID string) string {
	if r.dir == "" || intentID == "" {
		return ""
	}
	return filepath.Join(r.dir, intentID+".json")
}

// persistLocked atomically mirrors a job to disk. Caller MUST hold r.mu.
// No-op when persistence is disabled. Errors are non-fatal.
func (r *asyncRegistry) persistLocked(job *asyncJob) {
	path := r.jobPathLocked(job.IntentID)
	if path == "" {
		return
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "async: mkdir %s: %v\n", r.dir, err)
		return
	}
	data, err := json.Marshal(job)
	if err != nil {
		fmt.Fprintf(os.Stderr, "async: marshal job %s: %v\n", job.IntentID, err)
		return
	}
	// Write to a temp file then rename so a reader never observes a
	// half-written job (atomic on the same filesystem).
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "async: write %s: %v\n", tmp, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		fmt.Fprintf(os.Stderr, "async: rename %s: %v\n", path, err)
		_ = os.Remove(tmp)
	}
}

// removeFileLocked deletes a job's persisted file (on eviction). Caller
// MUST hold r.mu. No-op when persistence is disabled.
func (r *asyncRegistry) removeFileLocked(intentID string) {
	if path := r.jobPathLocked(intentID); path != "" {
		_ = os.Remove(path)
	}
}

// loadFromDir rehydrates persisted jobs into the in-memory map. Called
// once from the constructor (single-threaded, no lock needed). Corrupt
// or unreadable files are skipped, never fatal.
func (r *asyncRegistry) loadFromDir() {
	if r.dir == "" {
		return
	}
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		// Missing dir is normal on first boot; created lazily on persist.
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "async: readdir %s: %v\n", r.dir, err)
		}
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(r.dir, e.Name())
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		var job asyncJob
		if jerr := json.Unmarshal(data, &job); jerr != nil || job.IntentID == "" {
			continue
		}
		r.jobs[job.IntentID] = &job
	}
}

// resumableJobs returns a snapshot of jobs that need post-boot handling:
// queued jobs to (re)dispatch and running jobs interrupted by an unclean
// restart. Terminal jobs are left untouched (kept for pull-retrieval).
func (r *asyncRegistry) resumableJobs() (queued, interrupted []*asyncJob) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, j := range r.jobs {
		switch j.Status {
		case asyncQueued:
			cp := *j
			cp.cancelFunc = nil
			queued = append(queued, &cp)
		case asyncRunning:
			cp := *j
			cp.cancelFunc = nil
			interrupted = append(interrupted, &cp)
		}
	}
	return queued, interrupted
}

// resumeAsyncJobs is invoked once after the server is listening. It
// completes the durable-inbox guarantee:
//
//   - queued jobs (accepted but never started) are dispatched, so an
//     accepted message always runs even if the daemon died before it
//     began.
//   - running jobs (interrupted mid-flight by a crash/redeploy) are
//     marked failed with a deterministic, user-facing "interrupted"
//     outcome rather than blindly re-run — re-running could double a
//     side effect (e.g. an on-chain write). The user retries explicitly.
func (d *daemonState) resumeAsyncJobs(t *transcript) {
	if d.asyncReg == nil {
		return
	}
	queued, interrupted := d.asyncReg.resumableJobs()

	for _, j := range interrupted {
		res := &messageResult{
			IntentID: j.IntentID,
			Status:   "failed",
			Error:    "interrupted by daemon restart before completion",
			Answer:   "This task was interrupted by a system restart before it finished, so it did not complete. No partial result is available — please send the request again.",
		}
		d.asyncReg.MarkResult(j.IntentID, res, nil, nil)
		if t != nil {
			t.Event("async.resume.interrupted", "boot", map[string]interface{}{
				"intent_id": j.IntentID,
			})
		}
	}

	for _, j := range queued {
		if t != nil {
			t.Event("async.resume.dispatch", "boot", map[string]interface{}{
				"intent_id": j.IntentID,
			})
		}
		go d.runAsyncMessage(j.IntentID, j.Request, t)
	}

	if t != nil && (len(queued) > 0 || len(interrupted) > 0) {
		t.Event("async.resume.summary", "boot", map[string]interface{}{
			"dispatched":  len(queued),
			"interrupted": len(interrupted),
		})
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
