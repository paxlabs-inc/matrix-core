package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"centra/executor/tool"
	"centra/agents/neo/internal/llm"
)

var inlineCredentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:^|[[:space:];])(?:export[[:space:]]+)?[A-Z0-9_]*(?:TOKEN|SECRET|PASSWORD|PASSWD|API_KEY|PRIVATE_KEY)[[:space:]]*=[[:space:]]*['"]?[^[:space:]'"]{8,}`),
	regexp.MustCompile(`(?i)authorization[[:space:]]*:[[:space:]]*bearer[[:space:]]+[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?i)https?://[^/@[:space:]]+:[^/@[:space:]]+@`),
	regexp.MustCompile(`(?:gh[pousr]_[A-Za-z0-9]{20,}|sk-[A-Za-z0-9_-]{20,})`),
}

const (
	nativeReadFile       = "read_text_file"
	nativeReadMany       = "read_multiple_files"
	nativeWriteFile      = "write_file"
	nativeEditFile       = "edit_file"
	nativePatchFiles     = "patch_files"
	nativeCreateDir      = "create_directory"
	nativeListDir        = "list_directory"
	nativeTree           = "directory_tree"
	nativeMoveFile       = "move_file"
	nativeSearchFiles    = "search_files"
	nativeFileInfo       = "get_file_info"
	nativeShell          = "shell"
	nativeShellOutput    = "shell_output"
	nativeServiceStart   = "service_start"
	nativeServiceList    = "service_list"
	nativeServiceLogs    = "service_logs"
	nativeServiceStop    = "service_stop"
	nativeServiceRestart = "service_restart"
	nativeGitStatus      = "git_status"
	nativeGitDiff        = "git_diff"
	nativeGitLog         = "git_log"
	nativeGitShow        = "git_show"
	nativeGitBranch      = "git_branch"

	nativeMaxFileBytes       = 1 << 20
	nativeMaxOutput          = 128 << 10
	nativeMaxStreamPreview   = nativeMaxOutput / 2
	nativeMaxRetainedOutput  = 8 << 20
	nativeMaxPatchFiles      = 32
	nativeShellOutputMaxRead = 256 << 10
)

type nativeLocal struct {
	root      string
	readRoots []string
	stateDir  string
	gitPath   string
	mu        sync.Mutex
	services  map[string]*nativeService
}

type nativeService struct {
	Name       string `json:"name"`
	Command    string `json:"command"`
	Cwd        string `json:"cwd"`
	Autostart  bool   `json:"autostart"`
	PID        int    `json:"pid,omitempty"`
	StartTicks uint64 `json:"start_ticks,omitempty"`
	LogPath    string `json:"log_path"`
	StartedAt  string `json:"started_at,omitempty"`
	cmd        *exec.Cmd
}

type nativeRegistry struct {
	Services []*nativeService `json:"services"`
}

func newNativeLocal(root string, readRoots []string, stateDir, gitPath string) (*nativeLocal, error) {
	root, err := canonicalExistingDir(root)
	if err != nil {
		return nil, fmt.Errorf("native local root: %w", err)
	}
	if stateDir == "" {
		stateDir = filepath.Join(root, ".matrix", "services")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("native service state: %w", err)
	}
	n := &nativeLocal{root: root, stateDir: stateDir, gitPath: gitPath, services: map[string]*nativeService{}}
	for _, candidate := range readRoots {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			if resolved, err := canonicalExistingDir(candidate); err == nil && resolved != root {
				n.readRoots = append(n.readRoots, resolved)
			}
		}
	}
	if n.gitPath == "" {
		n.gitPath = "git"
	}
	n.cleanupShellOutputs()
	if err := n.loadAndRestore(); err != nil {
		return nil, err
	}
	return n, nil
}

func (n *nativeLocal) cleanupShellOutputs() {
	dir := filepath.Join(n.stateDir, "shell-output")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type retained struct {
		id       string
		modified time.Time
	}
	metas := make([]retained, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		info, statErr := entry.Info()
		if statErr != nil {
			continue
		}
		metas = append(metas, retained{id: strings.TrimSuffix(entry.Name(), ".json"), modified: info.ModTime()})
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].modified.After(metas[j].modified) })
	cutoff := time.Now().Add(-24 * time.Hour)
	for index, item := range metas {
		if index < 64 && item.modified.After(cutoff) {
			continue
		}
		for _, suffix := range []string{".json", ".stdout", ".stderr"} {
			_ = os.Remove(filepath.Join(dir, item.id+suffix))
		}
	}
}

func canonicalExistingDir(path string) (string, error) {
	abs, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", path)
	}
	return filepath.Clean(resolved), nil
}

func (n *nativeLocal) resolve(path string, write bool) (string, error) {
	if strings.TrimSpace(path) == "" {
		path = "."
	}
	candidate := filepath.FromSlash(path)
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(n.root, candidate)
	}
	candidate, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	candidate = filepath.Clean(candidate)
	roots := []string{n.root}
	if !write {
		roots = append(roots, n.readRoots...)
	}
	allowed := false
	for _, root := range roots {
		if within(root, candidate) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", errors.New("path is outside the allowed workspace")
	}
	probe := candidate
	for {
		if resolved, statErr := filepath.EvalSymlinks(probe); statErr == nil {
			suffix, relErr := filepath.Rel(probe, candidate)
			if relErr != nil {
				return "", relErr
			}
			final := filepath.Clean(filepath.Join(resolved, suffix))
			for _, root := range roots {
				if within(root, final) {
					return final, nil
				}
			}
			return "", errors.New("path resolves outside the allowed workspace")
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", errors.New("cannot resolve path")
		}
		probe = parent
	}
}

