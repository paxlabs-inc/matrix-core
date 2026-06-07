package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/paxlabs-inc/tachyon-tools/internal/config"
	"github.com/paxlabs-inc/tachyon-tools/pkg/types"
)

// ozTokenSource imports the @openzeppelin/contracts/ corpus exactly as a real
// caller would, exercising the workdir's linked .oz/ + lib/ remappings.
const ozTokenSource = `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import "@openzeppelin/contracts/token/ERC20/ERC20.sol";

contract DemoToken is ERC20 {
    constructor() ERC20("Demo", "DEMO") {
        _mint(msg.sender, 1000 ether);
    }
}
`

// ozTokenTest imports forge-std/Test.sol to confirm the test remapping too.
const ozTokenTest = `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import "forge-std/Test.sol";
import "../src/DemoToken.sol";

contract DemoTokenTest is Test {
    DemoToken token;

    function setUp() public {
        token = new DemoToken();
    }

    function test_Supply() public {
        assertEq(token.totalSupply(), 1000 ether);
        assertEq(token.balanceOf(address(this)), 1000 ether);
    }
}
`

// boxRootForTest resolves the tachyon repo root from this test file's location.
func boxRootForTest() string {
	_, file, _, _ := runtime.Caller(0)
	// internal/engine/<file> -> repo root is two dirs up.
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// TestCompileAndTestFromUploadedSourcesWithOZ proves the shared-box convention:
// an uploaded contract importing @openzeppelin and a test importing forge-std
// both resolve against the box's linked corpus. Skips when forge or the corpus
// is unavailable.
func TestCompileAndTestFromUploadedSourcesWithOZ(t *testing.T) {
	if _, err := exec.LookPath("forge"); err != nil {
		t.Skip("forge not installed; skipping OZ uploaded-source test")
	}
	box := boxRootForTest()
	if _, err := os.Stat(filepath.Join(box, "contracts", "token", "ERC20", "ERC20.sol")); err != nil {
		t.Skip("@openzeppelin corpus not present at box root; skipping")
	}
	if _, err := os.Stat(filepath.Join(box, "lib", "forge-std", "src", "Test.sol")); err != nil {
		t.Skip("forge-std not present at box root; skipping")
	}

	e, err := New(config.Config{
		ProjectRoot:  box,
		ArtifactsDir: "artifacts",
		RegistryPath: filepath.Join(t.TempDir(), "registry.json"),
		ForgePath:    "forge",
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	sources := map[string]string{
		"src/DemoToken.sol":    ozTokenSource,
		"test/DemoToken.t.sol": ozTokenTest,
	}

	cEnv := e.Compile(context.Background(), types.CompileRequest{Sources: sources})
	if !cEnv.Ok {
		t.Fatalf("compile (OZ) failed: %+v", cEnv.Error)
	}

	tEnv := e.Test(context.Background(), types.TestRequest{Sources: sources})
	if !tEnv.Ok {
		t.Fatalf("test (forge-std) failed: %+v (data=%+v)", tEnv.Error, tEnv.Data)
	}
	if tEnv.Data.Passed < 1 || tEnv.Data.Failed != 0 {
		t.Fatalf("expected >=1 passing, 0 failing; got passed=%d failed=%d", tEnv.Data.Passed, tEnv.Data.Failed)
	}
}
