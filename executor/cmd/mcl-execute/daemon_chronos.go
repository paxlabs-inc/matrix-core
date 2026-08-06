// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	machinechronos "matrix/machine/chronos"
)

type localAlarmRequest struct {
	ID             string          `json:"id"`
	Label          string          `json:"label"`
	Kind           string          `json:"kind"`
	DelaySeconds   int64           `json:"delay_seconds"`
	FireAt         string          `json:"fire_at"`
	CronExpr       string          `json:"cron_expr"`
	Timezone       string          `json:"timezone"`
	ConversationID string          `json:"conversation_id"`
	WakeMessage    string          `json:"wake_message"`
	Payload        json.RawMessage `json:"payload"`
	IdempotencyKey string          `json:"idempotency_key"`
	MaxFailures    int             `json:"max_failures"`
	Target         string          `json:"target"`
	NextFireAt     string          `json:"next_fire_at"`
}

func (d *daemonState) handleLocalChronosAlarms(response http.ResponseWriter, request *http.Request) {
	if !d.requireChronosCapability(response, request) {
		return
	}
	if d.chronosEngine == nil {
		writeJSON(response, http.StatusServiceUnavailable, map[string]interface{}{"ok": false, "error": map[string]string{"code": "local_chronos_unavailable"}})
		return
	}
	switch request.Method {
	case http.MethodGet:
		alarms, err := d.chronosEngine.List(request.Context())
		if err != nil {
			writeLocalChronosError(response, err)
			return
		}
		limit := 100
		if value, err := strconv.Atoi(request.URL.Query().Get("limit")); err == nil && value > 0 && value < limit {
			limit = value
		}
		if len(alarms) > limit {
			alarms = alarms[:limit]
		}
		writeJSON(response, http.StatusOK, map[string]interface{}{"ok": true, "data": map[string]interface{}{"alarms": localAlarmViews(alarms)}})
	case http.MethodPost:
		var input localAlarmRequest
		decoder := json.NewDecoder(http.MaxBytesReader(response, request.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			writeJSON(response, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": map[string]string{"code": "invalid_request", "message": err.Error()}})
			return
		}
		alarmRequest, err := d.localCreateRequest(input)
		if err != nil {
			writeLocalChronosError(response, err)
			return
		}
		alarm, created, err := d.chronosEngine.Create(request.Context(), alarmRequest)
		if err != nil {
			writeLocalChronosError(response, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(response, status, map[string]interface{}{"ok": true, "data": localAlarmView(alarm), "created": created})
	default:
		response.Header().Set("Allow", "GET, POST")
		writeJSON(response, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (d *daemonState) handleLocalChronosAlarm(response http.ResponseWriter, request *http.Request) {
	if !d.requireChronosCapability(response, request) {
		return
	}
	if d.chronosEngine == nil {
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{"error": "local Chronos unavailable"})
		return
	}
	path := strings.TrimPrefix(request.URL.Path, "/chronos/v1/alarms/")
	reschedule := strings.HasSuffix(path, "/reschedule")
	id := strings.TrimSuffix(path, "/reschedule")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(response, request)
		return
	}
	if reschedule && request.Method == http.MethodPost {
		var input localAlarmRequest
		if err := json.NewDecoder(http.MaxBytesReader(response, request.Body, 4096)).Decode(&input); err != nil {
			writeLocalChronosError(response, err)
			return
		}
		next, err := time.Parse(time.RFC3339Nano, input.NextFireAt)
		if err != nil {
			writeLocalChronosError(response, err)
			return
		}
		changed, err := d.chronosEngine.Reschedule(request.Context(), id, next)
		if err != nil {
			writeLocalChronosError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, map[string]interface{}{"ok": true, "data": map[string]interface{}{"id": id, "next_fire_at": next.UTC(), "changed": changed}})
		return
	}
	switch request.Method {
	case http.MethodGet:
		alarms, err := d.chronosEngine.List(request.Context())
		if err != nil {
			writeLocalChronosError(response, err)
			return
		}
		for _, alarm := range alarms {
			if alarm.ID == id {
				writeJSON(response, http.StatusOK, map[string]interface{}{"ok": true, "data": localAlarmView(alarm)})
				return
			}
		}
		writeLocalChronosError(response, machinechronos.ErrNotFound)
	case http.MethodDelete:
		changed, err := d.chronosEngine.Cancel(request.Context(), id)
		if err != nil {
			writeLocalChronosError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, map[string]interface{}{"ok": true, "data": map[string]interface{}{"id": id, "status": machinechronos.StatusCanceled, "changed": changed}})
	default:
		response.Header().Set("Allow", "GET, DELETE, POST")
		writeJSON(response, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (d *daemonState) localCreateRequest(input localAlarmRequest) (machinechronos.CreateRequest, error) {
	now := time.Now().UTC()
	request := machinechronos.CreateRequest{ID: input.ID, IdempotencyKey: strings.TrimSpace(input.IdempotencyKey),
		CronExpr: strings.TrimSpace(input.CronExpr), Timezone: strings.TrimSpace(input.Timezone), MisfirePolicy: machinechronos.MisfireCoalesce,
		Body: machinechronos.Body{Payload: input.Payload, WakeMessage: strings.TrimSpace(input.WakeMessage),
			ConversationID: strings.TrimSpace(input.ConversationID), Label: strings.TrimSpace(input.Label),
			Target: strings.TrimSpace(input.Target), MaxFailures: input.MaxFailures}}
	if request.Body.WakeMessage == "" {
		return request, errors.New("wake_message is required")
	}
	switch strings.TrimSpace(input.Kind) {
	case "once":
		request.MisfirePolicy = machinechronos.MisfireFireOnce
		if input.DelaySeconds > 0 && input.FireAt == "" {
			request.NextFire = now.Add(time.Duration(input.DelaySeconds) * time.Second)
		} else if input.FireAt != "" && input.DelaySeconds == 0 {
			parsed, err := time.Parse(time.RFC3339Nano, input.FireAt)
			if err != nil {
				return request, err
			}
			request.NextFire = parsed.UTC()
		} else {
			return request, errors.New("once alarm requires exactly one of delay_seconds or fire_at")
		}
	case "cron":
		if request.CronExpr == "" {
			return request, errors.New("cron alarm requires cron_expr")
		}
	default:
		return request, errors.New("kind must be once or cron")
	}
	if request.IdempotencyKey == "" {
		sum := sha256.Sum256([]byte(newRequestID() + "\x00" + request.Body.WakeMessage))
		request.IdempotencyKey = "local-" + hex.EncodeToString(sum[:16])
	}
	return request, nil
}

func (d *daemonState) requireChronosCapability(response http.ResponseWriter, request *http.Request) bool {
	want := d.chronosCapability
	got := strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
	if want == "" || len(got) != len(want) || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		writeJSON(response, http.StatusUnauthorized, map[string]interface{}{"ok": false, "error": map[string]string{"code": "unauthorized"}})
		return false
	}
	return true
}

func localAlarmViews(alarms []machinechronos.Alarm) []map[string]interface{} {
	views := make([]map[string]interface{}, 0, len(alarms))
	for _, alarm := range alarms {
		views = append(views, localAlarmView(alarm))
	}
	return views
}

func localAlarmView(alarm machinechronos.Alarm) map[string]interface{} {
	return map[string]interface{}{"id": alarm.ID, "idempotency_key": alarm.IdempotencyKey,
		"label": alarm.Body.Label, "kind": map[bool]string{true: "cron", false: "once"}[alarm.CronExpr != "" || alarm.Interval > 0],
		"next_fire_at": alarm.NextFire, "cron_expr": alarm.CronExpr, "timezone": alarm.Timezone,
		"conversation_id": alarm.Body.ConversationID, "wake_message": alarm.Body.WakeMessage, "payload": alarm.Body.Payload,
		"status": alarm.Status, "delivery_attempts": alarm.DeliveryAttempts, "last_error": alarm.LastError,
		"created_at": alarm.CreatedAt, "updated_at": alarm.UpdatedAt}
}

func writeLocalChronosError(response http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	code := "invalid_request"
	if errors.Is(err, machinechronos.ErrNotFound) {
		status, code = http.StatusNotFound, "not_found"
	} else if errors.Is(err, machinechronos.ErrConflict) {
		status, code = http.StatusConflict, "idempotency_conflict"
	}
	writeJSON(response, status, map[string]interface{}{"ok": false, "error": map[string]string{"code": code, "message": err.Error()}})
}