func within(root, path string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

func nativeSchemas() []llm.Tool {
	obj := func(properties map[string]interface{}, required ...string) map[string]interface{} {
		if properties == nil {
			properties = map[string]interface{}{}
		}
		v := map[string]interface{}{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			requiredValues := make([]interface{}, len(required))
			for i := range required {
				requiredValues[i] = required[i]
			}
			v["required"] = requiredValues
		}
		return v
	}
	str := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	integer := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "integer", "description": desc}
	}
	boolean := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "boolean", "description": desc}
	}
	return []llm.Tool{
		llm.NewFunctionTool(nativeReadFile, "Read a UTF-8 text file from the workspace or an approved attachment directory.", obj(map[string]interface{}{"path": str("Workspace-relative or approved absolute path."), "max_bytes": integer("Maximum bytes to return, up to 1048576.")}, "path")),
		llm.NewFunctionTool(nativeReadMany, "Read several UTF-8 text files in one bounded call.", obj(map[string]interface{}{"paths": map[string]interface{}{"type": "array", "items": str("File path"), "maxItems": 20}}, "paths")),
		llm.NewFunctionTool(nativeWriteFile, "Atomically create or replace a workspace text file.", obj(map[string]interface{}{"path": str("Workspace-relative path."), "content": str("Complete file content.")}, "path", "content")),
		llm.NewFunctionTool(nativeEditFile, "Apply an exact anchored text replacement to a workspace file.", obj(map[string]interface{}{"path": str("Workspace-relative path."), "old_text": str("Exact text to replace."), "new_text": str("Replacement text."), "replace_all": boolean("Replace every exact occurrence instead of requiring one.")}, "path", "old_text", "new_text")),
		llm.NewFunctionTool(nativePatchFiles, "Atomically apply a validated set of complete-file writes and exact anchored replacements. Every change is prepared before any file is committed; a validation or commit failure leaves the original set intact.", obj(map[string]interface{}{"changes": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": nativeMaxPatchFiles, "items": map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{"path": str("Workspace-relative file path."), "content": str("Complete replacement content. Mutually exclusive with old_text/new_text."), "old_text": str("Exact anchor to replace. Requires new_text."), "new_text": str("Replacement for old_text."), "replace_all": boolean("Replace every occurrence instead of requiring a unique anchor."), "expected_sha256": str("Optional lowercase SHA-256 of the current file, or 'missing' when the file must not exist.")}, "required": []interface{}{"path"}}}}, "changes")),
		llm.NewFunctionTool(nativeCreateDir, "Create a workspace directory and missing parents.", obj(map[string]interface{}{"path": str("Workspace-relative path.")}, "path")),
		llm.NewFunctionTool(nativeListDir, "List one workspace or attachment directory.", obj(map[string]interface{}{"path": str("Directory path; defaults to workspace root.")})),
		llm.NewFunctionTool(nativeTree, "Return a bounded recursive directory tree.", obj(map[string]interface{}{"path": str("Directory path; defaults to workspace root."), "max_depth": integer("Depth from 1 to 8.")})),
		llm.NewFunctionTool(nativeMoveFile, "Move or rename a workspace file or directory without overwriting the destination.", obj(map[string]interface{}{"source": str("Existing workspace path."), "destination": str("New workspace path.")}, "source", "destination")),
		llm.NewFunctionTool(nativeSearchFiles, "Search file names and bounded text contents under a directory.", obj(map[string]interface{}{"path": str("Directory path; defaults to workspace root."), "query": str("Case-insensitive literal text."), "max_results": integer("Maximum results from 1 to 200.")}, "query")),
		llm.NewFunctionTool(nativeFileInfo, "Inspect file type, size, permissions, and modification time.", obj(map[string]interface{}{"path": str("File or directory path.")}, "path")),
		llm.NewFunctionTool(nativeShell, "Run one bounded Bash command in a workspace directory with a credential-filtered environment. Returns separate stdout/stderr, exit status, byte counts, and a retrievable output_id whenever inline output is truncated. Use named service tools for long-running processes.", obj(map[string]interface{}{"command": str("Bash command."), "cwd": str("Workspace directory; defaults to root."), "timeout_seconds": integer("Timeout from 1 to 300 seconds.")}, "command")),
		llm.NewFunctionTool(nativeShellOutput, "Read a retained stdout or stderr segment from a truncated shell result.", obj(map[string]interface{}{"output_id": str("Identifier returned by shell."), "stream": map[string]interface{}{"type": "string", "enum": []interface{}{"stdout", "stderr"}}, "offset": integer("Zero-based byte offset."), "max_bytes": integer("Bytes to return, from 1 to 262144.")}, "output_id", "stream")),
		llm.NewFunctionTool(nativeServiceStart, "Start a durable named workspace process with persisted logs and restart metadata.", obj(map[string]interface{}{"name": str("Stable lowercase service name."), "command": str("Long-running shell command."), "cwd": str("Workspace directory; defaults to root."), "autostart": boolean("Restore the service after Neo/container restart; defaults true.")}, "name", "command")),
		llm.NewFunctionTool(nativeServiceList, "List durable named services and their verified process state.", obj(nil)),
		llm.NewFunctionTool(nativeServiceLogs, "Read the tail of a durable service log.", obj(map[string]interface{}{"name": str("Service name."), "lines": integer("Last 1 to 1000 lines.")}, "name")),
		llm.NewFunctionTool(nativeServiceStop, "Stop a durable named service process group.", obj(map[string]interface{}{"name": str("Service name.")}, "name")),
		llm.NewFunctionTool(nativeServiceRestart, "Restart a durable named service from its persisted command and cwd.", obj(map[string]interface{}{"name": str("Service name.")}, "name")),
		llm.NewFunctionTool(nativeGitStatus, "Read git status for a workspace repository.", obj(map[string]interface{}{"cwd": str("Repository directory; defaults to workspace root.")})),
		llm.NewFunctionTool(nativeGitDiff, "Read an unstaged, staged, or combined git diff without modifying the repository.", obj(map[string]interface{}{"cwd": str("Repository directory."), "staged": boolean("Show staged diff only."), "ref": str("Optional revision/range for git diff.")})),
		llm.NewFunctionTool(nativeGitLog, "Read bounded git history without modifying the repository.", obj(map[string]interface{}{"cwd": str("Repository directory."), "limit": integer("Commit limit from 1 to 100.")})),
		llm.NewFunctionTool(nativeGitShow, "Read one git object or revision without modifying the repository.", obj(map[string]interface{}{"cwd": str("Repository directory."), "revision": str("Revision; defaults to HEAD.")})),
		llm.NewFunctionTool(nativeGitBranch, "List local and remote git branches without modifying the repository.", obj(map[string]interface{}{"cwd": str("Repository directory.")})),
	}
}

func nativeResearchSchemas() []llm.Tool {
	allowed := map[string]bool{
		nativeReadFile: true, nativeReadMany: true, nativeListDir: true,
		nativeTree: true, nativeSearchFiles: true, nativeFileInfo: true,
		nativeGitStatus: true, nativeGitDiff: true, nativeGitLog: true,
		nativeGitShow: true, nativeGitBranch: true,
	}
	out := make([]llm.Tool, 0, len(allowed))
	for _, schema := range nativeSchemas() {
		if allowed[schema.Function.Name] {
			out = append(out, schema)
		}
	}
	return out
}

func isNativeTool(name string) bool {
	for _, schema := range nativeSchemas() {
		if schema.Function.Name == name {
			return true
		}
	}
	return false
}

func (n *nativeLocal) call(ctx context.Context, name string, args map[string]interface{}) (string, bool, error) {
	var value interface{}
	var err error
	switch name {
	case nativeReadFile:
		value, err = n.readFile(args)
	case nativeReadMany:
		value, err = n.readMany(args)
	case nativeWriteFile:
		value, err = n.writeFile(args)
	case nativeEditFile:
		value, err = n.editFile(args)
	case nativePatchFiles:
		value, err = n.patchFiles(args)
	case nativeCreateDir:
		value, err = n.createDir(args)
	case nativeListDir:
		value, err = n.listDir(args)
	case nativeTree:
		value, err = n.tree(args)
	case nativeMoveFile:
		value, err = n.moveFile(args)
	case nativeSearchFiles:
		value, err = n.searchFiles(args)
	case nativeFileInfo:
		value, err = n.fileInfo(args)
	case nativeShell:
		value, err = n.shell(ctx, args)
	case nativeShellOutput:
		value, err = n.shellOutput(args)
	case nativeServiceStart:
		value, err = n.serviceStart(args)
	case nativeServiceList:
		value, err = n.serviceList(), nil
	case nativeServiceLogs:
		value, err = n.serviceLogs(args)
	case nativeServiceStop:
		value, err = n.serviceStop(args)
	case nativeServiceRestart:
		value, err = n.serviceRestart(args)
	case nativeGitStatus, nativeGitDiff, nativeGitLog, nativeGitShow, nativeGitBranch:
		value, err = n.git(ctx, name, args)
	default:
		return "", true, fmt.Errorf("unknown native tool %q", name)
	}
	if err != nil {
		return err.Error(), true, nil
	}
	out, marshalErr := json.Marshal(value)
	if marshalErr != nil {
		return "", true, marshalErr
	}
	return string(out), false, nil
}

func argString(args map[string]interface{}, key string) string {
	v, _ := args[key].(string)
	return strings.TrimSpace(v)
}

func argInt(args map[string]interface{}, key string, fallback int) int {
	switch v := args[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	case json.Number:
		i, _ := strconv.Atoi(v.String())
		return i
	default:
		return fallback
	}
}

func argBool(args map[string]interface{}, key string, fallback bool) bool {
	if v, ok := args[key].(bool); ok {
		return v
	}
	return fallback
}

func (n *nativeLocal) readFile(args map[string]interface{}) (interface{}, error) {
	path, err := n.resolve(argString(args, "path"), false)
	if err != nil {
		return nil, err
	}
	max := argInt(args, "max_bytes", 256<<10)
	if max < 1 || max > nativeMaxFileBytes {
		max = nativeMaxFileBytes
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(max+1)))
	if err != nil {
		return nil, err
	}
	truncated := len(b) > max
	if truncated {
		b = b[:max]
	}
	return map[string]interface{}{"path": path, "content": string(b), "bytes": len(b), "truncated": truncated}, nil
}

func (n *nativeLocal) readMany(args map[string]interface{}) (interface{}, error) {
	raw, ok := args["paths"].([]interface{})
	if !ok || len(raw) == 0 || len(raw) > 20 {
		return nil, errors.New("paths must contain 1 to 20 items")
	}
	out := make([]interface{}, 0, len(raw))
	for _, item := range raw {
		path, ok := item.(string)
		if !ok {
			return nil, errors.New("every path must be a string")
		}
		v, err := n.readFile(map[string]interface{}{"path": path, "max_bytes": float64(256 << 10)})
		if err != nil {
			out = append(out, map[string]interface{}{"path": path, "error": err.Error()})
		} else {
			out = append(out, v)
		}
	}
	return out, nil
}

func (n *nativeLocal) writeFile(args map[string]interface{}) (interface{}, error) {
	path, err := n.resolve(argString(args, "path"), true)
	if err != nil {
		return nil, err
	}
	content, ok := args["content"].(string)
	if !ok {
		return nil, errors.New("content must be a string")
	}
	if len(content) > nativeMaxFileBytes {
		return nil, fmt.Errorf("content exceeds %d bytes", nativeMaxFileBytes)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := n.ownPathChain(filepath.Dir(path)); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".matrix-write-*")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o644); err == nil {
		_, err = io.WriteString(tmp, content)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmpName, path)
	}
	if err != nil {
		return nil, err
	}
	if err := n.ownPath(path); err != nil {
		return nil, err
	}
	return map[string]interface{}{"path": path, "bytes": len(content)}, nil
}

