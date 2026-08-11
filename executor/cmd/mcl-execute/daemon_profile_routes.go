// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

// daemon_profile_routes.go implements GET /profile and PUT /profile on the
// per-user daemon. The onboarding profile (preferred_name, agent_name,
// expertise_domains) is stored as a pinned cortex Identity record tagged
// "onboarding-profile", not as an unsupervised learned fact.
//
// Absent profile → clean fallback to daemon defaults (agent_name from
// config, empty preferred_name/expertise). The prompt wiring that
// consumes these fields lives in neo/internal/agent/prompt.go (task 2.2).

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"matrix/cortex"
	"matrix/cortex/memory"
)

// profileTag is the cortex Head tag that marks the onboarding-profile
// Identity record. Identity memories are always pinned (tierPinned
// includes every TypeIdentity), so the profile is never salience-evicted.
const profileTag memory.Tag = "onboarding-profile"

// profileResponse is the JSON shape returned by GET /profile.
type profileResponse struct {
	PreferredName    string   `json:"preferred_name"`
	AgentName        string   `json:"agent_name"`
	ExpertiseDomains []string `json:"expertise_domains"`
	URI              string   `json:"uri,omitempty"`
}

// profileRequest is the JSON body for PUT /profile.
type profileRequest struct {
	PreferredName    string   `json:"preferred_name"`
	AgentName        string   `json:"agent_name"`
	ExpertiseDomains []string `json:"expertise_domains"`
}

// handleProfile serves GET /profile and PUT /profile.
func (d *daemonState) handleProfile(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		d.getProfile(w, r)
	case http.MethodPut:
		d.putProfile(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (d *daemonState) getProfile(w http.ResponseWriter, r *http.Request) {
	if d.infra == nil || d.infra.cortex == nil {
		writeJSON(w, http.StatusOK, d.defaultProfileResponse())
		return
	}

	mem, err := d.findProfileMemory()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "profile lookup: " + err.Error()})
		return
	}
	if mem == nil {
		writeJSON(w, http.StatusOK, d.defaultProfileResponse())
		return
	}

	data, err := memory.DecodeData(mem.Version.Type, mem.Version.Data)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "profile decode: " + err.Error()})
		return
	}
	idData, ok := data.(memory.IdentityData)
	if !ok {
		writeJSON(w, http.StatusOK, d.defaultProfileResponse())
		return
	}

	resp := profileResponse{
		PreferredName:    idData.Name,
		AgentName:        idData.AgentName,
		ExpertiseDomains: idData.ExpertiseDomains,
		URI:              string(cortex.BuildURI(memory.TypeIdentity, mem.Head.ID, mem.Head.CurrentVersion)),
	}
	if resp.AgentName == "" {
		resp.AgentName = d.defaultAgentName()
	}
	if resp.ExpertiseDomains == nil {
		resp.ExpertiseDomains = []string{}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (d *daemonState) putProfile(w http.ResponseWriter, r *http.Request) {
	if d.infra == nil || d.infra.cortex == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cortex not enabled"})
		return
	}

	var req profileRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	req.PreferredName = strings.TrimSpace(req.PreferredName)
	req.AgentName = strings.TrimSpace(req.AgentName)

	domains, verr := validateProfile(req.PreferredName, req.AgentName, req.ExpertiseDomains)
	if verr != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": verr})
		return
	}
	req.ExpertiseDomains = domains

	data := memory.IdentityData{
		SchemaVersion:    1,
		Name:             req.PreferredName,
		AgentName:        req.AgentName,
		ExpertiseDomains: domains,
	}

	actorScope := ""
	if d.actor != nil {
		actorScope = d.actor.UserURI
	}
	createdBy := "matrix://onboarding"
	if d.actor != nil {
		createdBy = d.actor.UserURI
	}

	existing, err := d.findProfileMemory()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "profile lookup: " + err.Error()})
		return
	}

	if existing != nil {
		uri := cortex.BuildURI(memory.TypeIdentity, existing.Head.ID, existing.Head.CurrentVersion)
		meta := cortex.WriteMeta{
			CreatedBy:  createdBy,
			Confidence: 1.0,
			Provenance: memory.Provenance{Source: memory.SourceUserInput},
		}
		newURI, err := d.infra.cortex.Update(uri, data, meta)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "profile update: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, profileResponse{
			PreferredName:    req.PreferredName,
			AgentName:        req.AgentName,
			ExpertiseDomains: domains,
			URI:              string(newURI),
		})
		return
	}

	head := memory.Head{
		ActorScope:         actorScope,
		Visibility:         memory.VisPrivate,
		DeclaredImportance: 10, // max valid (cortex caps at 10); Identity is pinned by type regardless
		Tags:               []memory.Tag{profileTag},
	}
	meta := cortex.WriteMeta{
		CreatedBy:  createdBy,
		Confidence: 1.0,
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	}
	uri, err := d.infra.cortex.Write(head, data, meta)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "profile write: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, profileResponse{
		PreferredName:    req.PreferredName,
		AgentName:        req.AgentName,
		ExpertiseDomains: domains,
		URI:              string(uri),
	})
}

