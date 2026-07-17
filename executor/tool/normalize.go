// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tool

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"matrix/executor/mcp"
)

// FailureClass is the stable layer at which a tool operation failed.
type FailureClass string

const (
	FailureNone          FailureClass = ""
	FailureInvocation    FailureClass = "invocation"
	FailureTransport     FailureClass = "transport"
	FailureProcess       FailureClass = "process"
	FailureProtocol      FailureClass = "protocol"
	FailureHTTP          FailureClass = "http"
	FailureApplication   FailureClass = "application"
	FailureValidation    FailureClass = "validation"
	FailurePolicy        FailureClass = "policy"
	FailureAuthorization FailureClass = "authorization"
	FailureConflict      FailureClass = "conflict"
	FailureCancellation  FailureClass = "cancellation"
)

// FailureError preserves a failure layer across ordinary error wrapping.
type FailureError struct {
	Class FailureClass
	Err   error
}

func (e *FailureError) Error() string { return e.Err.Error() }
func (e *FailureError) Unwrap() error { return e.Err }

func failureError(class FailureClass, err error) error {
	if err == nil {
		return nil
	}
	return &FailureError{Class: class, Err: err}
}

// FailureClassOf returns the stable layer carried by an invocation error.
func FailureClassOf(err error) FailureClass {
	if err == nil {
		return FailureNone
	}
	var fe *FailureError
	if errors.As(err, &fe) {
		return fe.Class
	}
	var rpc *mcp.RPCError
	if errors.As(err, &rpc) {
		return FailureProtocol
	}
	if errors.Is(err, ErrUnknownTool) || errors.Is(err, ErrInvalidURI) ||
		errors.Is(err, ErrUnpinnedTool) || errors.Is(err, ErrSideEffectDenied) ||
		errors.Is(err, ErrInvalidSideEffect) {
		return FailureInvocation
	}
	return FailureTransport
}

type envelopeObservation struct {
	processExit *int
	timedOut    bool
	httpStatus  int
	appOK       *bool
	errorText   string
}

// NormalizeResult derives one typed outcome from recognized tool envelopes.
// It is deliberately conservative: unrecognized text and ordinary JSON remain
// successful and byte-identical.
func NormalizeResult(res *Result) *Result {
	if res == nil {
		return nil
	}
	originalProtocolError := res.IsError
	var observed envelopeObservation
	for _, content := range res.Content {
		if content.Type != ContentTypeText {
			continue
		}
		mergeObservation(&observed, observeJSON(content.Text, true))
	}

	res.ProcessExitCode = observed.processExit
	res.ProcessTimedOut = observed.timedOut
	res.HTTPStatus = observed.httpStatus
	res.ApplicationOK = observed.appOK

	switch {
	case observed.httpStatus != 0 && (observed.httpStatus < 200 || observed.httpStatus >= 300):
		res.IsError = true
		res.FailureClass = FailureHTTP
		res.Retryable = observed.httpStatus == 429 || observed.httpStatus >= 500
		res.FailureMessage = fmt.Sprintf("HTTP request failed with status %d.", observed.httpStatus)
	case observed.timedOut:
		res.IsError = true
		res.FailureClass = FailureProcess
		res.Retryable = true
		res.FailureMessage = "The process timed out before the operation completed."
	case observed.processExit != nil && *observed.processExit != 0:
		res.IsError = true
		res.FailureClass = FailureProcess
		res.FailureMessage = fmt.Sprintf("The process failed with exit code %d.", *observed.processExit)
	case observed.appOK != nil && !*observed.appOK:
		res.IsError = true
		res.FailureClass = FailureApplication
		res.FailureMessage = applicationFailureMessage(observed.errorText)
	case observed.appOK == nil && observed.errorText != "":
		res.IsError = true
		res.FailureClass = FailureApplication
		res.FailureMessage = applicationFailureMessage(observed.errorText)
	case originalProtocolError:
		res.IsError = true
		res.FailureClass = FailureProtocol
		res.FailureMessage = "The tool protocol reported an error."
	default:
		res.IsError = false
		res.FailureClass = FailureNone
		res.FailureMessage = ""
		res.Retryable = false
	}
	return res
}

func observeJSON(text string, allowShellStdout bool) envelopeObservation {
	var raw map[string]json.RawMessage
	if json.Unmarshal([]byte(strings.TrimSpace(text)), &raw) != nil || raw == nil {
		return envelopeObservation{}
	}
	var out envelopeObservation
	toolName := rawString(raw["tool"])
	if code, ok := rawInt(raw["exit_code"]); ok {
		out.processExit = &code
	}
	if timedOut, ok := rawBool(raw["timed_out"]); ok {
		out.timedOut = timedOut
	}
	for _, key := range []string{"http_status", "status_code"} {
		if status, ok := rawInt(raw[key]); ok && status >= 100 && status <= 599 {
			out.httpStatus = status
			break
		}
	}

	// The exec shell's top-level ok field describes process success. Its stdout
	// may carry a separate structured application envelope.
	if toolName != "shell" {
		if okValue, ok := rawBool(raw["ok"]); ok {
			out.appOK = &okValue
		}
		if success, ok := rawBool(raw["success"]); ok {
			out.appOK = &success
		}
		out.errorText = rawError(raw["error"])
	}
	if allowShellStdout && toolName == "shell" && out.processExit != nil && *out.processExit == 0 {
		nested := observeJSON(rawString(raw["stdout"]), false)
		mergeObservation(&out, nested)
	}
	return out
}

func mergeObservation(dst *envelopeObservation, src envelopeObservation) {
	if src.processExit != nil {
		dst.processExit = src.processExit
	}
	if src.timedOut {
		dst.timedOut = true
	}
	if src.httpStatus != 0 {
		dst.httpStatus = src.httpStatus
	}
	if src.appOK != nil {
		dst.appOK = src.appOK
	}
	if src.errorText != "" {
		dst.errorText = src.errorText
	}
}

func rawBool(raw json.RawMessage) (bool, bool) {
	if len(raw) == 0 {
		return false, false
	}
	var value bool
	if json.Unmarshal(raw, &value) != nil {
		return false, false
	}
	return value, true
}

func rawInt(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var value int
	if json.Unmarshal(raw, &value) != nil {
		return 0, false
	}
	return value, true
}

func rawString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func rawError(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if text := rawString(raw); text != "" {
		return text
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return ""
	}
	for _, key := range []string{"message", "detail", "reason", "code"} {
		if text := rawString(object[key]); text != "" {
			return text
		}
	}
	return "structured application error"
}

func applicationFailureMessage(detail string) string {
	detail = strings.Join(strings.Fields(detail), " ")
	if detail == "" {
		return "The application rejected the operation."
	}
	if len(detail) > 240 {
		detail = detail[:240] + "…"
	}
	return "The application rejected the operation: " + detail
}
