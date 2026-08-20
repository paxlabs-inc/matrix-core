// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"centra/agents/neo/internal/capabilityhub"
	neotools "centra/agents/neo/internal/tools"
)

func TestCapabilityRoutesImportVerifyActivateAndAudit(t *testing.T) {
	store, err := capabilityhub.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := &Server{engine: &Engine{capabilities: store, capabilityLibrary: realSkillCorpus(t)}}
	catalog := requestCapability(t, server, http.MethodGet, "/capabilities/catalog?q=brainstorming", "", http.StatusOK)
	var discovery struct {
		Capabilities []capabilityhub.LibraryItem `json:"capabilities"`
	}
	if err := json.Unmarshal(catalog.Body.Bytes(), &discovery); err != nil {
		t.Fatal(err)
	}
	if len(discovery.Capabilities) != 1 || discovery.Capabilities[0].Slug != "brainstorming" {
		t.Fatalf("catalog response: %+v", discovery.Capabilities)
	}

	imported := requestCapability(t, server, http.MethodPost, "/capabilities/import", `{"source_type":"library","source":"brainstorming"}`, http.StatusCreated)
	var capability capabilityhub.Capability
	if err := json.Unmarshal(imported.Body.Bytes(), &capability); err != nil {
		t.Fatal(err)
	}
	if capability.Slug != "brainstorming" || capability.State != capabilityhub.StateQuarantine {
		t.Fatalf("import response: %+v", capability)
	}

	verified := requestCapability(t, server, http.MethodPost, "/capabilities/brainstorming/verify", `{"version":"0.1.0"}`, http.StatusOK)
	if err := json.Unmarshal(verified.Body.Bytes(), &capability); err != nil {
		t.Fatal(err)
	}
	if capability.State != capabilityhub.StateVerified {
		t.Fatalf("verify state = %s", capability.State)
	}
	activated := requestCapability(t, server, http.MethodPost, "/capabilities/brainstorming/activate", `{"version":"0.1.0"}`, http.StatusOK)
	if err := json.Unmarshal(activated.Body.Bytes(), &capability); err != nil {
		t.Fatal(err)
	}
	if capability.State != capabilityhub.StateActive {
		t.Fatalf("activate state = %s", capability.State)
	}

	listed := requestCapability(t, server, http.MethodGet, "/capabilities?q=brain", "", http.StatusOK)
	var list struct {
		Capabilities []capabilityhub.Capability `json:"capabilities"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Capabilities) != 1 || list.Capabilities[0].State != capabilityhub.StateActive {
		t.Fatalf("list response: %+v", list.Capabilities)
	}
	audit := requestCapability(t, server, http.MethodGet, "/capabilities/brainstorming/audit", "", http.StatusOK)
	var history struct {
		Events []capabilityhub.AuditEvent `json:"events"`
	}
	if err := json.Unmarshal(audit.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Events) < 3 || history.Events[0].Action != "activate" {
		t.Fatalf("audit response: %+v", history.Events)
	}
}

func TestCapabilityRoutesFailIndependentlyWhenDisabled(t *testing.T) {
	server := &Server{engine: &Engine{}}
	response := requestCapability(t, server, http.MethodGet, "/capabilities", "", http.StatusServiceUnavailable)
	if !bytes.Contains(response.Body.Bytes(), []byte("disabled")) {
		t.Fatalf("response = %s", response.Body.String())
	}
}

func TestCapabilityRoutesRejectUnknownJSONAndMissingVersion(t *testing.T) {
	store, err := capabilityhub.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := &Server{engine: &Engine{capabilities: store, capabilityLibrary: realSkillCorpus(t)}}
	requestCapability(t, server, http.MethodPost, "/capabilities/import", `{"source_type":"library","source":"brainstorming","unknown":true}`, http.StatusBadRequest)
	requestCapability(t, server, http.MethodDelete, "/capabilities/brainstorming", "", http.StatusBadRequest)
}

func TestCapabilityRouteVerificationUsesRealNativeToolManager(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(manifestPath, []byte(`{"schema_version":1,"agent":"capability-verifier","servers":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := neotools.Spawn(ctx, neotools.Options{
		ManifestPath: manifestPath, NativeRoot: workspace, NativeStateDir: t.TempDir(), SpawnTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	packageDir := filepath.Join(workspace, "skill")
	if err := copyDirectoryForCapabilityTest(filepath.Join(realSkillCorpus(t), "brainstorming"), packageDir); err != nil {
		t.Fatal(err)
	}
	manifestFile := filepath.Join(packageDir, "SKILL.mtx")
	manifest, err := os.ReadFile(manifestFile)
	if err != nil {
		t.Fatal(err)
	}
	toolURI := "matrix://tool/mcp/native/read_text_file@0.1.0"
	manifest = bytes.Replace(manifest, []byte("§TOOLS\nnone"), []byte("§TOOLS\n"+toolURI), 1)
	if err := os.WriteFile(manifestFile, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	suite := `{"schema_version":1,"cases":[{"name":"read validated manifest","tool":"` + toolURI + `","arguments":{"path":"skill/SKILL.mtx"},"expect_contains":"id=brainstorming"}]}`
	if err := os.WriteFile(filepath.Join(packageDir, "CAPABILITY_TESTS.json"), []byte(suite), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := capabilityhub.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	capability, err := store.ImportDirectory(ctx, capabilityhub.ImportRequest{SourceDir: packageDir, SourceType: capabilityhub.SourceAuthored, SourceRef: "proposal/native-verification"})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{engine: &Engine{capabilities: store, tools: manager}}
	requestCapability(t, server, http.MethodPost, "/capabilities/brainstorming/grant", `{"version":"`+capability.Version+`","permissions":["`+toolURI+`"]}`, http.StatusOK)
	verified := requestCapability(t, server, http.MethodPost, "/capabilities/brainstorming/verify", `{"version":"`+capability.Version+`"}`, http.StatusOK)
	if err := json.Unmarshal(verified.Body.Bytes(), &capability); err != nil {
		t.Fatal(err)
	}
	if capability.State != capabilityhub.StateVerified {
		t.Fatalf("verified state = %s", capability.State)
	}
}

func TestCapabilityToolInventoryChangesOnlyOnNextSchemaSnapshot(t *testing.T) {
	ctx := context.Background()
	store, err := capabilityhub.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manifestPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(manifestPath, []byte(`{"schema_version":1,"agent":"snapshot-test","servers":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := neotools.Spawn(ctx, neotools.Options{ManifestPath: manifestPath, NativeRoot: t.TempDir(), NativeStateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	first, err := store.ImportDirectory(ctx, capabilityhub.ImportRequest{SourceDir: filepath.Join(realSkillCorpus(t), "brainstorming"), SourceType: capabilityhub.SourceLibrary, SourceRef: "matrix-library/brainstorming"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Verify(ctx, first.Slug, first.Version, capabilityhub.Verification{}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Activate(ctx, first.Slug, first.Version); err != nil {
		t.Fatal(err)
	}
	engine := &Engine{capabilities: store, tools: manager}
	manager.SetCapabilityHub(engine.snapshotCapabilities, engine.createCapabilityCandidate)
	_ = manager.Schemas()
	assertSnapshotVersion(t, manager, "0.1.0")

	secondSource := filepath.Join(t.TempDir(), "skill")
	if err := copyDirectoryForCapabilityTest(filepath.Join(realSkillCorpus(t), "brainstorming"), secondSource); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(secondSource, "SKILL.mtx")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Replace(body, []byte("version=0.1.0"), []byte("version=0.2.0"), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := store.ImportDirectory(ctx, capabilityhub.ImportRequest{SourceDir: secondSource, SourceType: capabilityhub.SourceAuthored, SourceRef: "proposal/snapshot-v2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Verify(ctx, second.Slug, second.Version, capabilityhub.Verification{}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Activate(ctx, second.Slug, second.Version); err != nil {
		t.Fatal(err)
	}

	assertSnapshotVersion(t, manager, "0.1.0")
	_ = manager.VerificationSchemas()
	assertSnapshotVersion(t, manager, "0.1.0")
	_ = manager.Schemas()
	assertSnapshotVersion(t, manager, "0.2.0")
}

func TestNeoCapabilityCandidateToolCanOnlyCreateQuarantine(t *testing.T) {
	ctx := context.Background()
	store, err := capabilityhub.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manifestPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(manifestPath, []byte(`{"schema_version":1,"agent":"candidate-test","servers":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := neotools.Spawn(ctx, neotools.Options{ManifestPath: manifestPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	engine := &Engine{capabilities: store, tools: manager}
	manager.SetCapabilityHub(engine.snapshotCapabilities, engine.createCapabilityCandidate)
	manifest, err := os.ReadFile(filepath.Join(realSkillCorpus(t), "brainstorming", "SKILL.mtx"))
	if err != nil {
		t.Fatal(err)
	}
	instructions, err := os.ReadFile(filepath.Join(realSkillCorpus(t), "brainstorming", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	result, isError, err := manager.Dispatch(ctx, neotools.CapabilityCandidateTool, map[string]any{
		"manifest": string(manifest), "instructions": string(instructions), "provenance": "conversation/test-candidate",
	})
	if err != nil || isError {
		t.Fatalf("candidate dispatch: isError=%v err=%v result=%s", isError, err, result)
	}
	versions, err := store.Versions(ctx, "brainstorming")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 || versions[0].State != capabilityhub.StateQuarantine || len(versions[0].Granted) != 0 {
		t.Fatalf("candidate escaped quarantine: %+v", versions)
	}
}

func assertSnapshotVersion(t *testing.T, manager *neotools.Manager, version string) {
	t.Helper()
	result, isError, err := manager.Dispatch(context.Background(), neotools.CapabilitySearchTool, map[string]any{"slug": "brainstorming"})
	if err != nil || isError {
		t.Fatalf("capability search failed: isError=%v err=%v result=%s", isError, err, result)
	}
	if !bytes.Contains([]byte(result), []byte(`"version":"`+version+`"`)) {
		t.Fatalf("snapshot result does not contain version %s: %s", version, result)
	}
}

func requestCapability(t *testing.T, server *Server, method, target, body string, status int) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	server.handleCapabilities(response, request)
	if response.Code != status {
		t.Fatalf("%s %s: status %d, want %d, body=%s", method, target, response.Code, status, response.Body.String())
	}
	return response
}

func realSkillCorpus(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime caller unavailable")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "skills"))
	if _, err := os.Stat(filepath.Join(root, "brainstorming", "SKILL.mtx")); err != nil {
		t.Fatalf("real skill corpus unavailable: %v", err)
	}
	return root
}

func copyDirectoryForCapabilityTest(source, destination string) error {
	return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o600)
	})
}