func (n *nativeLocal) editFile(args map[string]interface{}) (interface{}, error) {
	path, err := n.resolve(argString(args, "path"), true)
	if err != nil {
		return nil, err
	}
	oldText, ok := args["old_text"].(string)
	if !ok || oldText == "" {
		return nil, errors.New("old_text must be a non-empty string")
	}
	newText, ok := args["new_text"].(string)
	if !ok {
		return nil, errors.New("new_text must be a string")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) > nativeMaxFileBytes {
		return nil, errors.New("file exceeds edit limit")
	}
	count := bytes.Count(b, []byte(oldText))
	if count == 0 {
		return nil, errors.New("old_text was not found")
	}
	all := argBool(args, "replace_all", false)
	if !all && count != 1 {
		return nil, fmt.Errorf("old_text matched %d times; provide a unique anchor or set replace_all", count)
	}
	limit := 1
	if all {
		limit = -1
	}
	replaced := bytes.Replace(b, []byte(oldText), []byte(newText), limit)
	_, err = n.writeFile(map[string]interface{}{"path": path, "content": string(replaced)})
	if err != nil {
		return nil, err
	}
	changed := 1
	if all {
		changed = count
	}
	return map[string]interface{}{"path": path, "replacements": changed, "bytes": len(replaced)}, nil
}

