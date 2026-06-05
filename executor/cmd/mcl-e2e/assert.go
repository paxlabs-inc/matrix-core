// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"bytes"
	"fmt"
	"os"
)

// AssertCtx accumulates pass/fail across the run so a single failure
// doesn't mask later ones; main() exits non-zero if Failed > 0.
type AssertCtx struct {
	Passed int
	Failed int
	t      *Transcript
}

func NewAssertCtx(t *Transcript) *AssertCtx { return &AssertCtx{t: t} }

const (
	cReset  = "\x1b[0m"
	cGreen  = "\x1b[32m"
	cRed    = "\x1b[31m"
	cYellow = "\x1b[33m"
	cBlue   = "\x1b[34m"
	cBold   = "\x1b[1m"
)

// True asserts cond is true; logs label as PASS or FAIL.
func (a *AssertCtx) True(label string, cond bool, detail string) {
	if cond {
		a.Passed++
		fmt.Fprintf(os.Stderr, "  %s✓ PASS%s  %s%s\n", cGreen, cReset, label, fmtDetail(detail))
		a.t.Event("assert", "assert", map[string]interface{}{"label": label, "ok": true, "detail": detail})
		return
	}
	a.Failed++
	fmt.Fprintf(os.Stderr, "  %s✗ FAIL%s  %s%s\n", cRed, cReset, label, fmtDetail(detail))
	a.t.Event("assert", "assert", map[string]interface{}{"label": label, "ok": false, "detail": detail})
}

// Equal asserts a == b (byte-equal for []byte / string).
func (a *AssertCtx) Equal(label string, expected, got interface{}) {
	ok := false
	switch e := expected.(type) {
	case []byte:
		if g, isBytes := got.([]byte); isBytes {
			ok = bytes.Equal(e, g)
		}
	case string:
		if g, isStr := got.(string); isStr {
			ok = e == g
		}
	default:
		ok = fmt.Sprint(expected) == fmt.Sprint(got)
	}
	if !ok {
		a.True(label, false, fmt.Sprintf("expected=%v got=%v", short(expected), short(got)))
		return
	}
	a.True(label, true, "")
}

// NoError asserts err is nil.
func (a *AssertCtx) NoError(label string, err error) {
	if err == nil {
		a.True(label, true, "")
		return
	}
	a.True(label, false, err.Error())
}

// Section prints a section banner for the run output.
func Section(title string) {
	fmt.Fprintf(os.Stderr, "\n%s%s━━━ %s ━━━%s\n", cBold, cBlue, title, cReset)
}

// Subsection prints a smaller phase header.
func Subsection(title string) {
	fmt.Fprintf(os.Stderr, "%s──── %s%s\n", cYellow, title, cReset)
}

// Summary prints the final pass/fail tally.
func (a *AssertCtx) Summary() {
	fmt.Fprintf(os.Stderr, "\n%sASSERTIONS:%s ", cBold, cReset)
	fmt.Fprintf(os.Stderr, "%s%d passed%s, ", cGreen, a.Passed, cReset)
	if a.Failed > 0 {
		fmt.Fprintf(os.Stderr, "%s%d failed%s\n", cRed, a.Failed, cReset)
	} else {
		fmt.Fprintf(os.Stderr, "0 failed\n")
	}
}

// ExitCode returns non-zero iff any assertion failed.
func (a *AssertCtx) ExitCode() int {
	if a.Failed > 0 {
		return 1
	}
	return 0
}

func short(v interface{}) string {
	s := fmt.Sprint(v)
	if len(s) > 200 {
		return s[:197] + "…"
	}
	return s
}

func fmtDetail(d string) string {
	if d == "" {
		return ""
	}
	return "  " + cYellow + "(" + d + ")" + cReset
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
