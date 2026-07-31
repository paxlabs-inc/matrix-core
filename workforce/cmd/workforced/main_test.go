package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun_PrintsVersion_WithVersionFlag(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run([]string{"-version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("version returned %d: %s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != version {
		t.Fatalf("version output = %q, want %q", stdout.String(), version)
	}
}

func TestRun_RejectsRuntimeStart_WithoutConfiguredService(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 2 {
		t.Fatalf("empty invocation returned %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("usage error missing: %q", stderr.String())
	}
}

func TestRun_RejectsUnknownFlag_WithoutStarting(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run([]string{"-unknown"}, &stdout, &stderr); code != 2 {
		t.Fatalf("unknown flag returned %d, want 2", code)
	}
}