type preparedPatch struct {
	path      string
	content   []byte
	existed   bool
	mode      os.FileMode
	stagePath string
	backup    string
}

func (n *nativeLocal) patchFiles(args map[string]interface{}) (interface{}, error) {
	raw, ok := args["changes"].([]interface{})
	if !ok || len(raw) == 0 || len(raw) > nativeMaxPatchFiles {
		return nil, fmt.Errorf("changes must contain 1 to %d items", nativeMaxPatchFiles)
	}
	prepared := make([]preparedPatch, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for index, item := range raw {
		change, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("change %d must be an object", index)
		}
		path, err := n.resolve(argString(change, "path"), true)
		if err != nil {
			return nil, fmt.Errorf("change %d: %w", index, err)
		}
		if _, duplicate := seen[path]; duplicate {
			return nil, fmt.Errorf("change %d repeats path %q", index, path)
		}
		seen[path] = struct{}{}
		current, readErr := os.ReadFile(path)
		existed := readErr == nil
		if readErr != nil && !os.IsNotExist(readErr) {
			return nil, fmt.Errorf("change %d: %w", index, readErr)
		}
		expected := strings.ToLower(argString(change, "expected_sha256"))
		if expected != "" {
			if expected == "missing" {
				if existed {
					return nil, fmt.Errorf("change %d: expected %q to be missing", index, path)
				}
			} else {
				actual := fmt.Sprintf("%x", sha256.Sum256(current))
				if !existed || actual != expected {
					return nil, fmt.Errorf("change %d: sha256 precondition failed for %q", index, path)
				}
			}
		}
		content, hasContent := change["content"].(string)
		oldText, hasOld := change["old_text"].(string)
		newText, hasNew := change["new_text"].(string)
		if hasContent == (hasOld || hasNew) {
			return nil, fmt.Errorf("change %d: provide either content or old_text with new_text", index)
		}
		var next []byte
		if hasContent {
			next = []byte(content)
		} else {
			if !hasOld || oldText == "" || !hasNew || !existed {
				return nil, fmt.Errorf("change %d: anchored replacement requires an existing file, non-empty old_text, and new_text", index)
			}
			count := bytes.Count(current, []byte(oldText))
			if count == 0 {
				return nil, fmt.Errorf("change %d: old_text was not found", index)
			}
			all := argBool(change, "replace_all", false)
			if !all && count != 1 {
				return nil, fmt.Errorf("change %d: old_text matched %d times", index, count)
			}
			limit := 1
			if all {
				limit = -1
			}
			next = bytes.Replace(current, []byte(oldText), []byte(newText), limit)
		}
		if len(next) > nativeMaxFileBytes {
			return nil, fmt.Errorf("change %d exceeds %d bytes", index, nativeMaxFileBytes)
		}
		mode := os.FileMode(0o644)
		if existed {
			if info, statErr := os.Stat(path); statErr == nil {
				mode = info.Mode().Perm()
			}
		}
		prepared = append(prepared, preparedPatch{path: path, content: next, existed: existed, mode: mode})
	}

	for index := range prepared {
		change := &prepared[index]
		if err := os.MkdirAll(filepath.Dir(change.path), 0o755); err != nil {
			cleanupPreparedPatches(prepared)
			return nil, err
		}
		if err := n.ownPathChain(filepath.Dir(change.path)); err != nil {
			cleanupPreparedPatches(prepared)
			return nil, err
		}
		stage, err := os.CreateTemp(filepath.Dir(change.path), ".matrix-patch-stage-*")
		if err != nil {
			cleanupPreparedPatches(prepared)
			return nil, err
		}
		change.stagePath = stage.Name()
		if err = stage.Chmod(change.mode); err == nil {
			_, err = stage.Write(change.content)
		}
		if syncErr := stage.Sync(); err == nil {
			err = syncErr
		}
		if closeErr := stage.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = n.ownPath(change.stagePath)
		}
		if err != nil {
			cleanupPreparedPatches(prepared)
			return nil, err
		}
	}

	committed := 0
	for index := range prepared {
		change := &prepared[index]
		if change.existed {
			change.backup = change.path + ".matrix-patch-backup"
			if _, err := os.Lstat(change.backup); err == nil {
				rollbackPreparedPatches(prepared, committed)
				return nil, fmt.Errorf("patch backup already exists for %q", change.path)
			}
			if err := os.Rename(change.path, change.backup); err != nil {
				rollbackPreparedPatches(prepared, committed)
				return nil, err
			}
		}
		if err := os.Rename(change.stagePath, change.path); err != nil {
			if change.existed {
				_ = os.Rename(change.backup, change.path)
				change.backup = ""
			}
			rollbackPreparedPatches(prepared, committed)
			return nil, err
		}
		change.stagePath = ""
		committed++
	}
	for index := range prepared {
		if prepared[index].backup != "" {
			_ = os.Remove(prepared[index].backup)
		}
	}
	files := make([]map[string]interface{}, 0, len(prepared))
	for _, change := range prepared {
		files = append(files, map[string]interface{}{
			"path": change.path, "bytes": len(change.content),
			"sha256": fmt.Sprintf("%x", sha256.Sum256(change.content)),
		})
	}
	digest, _ := json.Marshal(files)
	transaction := sha256.Sum256(digest)
	return map[string]interface{}{
		"applied": true, "transaction_id": fmt.Sprintf("%x", transaction[:12]), "files": files,
	}, nil
}

