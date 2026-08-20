// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"centra/agents/neo/internal/preview"
)

// Preview (NEO-WORKBENCH req 7): dev servers and unsafe execution run in
// on-demand Railway sandboxes, never on the main per-user VM. The controller
// lives in agents/neo/internal/preview; this file is the engine seam — the durable
// event emitter and the POST /workspace/preview route.

// publishPreview emits a preview.* event onto the conversation's run topic so
// it reaches live subscribers AND (being whitelisted in traceWorkspaceTypes)
// the durable trace, so reopen rebuilds the last preview state honestly.
func (e *Engine) publishPreview(convID, typ string, fields map[string]interface{}) {
	runID := e.activeRunForConv(convID)
	if runID == "" {
		runID = e.latestRunForConv(convID)
	}
	if runID == "" {
		// No run has ever existed for this conversation: publish on the
		// conversation id itself so at least live subscribers can follow.
		runID = convID
	}
	if fields == nil {
		fields = map[string]interface{}{}
	}
	fields["intent_id"] = runID
	fields["conversation_id"] = convID
	e.broker.ensure(runID)
	e.broker.publish(runID, typ, "neo", fields)
}

// agentPreview is the tools.PreviewFunc seam: it lets the agent itself launch
// the workbench preview for its conversation's active project (the same
// lifecycle as POST /workspace/preview, resolved from the conversation's
// project tag instead of a request header). Provisioning is kicked in the
// background — the tool result is a status line and the user follows the
// preview.* events in the Preview pane.
func (e *Engine) agentPreview(ctx context.Context) (string, error) {
	if e.preview == nil || !e.preview.Enabled() {
		return "", fmt.Errorf("sandbox previews are not configured in this environment")
	}
	r := runFromContext(ctx)
	if r == nil {
		return "", fmt.Errorf("no active run on context")
	}
	p, err := e.resolveProjectRecord(e.conv.Project(r.convID))
	if err != nil {
		return "", err
	}
	go func() {
		_, _ = e.preview.Provision(context.Background(), preview.Request{ConvID: r.convID, Root: p.Root})
	}()
	return fmt.Sprintf("Preview of %q is starting — it will appear in the workbench Preview pane when ready (or report a failure there).", p.Name), nil
}

// StartPreviewReaper runs the idle-sandbox reaper until ctx ends (no-op when
// preview is not configured).
func (e *Engine) StartPreviewReaper(ctx context.Context) {
	if e.preview != nil {
		go e.preview.StartReaper(ctx)
	}
}

// handleWorkspacePreview is the single-action (re)preview: POST it to
// provision the project's on-demand sandbox. It returns 202 and the lifecycle
// unfolds on preview.pending/ready/failed/expired events; the client renders
// those. A run need not be active — the user can ask for a preview of the
// current project state at any time.
func (s *Server) handleWorkspacePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.engine.preview == nil || !s.engine.preview.Enabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error": "preview is not configured in this environment",
		})
		return
	}
	var req struct {
		ConversationID string `json:"conversation_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	convID := strings.TrimSpace(req.ConversationID)
	if convID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "conversation_id is required"})
		return
	}
	root, err := s.engine.reqProjectRoot(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Provision in the background so the POST returns immediately; the client
	// follows preview.* on the conversation's live topic.
	go func() {
		_, _ = s.engine.preview.Provision(context.Background(), preview.Request{ConvID: convID, Root: root})
	}()
	writeJSON(w, http.StatusAccepted, map[string]interface{}{"preview": "provisioning"})
}
