package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun_PrintsVersion_WithVersionFlag(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run([]string{"-version"}, strings.NewReader(""), &stdout, &stderr,
		func(string) string { return "" }, func() []string { return nil }); code != 0 {
		t.Fatalf("version returned %d: %s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != version {
		t.Fatalf("version output = %q, want %q", stdout.String(), version)
	}
}

func TestRun_RejectsAuditorStart_WithoutVerdictPacket(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run(nil, strings.NewReader(""), &stdout, &stderr,
		func(string) string { return "" }, func() []string { return nil }); code != 3 {
		t.Fatalf("empty invocation returned %d, want 3", code)
	}
	if !strings.Contains(stderr.String(), "isolated session") {
		t.Fatalf("isolation error missing: %q", stderr.String())
	}
}

func TestRun_RejectsPositionalArgument_WithoutStarting(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run([]string{"-version", "extra"}, strings.NewReader(""), &stdout, &stderr,
		func(string) string { return "" }, func() []string { return nil }); code != 2 {
		t.Fatalf("positional argument returned %d, want 2", code)
	}
}
