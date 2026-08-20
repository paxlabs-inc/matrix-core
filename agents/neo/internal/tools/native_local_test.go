package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func newNativeTestRuntime(t *testing.T) *nativeLocal {
	t.Helper()
	root := t.TempDir()
	n, err := newNativeLocal(root, nil, filepath.Join(t.TempDir(), "services"), "git")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func nativeCall(t *testing.T, n *nativeLocal, name string, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	out, isErr, err := n.call(context.Background(), name, args)
	if err != nil || isErr {
		t.Fatalf("%s failed: output=%q isErr=%v err=%v", name, out, isErr, err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("decode %s output: %v (%s)", name, err, out)
	}
	return decoded
}

func TestNativeFilesystemRealOperationsAndContainment(t *testing.T) {
	n := newNativeTestRuntime(t)
	nativeCall(t, n, nativeCreateDir, map[string]interface{}{"path": "docs"})
	nativeCall(t, n, nativeWriteFile, map[string]interface{}{"path": "docs/note.txt", "content": "alpha beta"})
	nativeCall(t, n, nativeEditFile, map[string]interface{}{"path": "docs/note.txt", "old_text": "beta", "new_text": "gamma"})
	read := nativeCall(t, n, nativeReadFile, map[string]interface{}{"path": "docs/note.txt"})
	if read["content"] != "alpha gamma" {
		t.Fatalf("content = %#v", read["content"])
	}
	search := nativeCall(t, n, nativeSearchFiles, map[string]interface{}{"path": ".", "query": "gamma"})
	if len(search["results"].([]interface{})) != 1 {
		t.Fatalf("search = %#v", search)
	}
	nativeCall(t, n, nativeMoveFile, map[string]interface{}{"source": "docs/note.txt", "destination": "docs/moved.txt"})
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(n.root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, args := range []map[string]interface{}{{"path": "../outside", "content": "bad"}, {"path": "escape/bad", "content": "bad"}} {
		if _, isErr, err := n.call(context.Background(), nativeWriteFile, args); err != nil || !isErr {
			t.Fatalf("escape write was not rejected: isErr=%v err=%v", isErr, err)
		}
	}
}

func TestNativeShellUsesBoundedCredentialFilteredEnvironment(t *testing.T) {
	n := newNativeTestRuntime(t)
	t.Setenv("RAILWAY_API_TOKEN", "must-not-leak")
	result := nativeCall(t, n, nativeShell, map[string]interface{}{"command": "printf '%s|%s' \"$PWD\" \"${RAILWAY_API_TOKEN-unset}\"", "timeout_seconds": float64(5)})
	stdout := result["stdout"].(map[string]interface{})
	output := stdout["text"].(string)
	if !strings.Contains(output, n.root+"|unset") || strings.Contains(output, "must-not-leak") {
		t.Fatalf("shell environment = %q", output)
	}
	result = nativeCall(t, n, nativeShell, map[string]interface{}{"command": "sleep 2", "timeout_seconds": float64(1)})
	if timedOut, _ := result["timed_out"].(bool); !timedOut {
		t.Fatalf("expected timeout: %#v", result)
	}
}

func TestNativeShellReturnsStructuredRetrievableTruncation(t *testing.T) {
	n := newNativeTestRuntime(t)
	result := nativeCall(t, n, nativeShell, map[string]interface{}{
		"command": "head -c 200000 /dev/zero | tr '\\000' x; printf problem >&2",
	})
	if !result["truncated"].(bool) || result["output_id"] == "" {
		t.Fatalf("expected retrievable truncation: %#v", result)
	}
	if got := result["stderr"].(map[string]interface{})["text"]; got != "problem" {
		t.Fatalf("stderr = %#v", got)
	}
	page := nativeCall(t, n, nativeShellOutput, map[string]interface{}{
		"output_id": result["output_id"], "stream": "stdout", "offset": float64(nativeMaxStreamPreview), "max_bytes": float64(4096),
	})
	if len(page["content"].(string)) != 4096 || page["next_offset"].(float64) != float64(nativeMaxStreamPreview+4096) {
		t.Fatalf("retrieved page = %#v", page)
	}
}

func TestNativePatchFilesIsAllOrNothing(t *testing.T) {
	n := newNativeTestRuntime(t)
	nativeCall(t, n, nativeWriteFile, map[string]interface{}{"path": "a.txt", "content": "alpha"})
	nativeCall(t, n, nativeWriteFile, map[string]interface{}{"path": "b.txt", "content": "beta"})
	result := nativeCall(t, n, nativePatchFiles, map[string]interface{}{"changes": []interface{}{
		map[string]interface{}{"path": "a.txt", "old_text": "alpha", "new_text": "one"},
		map[string]interface{}{"path": "b.txt", "old_text": "beta", "new_text": "two"},
		map[string]interface{}{"path": "c.txt", "content": "three", "expected_sha256": "missing"},
	}})
	if !result["applied"].(bool) || len(result["files"].([]interface{})) != 3 {
		t.Fatalf("patch result = %#v", result)
	}
	for path, want := range map[string]string{"a.txt": "one", "b.txt": "two", "c.txt": "three"} {
		got, err := os.ReadFile(filepath.Join(n.root, path))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v", path, got, err)
		}
	}

	_, isErr, err := n.call(context.Background(), nativePatchFiles, map[string]interface{}{"changes": []interface{}{
		map[string]interface{}{"path": "a.txt", "content": "changed"},
		map[string]interface{}{"path": "b.txt", "old_text": "missing-anchor", "new_text": "changed"},
	}})
	if err != nil || !isErr {
		t.Fatalf("invalid patch = isErr %v err %v", isErr, err)
	}
	for path, want := range map[string]string{"a.txt": "one", "b.txt": "two"} {
		got, _ := os.ReadFile(filepath.Join(n.root, path))
		if string(got) != want {
			t.Fatalf("failed patch partially changed %s to %q", path, got)
		}
	}
}

func TestNativeCommandsRejectInlineCredentialsBeforeExecutionOrPersistence(t *testing.T) {
	n := newNativeTestRuntime(t)
	for _, call := range []struct {
		name string
		args map[string]interface{}
	}{
		{name: nativeShell, args: map[string]interface{}{"command": "API_TOKEN=supersecretvalue env"}},
		{name: nativeServiceStart, args: map[string]interface{}{"name": "unsafe", "command": "curl -H 'Authorization: Bearer supersecretvalue' https://example.test"}},
	} {
		out, isErr, err := n.call(context.Background(), call.name, call.args)
		if err != nil || !isErr || !strings.Contains(out, "inline credential") {
			t.Fatalf("%s inline credential result = (%q, %v, %v)", call.name, out, isErr, err)
		}
	}
	if len(n.services) != 0 {
		t.Fatalf("unsafe service reached durable registry: %#v", n.services)
	}
}

func TestNativeDurableServiceLifecycleAndRestore(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(t.TempDir(), "services")
	n, err := newNativeLocal(root, nil, state, "git")
	if err != nil {
		t.Fatal(err)
	}
	started := nativeCall(t, n, nativeServiceStart, map[string]interface{}{"name": "clock", "command": "while :; do echo tick; sleep 0.1; done", "autostart": true})
	pid := int(started["pid"].(float64))
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	time.Sleep(250 * time.Millisecond)
	logs := nativeCall(t, n, nativeServiceLogs, map[string]interface{}{"name": "clock", "lines": float64(20)})
	if !strings.Contains(logs["output"].(string), "tick") {
		t.Fatalf("logs = %#v", logs)
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && n.running(n.services["clock"]) {
		time.Sleep(20 * time.Millisecond)
	}
	restored, err := newNativeLocal(root, nil, state, "git")
	if err != nil {
		t.Fatal(err)
	}
	view := nativeCall(t, restored, nativeServiceRestart, map[string]interface{}{"name": "clock"})
	newPID := int(view["pid"].(float64))
	t.Cleanup(func() { _ = syscall.Kill(-newPID, syscall.SIGKILL) })
	if newPID == pid || !view["running"].(bool) {
		t.Fatalf("restored service = %#v", view)
	}
	stopped := nativeCall(t, restored, nativeServiceStop, map[string]interface{}{"name": "clock"})
	if stopped["running"].(bool) || stopped["autostart"].(bool) {
		t.Fatalf("stopped service = %#v", stopped)
	}
}

func TestNativeGitSurfaceIsReadOnlyAndReal(t *testing.T) {
	n := newNativeTestRuntime(t)
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = n.root
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Neo Test", "GIT_AUTHOR_EMAIL=neo@example.test", "GIT_COMMITTER_NAME=Neo Test", "GIT_COMMITTER_EMAIL=neo@example.test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit("init")
	if err := os.WriteFile(filepath.Join(n.root, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "a.txt")
	runGit("commit", "-m", "initial")
	if err := os.WriteFile(filepath.Join(n.root, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	status := nativeCall(t, n, nativeGitStatus, nil)
	if !strings.Contains(status["output"].(string), "a.txt") {
		t.Fatalf("status = %#v", status)
	}
	diff := nativeCall(t, n, nativeGitDiff, nil)
	if !strings.Contains(diff["output"].(string), "-one") {
		t.Fatalf("diff = %#v", diff)
	}
	log := nativeCall(t, n, nativeGitLog, map[string]interface{}{"limit": float64(5)})
	if !strings.Contains(log["output"].(string), "initial") {
		t.Fatalf("log = %#v", log)
	}
	for _, schema := range nativeSchemas() {
		if strings.Contains(schema.Function.Name, "commit") || strings.Contains(schema.Function.Name, "push") || strings.Contains(schema.Function.Name, "reset") {
			t.Fatalf("write-capable git tool advertised: %s", schema.Function.Name)
		}
	}
}

func TestManagerAdvertisesAndPreflightsNativeToolsWithoutLocalMCP(t *testing.T) {
	n := newNativeTestRuntime(t)
	m := &Manager{native: n, byFunc: map[string]*boundTool{}}
	if !m.NativeLocalEnabled() {
		t.Fatal("native runtime not enabled")
	}
	names := m.NaturalToolNames()
	for _, want := range []string{nativeReadFile, nativeShell, nativeServiceStart, nativeGitStatus} {
		if !containsName(names, want) {
			t.Fatalf("missing native tool %s", want)
		}
		if err := m.Preflight([]string{want}); err != nil {
			t.Fatalf("preflight %s: %v", want, err)
		}
	}
	if _, ok := m.byFunc["fs__read_text_file"]; ok {
		t.Fatal("native tool unexpectedly depends on MCP binding")
	}
}

func TestNativeRuntimeShadowsLegacyLocalSchemasButKeepsReplayBinding(t *testing.T) {
	m := &Manager{native: newNativeTestRuntime(t)}
	for _, test := range []struct {
		alias string
		name  string
		want  bool
	}{
		{alias: "fs", name: "write_file", want: true},
		{alias: "exec", name: "shell", want: true},
		{alias: "git", name: "status", want: true},
		{alias: "browser", name: "browser_navigate", want: false},
		{alias: "web-search", name: "web_search", want: false},
	} {
		bound := &boundTool{alias: test.alias, name: test.name}
		if got := m.nativeShadows(bound); got != test.want {
			t.Fatalf("nativeShadows(%s,%s) = %v, want %v", test.alias, test.name, got, test.want)
		}
	}
}

func containsName(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
