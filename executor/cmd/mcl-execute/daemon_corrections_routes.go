// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_corrections_routes.go — /classify route (sess#27).
//
// Route:
//
//   POST /classify          run D9 §18.1 materiality classifier on
//                           (original_intent, original_plan,
//                            new_intent, new_plan, original_anchor,
//                            new_anchor) and return {material, reasons[]}
//
// The intent-scoped POST /intents/:id/correct lives in
// daemon_intents_routes.go since it threads through the lifecycle
// driver + envelope chain.

import (
	"encoding/json"
	"net/http"

	"matrix/executor/materiality"
	"matrix/mcl/ir"
)

// classifyRequest is the wire-form body for POST /classify.
type classifyRequest struct {
	OriginalIntent json.RawMessage `json:"original_intent"`
	OriginalPlan   json.RawMessage `json:"original_plan"`
	NewIntent      json.RawMessage `json:"new_intent"`
	NewPlan        json.RawMessage `json:"new_plan"`
	OriginalAnchor bool            `json:"original_anchor,omitempty"`
	NewAnchor      bool            `json:"new_anchor,omitempty"`
}

// classifyResponse mirrors materiality.Classification for the wire.
type classifyResponse struct {
	Material bool                 `json:"material"`
	Reasons  []materiality.Reason `json:"reasons,omitempty"`
}

// handleClassify serves POST /classify.
//
// Each of original_intent / new_intent / original_plan / new_plan is
// optional — the §18.1 rules degrade gracefully when one side of the
// comparison is absent. At least ONE intent OR plan body is required;
// an empty request returns 400.
func (d *daemonState) handleClassify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	var req classifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "decode body: " + err.Error(),
		})
		return
	}
	if len(req.OriginalIntent) == 0 && len(req.NewIntent) == 0 &&
		len(req.OriginalPlan) == 0 && len(req.NewPlan) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "at least one of original_intent/original_plan/new_intent/new_plan is required",
		})
		return
	}

	in := materiality.Inputs{
		OriginalAnchor: req.OriginalAnchor,
		NewAnchor:      req.NewAnchor,
	}
	if len(req.OriginalIntent) > 0 {
		var i ir.Intent
		if err := json.Unmarshal(req.OriginalIntent, &i); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "original_intent decode: " + err.Error(),
			})
			return
		}
		in.OriginalIntent = &i
	}
	if len(req.NewIntent) > 0 {
		var i ir.Intent
		if err := json.Unmarshal(req.NewIntent, &i); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "new_intent decode: " + err.Error(),
			})
			return
		}
		in.NewIntent = &i
	}
	if len(req.OriginalPlan) > 0 {
		var p ir.PlanTree
		if err := json.Unmarshal(req.OriginalPlan, &p); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "original_plan decode: " + err.Error(),
			})
			return
		}
		in.OriginalPlan = &p
	}
	if len(req.NewPlan) > 0 {
		var p ir.PlanTree
		if err := json.Unmarshal(req.NewPlan, &p); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "new_plan decode: " + err.Error(),
			})
			return
		}
		in.NewPlan = &p
	}

	cls := materiality.Classify(in)
	writeJSON(w, http.StatusOK, classifyResponse{
		Material: cls.Material,
		Reasons:  cls.Reasons,
	})
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