func cleanupPreparedPatches(prepared []preparedPatch) {
	for _, change := range prepared {
		if change.stagePath != "" {
			_ = os.Remove(change.stagePath)
		}
	}
}

func rollbackPreparedPatches(prepared []preparedPatch, committed int) {
	for index := committed - 1; index >= 0; index-- {
		change := &prepared[index]
		_ = os.Remove(change.path)
		if change.backup != "" {
			_ = os.Rename(change.backup, change.path)
		}
	}
	cleanupPreparedPatches(prepared)
}

func (n *nativeLocal) createDir(args map[string]interface{}) (interface{}, error) {
	path, err := n.resolve(argString(args, "path"), true)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, err
	}
	if err := n.ownPathChain(path); err != nil {
		return nil, err
	}
	return map[string]interface{}{"path": path}, nil
}

func (n *nativeLocal) listDir(args map[string]interface{}) (interface{}, error) {
	path, err := n.resolve(argString(args, "path"), false)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]interface{}, 0, len(entries))
	for _, entry := range entries {
		row := map[string]interface{}{"name": entry.Name(), "directory": entry.IsDir()}
		if info, e := entry.Info(); e == nil {
			row["size"] = info.Size()
			row["modified"] = info.ModTime().UTC().Format(time.RFC3339)
		}
		out = append(out, row)
	}
	return map[string]interface{}{"path": path, "entries": out}, nil
}

func (n *nativeLocal) tree(args map[string]interface{}) (interface{}, error) {
	root, err := n.resolve(argString(args, "path"), false)
	if err != nil {
		return nil, err
	}
	depth := argInt(args, "max_depth", 4)
	if depth < 1 {
		depth = 1
	}
	if depth > 8 {
		depth = 8
	}
	rows := make([]map[string]interface{}, 0, 128)
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}
		level := len(strings.Split(filepath.ToSlash(rel), "/"))
		if level > depth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if len(rows) >= 1000 {
			return errors.New("tree result limit reached")
		}
		rows = append(rows, map[string]interface{}{"path": filepath.ToSlash(rel), "directory": d.IsDir()})
		return nil
	})
	if err != nil && err.Error() != "tree result limit reached" {
		return nil, err
	}
	return map[string]interface{}{"root": root, "entries": rows, "truncated": len(rows) >= 1000}, nil
}

func (n *nativeLocal) moveFile(args map[string]interface{}) (interface{}, error) {
	src, err := n.resolve(argString(args, "source"), true)
	if err != nil {
		return nil, err
	}
	dst, err := n.resolve(argString(args, "destination"), true)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		return nil, errors.New("destination already exists")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, err
	}
	if err := n.ownPathChain(filepath.Dir(dst)); err != nil {
		return nil, err
	}
	if err := os.Rename(src, dst); err != nil {
		return nil, err
	}
	return map[string]interface{}{"source": src, "destination": dst}, nil
}

func (n *nativeLocal) ownPath(path string) error {
	identity, configured, err := tool.AgentIdentityFromEnv()
	if err != nil || !configured || os.Geteuid() != 0 {
		return err
	}
	if !within(n.root, path) {
		return errors.New("ownership target is outside workspace")
	}
	return os.Lchown(path, int(identity.UID), int(identity.GID))
}

func (n *nativeLocal) ownPathChain(path string) error {
	for current := filepath.Clean(path); within(n.root, current) && current != n.root; current = filepath.Dir(current) {
		if err := n.ownPath(current); err != nil {
			return err
		}
	}
	return nil
}

func (n *nativeLocal) searchFiles(args map[string]interface{}) (interface{}, error) {
	root, err := n.resolve(argString(args, "path"), false)
	if err != nil {
		return nil, err
	}
	query := strings.ToLower(argString(args, "query"))
	if query == "" {
		return nil, errors.New("query is required")
	}
	max := argInt(args, "max_results", 50)
	if max < 1 {
		max = 1
	}
	if max > 200 {
		max = 200
	}
	rows := make([]map[string]interface{}, 0, max)
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || len(rows) >= max {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if strings.Contains(strings.ToLower(filepath.ToSlash(rel)), query) {
			rows = append(rows, map[string]interface{}{"path": filepath.ToSlash(rel), "match": "name"})
			return nil
		}
		info, e := d.Info()
		if e != nil || info.Size() > 256<<10 {
			return nil
		}
		b, e := os.ReadFile(path)
		if e == nil && !bytes.Contains(b, []byte{0}) && strings.Contains(strings.ToLower(string(b)), query) {
			rows = append(rows, map[string]interface{}{"path": filepath.ToSlash(rel), "match": "content"})
		}
		return nil
	})
	return map[string]interface{}{"root": root, "results": rows, "truncated": len(rows) >= max}, nil
}

