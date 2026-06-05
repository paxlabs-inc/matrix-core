// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package canonical

import (
	"os"
	"testing"

	"matrix/mcl/mtx/parser"
)

func TestHashDeterministic(t *testing.T) {
	src := []byte(`§SKILL
id=test
version=1.0.0
§INPUTS
slot target: string
  required
§CORTEX
§TOOLS
none
§SUB_SKILLS
none
§PROCEDURE
§OUTPUTS
§FAILURE_MODES
`)
	file1, _ := parser.New(src).Parse()
	file2, _ := parser.New(src).Parse()

	h1 := Hash(file1)
	h2 := Hash(file2)

	if h1 != h2 {
		t.Errorf("non-deterministic: %s != %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("expected 64 hex chars, got %d", len(h1))
	}
}

func TestHashIgnoresComments(t *testing.T) {
	src1 := []byte("§SKILL\nid=test\n")
	src2 := []byte("# a comment\n§SKILL\n# another\nid=test\n")

	file1, _ := parser.New(src1).Parse()
	file2, _ := parser.New(src2).Parse()

	h1 := Hash(file1)
	h2 := Hash(file2)

	if h1 != h2 {
		t.Errorf("comment changed hash: %s != %s", h1, h2)
	}
}

func TestHashIgnoresBlankLines(t *testing.T) {
	src1 := []byte("§SKILL\nid=test\n")
	src2 := []byte("§SKILL\n\n\nid=test\n\n")

	file1, _ := parser.New(src1).Parse()
	file2, _ := parser.New(src2).Parse()

	h1 := Hash(file1)
	h2 := Hash(file2)

	if h1 != h2 {
		t.Errorf("blank lines changed hash: %s != %s", h1, h2)
	}
}

func TestHashIgnoresHashSection(t *testing.T) {
	src1 := []byte("§SKILL\nid=test\n")
	src2 := []byte("§SKILL\nid=test\n§HASH\nv=1\nalgo=sha256_ast\ndigest=abc123\n")

	file1, _ := parser.New(src1).Parse()
	file2, _ := parser.New(src2).Parse()

	h1 := Hash(file1)
	h2 := Hash(file2)

	if h1 != h2 {
		t.Errorf("HASH section changed hash: %s != %s", h1, h2)
	}
}

func TestHashDiffersOnContent(t *testing.T) {
	src1 := []byte("§SKILL\nid=test\n")
	src2 := []byte("§SKILL\nid=other\n")

	file1, _ := parser.New(src1).Parse()
	file2, _ := parser.New(src2).Parse()

	h1 := Hash(file1)
	h2 := Hash(file2)

	if h1 == h2 {
		t.Error("different content produced same hash")
	}
}

func TestBytesNonEmpty(t *testing.T) {
	src := []byte("§SKILL\nid=test\nversion=1.0.0\n")
	file, _ := parser.New(src).Parse()
	b := Bytes(file)
	if len(b) == 0 {
		t.Error("canonical bytes should not be empty")
	}
}

func TestHashPromptBlock(t *testing.T) {
	src := []byte(`§PROCEDURE
on verb=build
  prompt
    system="sys"
    user="usr"
  end
end
`)
	file, _ := parser.New(src).Parse()
	h := Hash(file)
	if len(h) != 64 {
		t.Errorf("expected 64 hex chars, got %d", len(h))
	}

	// Verify canonical bytes contain the prompt structure
	b := string(Bytes(file))
	if !containsStr(b, `system="sys"`) {
		t.Error("canonical bytes missing system role")
	}
	if !containsStr(b, `user="usr"`) {
		t.Error("canonical bytes missing user role")
	}
}

func TestHashResolveStmt(t *testing.T) {
	src := []byte(`§PROCEDURE
on verb=build
  resolve slot.target <- cortex.find(type=Fact,limit=5)
end
`)
	file, _ := parser.New(src).Parse()
	b := string(Bytes(file))
	if !containsStr(b, "resolve slot.target <- cortex.find(") {
		t.Errorf("canonical bytes missing resolve: %s", b)
	}
}

// ---- integration ----

func TestHashWritingPlansSKILL(t *testing.T) {
	src := readTestFile(t, "/root/matrix/skills/writing-plans/SKILL.mtx")
	file, _ := parser.New(src).Parse()
	h := Hash(file)

	if len(h) != 64 {
		t.Errorf("expected 64 hex chars, got %d", len(h))
	}

	// Hash should be stable across re-parse
	file2, _ := parser.New(src).Parse()
	h2 := Hash(file2)
	if h != h2 {
		t.Errorf("re-parse changed hash: %s != %s", h, h2)
	}
	t.Logf("SKILL.mtx canonical hash: %s", h)
}

func TestHashCoreVerbMtx(t *testing.T) {
	src := readTestFile(t, "/root/matrix/MCL/core/verb.mtx")
	file, _ := parser.New(src).Parse()
	h := Hash(file)
	if len(h) != 64 {
		t.Errorf("expected 64 hex chars, got %d", len(h))
	}
	t.Logf("verb.mtx canonical hash: %s", h)
}

func TestHashCorePipelineMtx(t *testing.T) {
	src := readTestFile(t, "/root/matrix/MCL/core/pipeline.mtx")
	file, _ := parser.New(src).Parse()
	h := Hash(file)
	if len(h) != 64 {
		t.Errorf("expected 64 hex chars, got %d", len(h))
	}
	t.Logf("pipeline.mtx canonical hash: %s", h)
}

// ---- helpers ----

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("test file not found: %s", path)
	}
	return data
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