// findProfileMemory scans Identity-type memories for the one tagged
// "onboarding-profile". Returns nil (no error) when no profile exists.
func (d *daemonState) findProfileMemory() (*memory.Memory, error) {
	if d.infra == nil || d.infra.cortex == nil {
		return nil, nil
	}
	ids, err := d.infra.cortex.ListByType(memory.TypeIdentity, 0)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		mem, err := d.infra.cortex.ResolveLatest(id)
		if err != nil {
			if errors.Is(err, memory.ErrNotFound) {
				continue
			}
			return nil, err
		}
		if mem.Head.Tombstoned != nil {
			continue
		}
		for _, t := range mem.Head.Tags {
			if t == profileTag {
				return mem, nil
			}
		}
	}
	return nil, nil
}

// defaultProfileResponse returns the fallback profile when no cortex
// profile record exists.
func (d *daemonState) defaultProfileResponse() profileResponse {
	return profileResponse{
		PreferredName:    "",
		AgentName:        d.defaultAgentName(),
		ExpertiseDomains: []string{},
	}
}

// defaultAgentName returns the daemon's configured agent name, or "Neo"
// as the ultimate fallback.
func (d *daemonState) defaultAgentName() string {
	if d.actor != nil && d.actor.AgentURI != "" {
		parts := strings.Split(d.actor.AgentURI, "/")
		if len(parts) > 0 {
			return parts[len(parts)-1]
		}
	}
	return "Neo"
}

// Profile input bounds (req 2.3): keep the pinned Identity record — which
// rides Neo's STABLE system-prompt prefix — small and well-formed.
const (
	maxPreferredNameLen = 100
	maxAgentNameLen     = 60
	maxExpertiseItems   = 24
	maxExpertiseItemLen = 64
)

// validateProfile enforces the input bounds and returns the normalized
// expertise list (trimmed, empties dropped, case-insensitively deduped,
// order-preserving). A non-empty second return value is a plain-language
// validation error; callers MUST reject with 400 when it is set.
func validateProfile(preferredName, agentName string, rawDomains []string) ([]string, string) {
	if len([]rune(preferredName)) > maxPreferredNameLen {
		return nil, fmt.Sprintf("preferred_name too long (max %d characters)", maxPreferredNameLen)
	}
	if len([]rune(agentName)) > maxAgentNameLen {
		return nil, fmt.Sprintf("agent_name too long (max %d characters)", maxAgentNameLen)
	}
	out := make([]string, 0, len(rawDomains))
	seen := make(map[string]struct{}, len(rawDomains))
	for _, d := range rawDomains {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if len([]rune(d)) > maxExpertiseItemLen {
			return nil, fmt.Sprintf("expertise item too long (max %d characters): %q", maxExpertiseItemLen, d)
		}
		key := strings.ToLower(d)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, d)
		if len(out) > maxExpertiseItems {
			return nil, fmt.Sprintf("too many expertise domains (max %d)", maxExpertiseItems)
		}
	}
	return out, ""
}