func (n *nativeLocal) fileInfo(args map[string]interface{}) (interface{}, error) {
	path, err := n.resolve(argString(args, "path"), false)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"path": path, "name": info.Name(), "size": info.Size(), "mode": info.Mode().String(), "directory": info.IsDir(), "modified": info.ModTime().UTC().Format(time.RFC3339)}, nil
}

func (n *nativeLocal) command(command, cwd, logPath string) (*exec.Cmd, *os.File, error) {
	if strings.TrimSpace(command) == "" {
		return nil, nil, errors.New("command is required")
	}
	if containsInlineCredential(command) {
		return nil, nil, errors.New("command contains a likely inline credential; use the owning typed integration or an approved secret broker")
	}
	dir, err := n.resolve(cwd, true)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, nil, errors.New("cwd must be a workspace directory")
	}
	cmd := exec.Command("/bin/bash", "-c", command)
	cmd.Dir = dir
	cmd.Env = nativeEnvironment(dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := tool.ConfigureAgentCommand(cmd); err != nil {
		return nil, nil, err
	}
	var log *os.File
	if logPath != "" {
		log, err = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, nil, err
		}
		cmd.Stdout, cmd.Stderr = log, log
	}
	return cmd, log, nil
}

func containsInlineCredential(command string) bool {
	for _, pattern := range inlineCredentialPatterns {
		if pattern.MatchString(command) {
			return true
		}
	}
	return false
}

func nativeEnvironment(cwd string) []string {
	values := map[string]string{"HOME": cwd, "USER": "matrix-agent", "LOGNAME": "matrix-agent", "PATH": "/opt/matrix/coding-bin:/usr/local/bin:/usr/bin:/bin", "LANG": "C.UTF-8", "LC_ALL": "C.UTF-8", "PWD": cwd, "TMPDIR": "/tmp"}
	for _, key := range []string{"TERM", "TZ", "MATRIX_USER_ID", "CODY_USER_ID", "PAXC_API", "PAXC_TOKEN"} {
		if value := os.Getenv(key); value != "" {
			values[key] = value
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

func (n *nativeLocal) shell(ctx context.Context, args map[string]interface{}) (interface{}, error) {
	timeout := argInt(args, "timeout_seconds", 60)
	if timeout < 1 {
		timeout = 1
	}
	if timeout > 300 {
		timeout = 300
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	cmd, _, err := n.command(argString(args, "command"), argString(args, "cwd"), "")
	if err != nil {
		return nil, err
	}
	outputDir := filepath.Join(n.stateDir, "shell-output")
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return nil, err
	}
	seed := fmt.Sprintf("%d\x00%d\x00%s\x00%s", time.Now().UnixNano(), os.Getpid(), cmd.Dir, argString(args, "command"))
	digest := sha256.Sum256([]byte(seed))
	outputID := fmt.Sprintf("%x", digest[:12])
	stdoutPath := filepath.Join(outputDir, outputID+".stdout")
	stderrPath := filepath.Join(outputDir, outputID+".stderr")
	stdoutFile, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	stderrFile, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		stdoutFile.Close()
		_ = os.Remove(stdoutPath)
		return nil, err
	}
	stdout := &retainedOutputWriter{file: stdoutFile, limit: nativeMaxRetainedOutput}
	stderr := &retainedOutputWriter{file: stderrFile, limit: nativeMaxRetainedOutput}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		stdoutFile.Close()
		stderrFile.Close()
		_ = os.Remove(stdoutPath)
		_ = os.Remove(stderrPath)
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	timedOut := false
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		timedOut = true
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		waitErr = <-done
	}
	exit := 0
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			exit = ee.ExitCode()
		} else {
			exit = -1
		}
	}
	if err := stdoutFile.Close(); err != nil {
		return nil, err
	}
	if err := stderrFile.Close(); err != nil {
		return nil, err
	}
	stdoutPreview, err := readOutputPreview(stdoutPath, nativeMaxStreamPreview)
	if err != nil {
		return nil, err
	}
	stderrPreview, err := readOutputPreview(stderrPath, nativeMaxStreamPreview)
	if err != nil {
		return nil, err
	}
	truncated := stdout.total > int64(len(stdoutPreview)) || stderr.total > int64(len(stderrPreview))
	result := map[string]interface{}{
		"command": argString(args, "command"), "cwd": cmd.Dir,
		"exit_code": exit, "timed_out": timedOut, "truncated": truncated,
		"stdout": map[string]interface{}{"text": string(stdoutPreview), "bytes": stdout.total, "inline_bytes": len(stdoutPreview), "capture_truncated": stdout.dropped},
		"stderr": map[string]interface{}{"text": string(stderrPreview), "bytes": stderr.total, "inline_bytes": len(stderrPreview), "capture_truncated": stderr.dropped},
	}
	if truncated {
		meta := shellOutputMeta{StdoutBytes: stdout.total, StderrBytes: stderr.total, StdoutCaptureTruncated: stdout.dropped, StderrCaptureTruncated: stderr.dropped, CreatedAt: time.Now().UTC()}
		encoded, _ := json.Marshal(meta)
		if err := os.WriteFile(filepath.Join(outputDir, outputID+".json"), encoded, 0o600); err != nil {
			return nil, err
		}
		result["output_id"] = outputID
		result["retrieval"] = map[string]interface{}{"tool": nativeShellOutput, "streams": []string{"stdout", "stderr"}}
	} else {
		_ = os.Remove(stdoutPath)
		_ = os.Remove(stderrPath)
	}
	return result, nil
}

type retainedOutputWriter struct {
	file    *os.File
	limit   int64
	written int64
	total   int64
	dropped bool
}

func (writer *retainedOutputWriter) Write(p []byte) (int, error) {
	original := len(p)
	writer.total += int64(original)
	remaining := writer.limit - writer.written
	if remaining <= 0 {
		writer.dropped = true
		return original, nil
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
		writer.dropped = true
	}
	written, err := writer.file.Write(p)
	writer.written += int64(written)
	if err != nil {
		return written, err
	}
	return original, nil
}

type shellOutputMeta struct {
	StdoutBytes            int64     `json:"stdout_bytes"`
	StderrBytes            int64     `json:"stderr_bytes"`
	StdoutCaptureTruncated bool      `json:"stdout_capture_truncated"`
	StderrCaptureTruncated bool      `json:"stderr_capture_truncated"`
	CreatedAt              time.Time `json:"created_at"`
}

func readOutputPreview(path string, limit int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, int64(limit)))
}

func (n *nativeLocal) shellOutput(args map[string]interface{}) (interface{}, error) {
	id := argString(args, "output_id")
	if len(id) != 24 {
		return nil, errors.New("output_id is invalid")
	}
	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return nil, errors.New("output_id is invalid")
		}
	}
	stream := argString(args, "stream")
	if stream != "stdout" && stream != "stderr" {
		return nil, errors.New("stream must be stdout or stderr")
	}
	offset := argInt(args, "offset", 0)
	if offset < 0 {
		return nil, errors.New("offset cannot be negative")
	}
	maxBytes := argInt(args, "max_bytes", nativeMaxOutput)
	if maxBytes < 1 || maxBytes > nativeShellOutputMaxRead {
		return nil, fmt.Errorf("max_bytes must be from 1 to %d", nativeShellOutputMaxRead)
	}
	outputDir := filepath.Join(n.stateDir, "shell-output")
	metaBytes, err := os.ReadFile(filepath.Join(outputDir, id+".json"))
	if err != nil {
		return nil, errors.New("shell output is unavailable or expired")
	}
	var meta shellOutputMeta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return nil, errors.New("shell output metadata is invalid")
	}
	file, err := os.Open(filepath.Join(outputDir, id+"."+stream))
	if err != nil {
		return nil, errors.New("shell output stream is unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if int64(offset) > info.Size() {
		return nil, errors.New("offset exceeds retained output")
	}
	if _, err := file.Seek(int64(offset), io.SeekStart); err != nil {
		return nil, err
	}
	chunk, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)))
	if err != nil {
		return nil, err
	}
	next := int64(offset + len(chunk))
	captureTruncated := meta.StdoutCaptureTruncated
	total := meta.StdoutBytes
	if stream == "stderr" {
		captureTruncated = meta.StderrCaptureTruncated
		total = meta.StderrBytes
	}
	return map[string]interface{}{
		"output_id": id, "stream": stream, "offset": offset, "content": string(chunk),
		"next_offset": next, "eof": next >= info.Size(), "retained_bytes": info.Size(),
		"total_bytes": total, "capture_truncated": captureTruncated,
	}, nil
}

type cappedBuffer struct {
	b         bytes.Buffer
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	if b.b.Len() < nativeMaxOutput {
		remaining := nativeMaxOutput - b.b.Len()
		if len(p) > remaining {
			p = p[:remaining]
			b.truncated = true
		}
		_, _ = b.b.Write(p)
	} else {
		b.truncated = true
	}
	return original, nil
}
func (b *cappedBuffer) String() string { return b.b.String() }

func validServiceName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !(r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func (n *nativeLocal) serviceStart(args map[string]interface{}) (interface{}, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	name := argString(args, "name")
	if !validServiceName(name) {
		return nil, errors.New("name must use lowercase letters, digits, dash, or underscore")
	}
	if existing := n.services[name]; existing != nil {
		if err := n.stopLocked(existing); err != nil {
			return nil, err
		}
	} else if len(n.services) >= 32 {
		return nil, errors.New("durable service limit reached (32)")
	}
	cwd, err := n.resolve(argString(args, "cwd"), true)
	if err != nil {
		return nil, err
	}
	svc := &nativeService{Name: name, Command: argString(args, "command"), Cwd: cwd, Autostart: argBool(args, "autostart", true), LogPath: filepath.Join(n.stateDir, name+".log")}
	if err := n.startLocked(svc); err != nil {
		return nil, err
	}
	n.services[name] = svc
	if err := n.saveLocked(); err != nil {
		_ = n.stopLocked(svc)
		return nil, err
	}
	return n.serviceView(svc), nil
}

func (n *nativeLocal) startLocked(svc *nativeService) error {
	cmd, log, err := n.command(svc.Command, svc.Cwd, svc.LogPath)
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		log.Close()
		return err
	}
	_ = log.Close()
	svc.cmd = cmd
	svc.PID = cmd.Process.Pid
	svc.StartTicks, _ = processStartTicks(svc.PID)
	svc.StartedAt = time.Now().UTC().Format(time.RFC3339)
	go func() { _ = cmd.Wait() }()
	time.Sleep(75 * time.Millisecond)
	if !n.running(svc) {
		return fmt.Errorf("service %q exited during startup; inspect %s", svc.Name, svc.LogPath)
	}
	return nil
}

func (n *nativeLocal) serviceList() interface{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]interface{}, 0, len(n.services))
	names := make([]string, 0, len(n.services))
	for name := range n.services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, n.serviceView(n.services[name]))
	}
	return out
}

func (n *nativeLocal) serviceView(svc *nativeService) map[string]interface{} {
	return map[string]interface{}{"name": svc.Name, "command": svc.Command, "cwd": svc.Cwd, "autostart": svc.Autostart, "pid": svc.PID, "running": n.running(svc), "log_path": svc.LogPath, "started_at": svc.StartedAt}
}

func (n *nativeLocal) serviceLogs(args map[string]interface{}) (interface{}, error) {
	n.mu.Lock()
	svc := n.services[argString(args, "name")]
	n.mu.Unlock()
	if svc == nil {
		return nil, errors.New("service not found")
	}
	b, err := os.ReadFile(svc.LogPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	lines := strings.Split(string(b), "\n")
	limit := argInt(args, "lines", 200)
	if limit < 1 {
		limit = 1
	}
	if limit > 1000 {
		limit = 1000
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return map[string]interface{}{"name": svc.Name, "running": n.running(svc), "output": strings.Join(lines, "\n")}, nil
}

func (n *nativeLocal) serviceStop(args map[string]interface{}) (interface{}, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	svc := n.services[argString(args, "name")]
	if svc == nil {
		return nil, errors.New("service not found")
	}
	if err := n.stopLocked(svc); err != nil {
		return nil, err
	}
	svc.Autostart = false
	if err := n.saveLocked(); err != nil {
		return nil, err
	}
	return n.serviceView(svc), nil
}

func (n *nativeLocal) stopLocked(svc *nativeService) error {
	if !n.running(svc) {
		svc.PID = 0
		svc.StartTicks = 0
		return nil
	}
	_ = syscall.Kill(-svc.PID, syscall.SIGTERM)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !n.running(svc) {
			svc.PID = 0
			svc.StartTicks = 0
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(-svc.PID, syscall.SIGKILL)
	svc.PID = 0
	svc.StartTicks = 0
	return nil
}

func (n *nativeLocal) serviceRestart(args map[string]interface{}) (interface{}, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	svc := n.services[argString(args, "name")]
	if svc == nil {
		return nil, errors.New("service not found")
	}
	_ = n.stopLocked(svc)
	svc.Autostart = true
	if err := n.startLocked(svc); err != nil {
		return nil, err
	}
	if err := n.saveLocked(); err != nil {
		return nil, err
	}
	return n.serviceView(svc), nil
}

func (n *nativeLocal) running(svc *nativeService) bool {
	if svc == nil || svc.PID <= 0 {
		return false
	}
	ticks, err := processStartTicks(svc.PID)
	return err == nil && ticks == svc.StartTicks
}

func processStartTicks(pid int) (uint64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	text := string(b)
	end := strings.LastIndex(text, ") ")
	if end < 0 {
		return 0, errors.New("invalid proc stat")
	}
	fields := strings.Fields(text[end+2:])
	if len(fields) < 20 {
		return 0, errors.New("short proc stat")
	}
	return strconv.ParseUint(fields[19], 10, 64)
}

func (n *nativeLocal) registryPath() string { return filepath.Join(n.stateDir, "services.json") }
func (n *nativeLocal) saveLocked() error {
	values := make([]*nativeService, 0, len(n.services))
	for _, svc := range n.services {
		values = append(values, svc)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Name < values[j].Name })
	b, err := json.MarshalIndent(nativeRegistry{Services: values}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(n.stateDir, ".services-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	_ = tmp.Chmod(0o600)
	if _, err = tmp.Write(b); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, n.registryPath())
	}
	return err
}

func (n *nativeLocal) loadAndRestore() error {
	b, err := os.ReadFile(n.registryPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var reg nativeRegistry
	if err := json.Unmarshal(b, &reg); err != nil {
		return fmt.Errorf("native service registry: %w", err)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, svc := range reg.Services {
		if svc == nil || !validServiceName(svc.Name) {
			continue
		}
		n.services[svc.Name] = svc
		if !n.running(svc) && svc.Autostart {
			svc.PID = 0
			svc.StartTicks = 0
			if err := n.startLocked(svc); err != nil {
				svc.Autostart = false
			}
		}
	}
	return n.saveLocked()
}

func (n *nativeLocal) git(ctx context.Context, name string, args map[string]interface{}) (interface{}, error) {
	cwd, err := n.resolve(argString(args, "cwd"), false)
	if err != nil {
		return nil, err
	}
	var argv []string
	switch name {
	case nativeGitStatus:
		argv = []string{"status", "--short", "--branch"}
	case nativeGitDiff:
		argv = []string{"diff", "--no-color"}
		if argBool(args, "staged", false) {
			argv = append(argv, "--cached")
		}
		if ref := argString(args, "ref"); ref != "" {
			if strings.HasPrefix(ref, "-") {
				return nil, errors.New("ref may not start with dash")
			}
			argv = append(argv, ref)
		}
	case nativeGitLog:
		limit := argInt(args, "limit", 20)
		if limit < 1 {
			limit = 1
		}
		if limit > 100 {
			limit = 100
		}
		argv = []string{"log", "--no-color", "--date=iso-strict", fmt.Sprintf("--max-count=%d", limit), "--pretty=format:%H%x09%ad%x09%an%x09%s"}
	case nativeGitShow:
		rev := argString(args, "revision")
		if rev == "" {
			rev = "HEAD"
		}
		if strings.HasPrefix(rev, "-") {
			return nil, errors.New("revision may not start with dash")
		}
		argv = []string{"show", "--no-color", "--stat", "--oneline", rev}
	case nativeGitBranch:
		argv = []string{"branch", "--all", "--verbose", "--no-color"}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, n.gitPath, argv...)
	cmd.Dir = cwd
	cmd.Env = nativeEnvironment(cwd)
	if err := tool.ConfigureAgentCommand(cmd); err != nil {
		return nil, err
	}
	var out cappedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	runErr := cmd.Run()
	exit := 0
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			exit = ee.ExitCode()
		} else {
			return nil, runErr
		}
	}
	return map[string]interface{}{"cwd": cwd, "exit_code": exit, "output": out.String(), "truncated": out.truncated}, nil
}
