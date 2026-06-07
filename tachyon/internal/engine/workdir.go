package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/paxlabs-inc/tachyon-tools/pkg/types"
)

// maxSourceBytes bounds one uploaded source file (defensive against runaway
// payloads). Solidity files comfortably fit well under this.
const maxSourceBytes = 8 << 20 // 8 MiB

// defaultEVMVersion is the solc/EVM target used for uploaded-source compiles
// when the caller does not pin one. Deliberately conservative: Paxeer mainnet
// (chain 125) and many other EVM chains are PRE-Cancun, so solc 0.8.27's
// default ("cancun") emits the MCOPY opcode (0x5E) — string/dynamic-memory
// calls like name()/symbol() then revert on-chain with "invalid opcode: opcode
// 0x5e not defined". "shanghai" still emits PUSH0 (which Paxeer supports) but
// no Cancun-only opcodes, so it deploys and runs everywhere modern. Callers can
// override per-chain via CompileRequest/TestRequest.EVMVersion (e.g. "cancun"
// for a chain that supports it, or "paris" for a PUSH0-less node).
const defaultEVMVersion = "shanghai"

// foundryTomlFor renders the uploaded-source foundry.toml written when the
// caller did not provide their own. Contracts live under src/, tests under
// test/; the box's dependency tree is linked in as lib/ (see
// prepareSourceWorkdir). The EVM version is pinned (caller override falls back
// to defaultEVMVersion) so artifacts target an opcode set the deploy chain runs.
func foundryTomlFor(evmVersion string) string {
	if strings.TrimSpace(evmVersion) == "" {
		evmVersion = defaultEVMVersion
	}
	return `[profile.default]
src = "src"
test = "test"
out = "out"
libs = ["lib"]
optimizer = true
optimizer_runs = 200
evm_version = "` + evmVersion + `"
`
}

// sourcesProjectID derives a deterministic project id from the uploaded source
// set, so a compile and a later deploy/call resolve the same registry entries
// without the caller threading a path-derived id. Stable across requests with
// identical content (recompiles are idempotent).
func sourcesProjectID(sources map[string]string) string {
	keys := make([]string, 0, len(sources))
	for k := range sources {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(sources[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// prepareSourceWorkdir materializes an uploaded source set into an ephemeral
// Foundry project so the shared engine can compile/test contracts that were
// never on disk. The box's baked dependency tree is linked in (relative, inside
// the workdir root) so uploaded contracts can import forge-std and the
// @openzeppelin/contracts/ corpus without absolute paths:
//
//	lib/   -> <boxRoot>/lib     (forge-std, erc4626-tests, halmos-cheatcodes)
//	.oz/   -> <boxRoot>/contracts  (the OpenZeppelin corpus, matching the box
//	                                remapping @openzeppelin/contracts/=.oz/)
//
// Caller-provided foundry.toml / remappings.txt / lib files take precedence.
// evmVersion pins the generated foundry.toml's evm_version (empty →
// defaultEVMVersion); ignored when the caller uploads their own foundry.toml.
// Returns the workdir and a cleanup func the caller must always defer.
func (e *Engine) prepareSourceWorkdir(sources map[string]string, evmVersion string) (string, func(), *types.Error) {
	dir, err := os.MkdirTemp("", "tachyon-src-")
	if err != nil {
		return "", func() {}, types.NewError(types.CodeInternal, "create workdir: "+err.Error(), false, nil)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	provided := map[string]bool{}
	for rel, content := range sources {
		clean, ok := safeRel(rel)
		if !ok {
			cleanup()
			return "", func() {}, types.NewError(types.CodeInvalidRequest, "unsafe source path: "+rel, false, nil)
		}
		if len(content) > maxSourceBytes {
			cleanup()
			return "", func() {}, types.NewError(types.CodeInvalidRequest, "source too large: "+rel, false, nil)
		}
		dst := filepath.Join(dir, clean)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			cleanup()
			return "", func() {}, types.NewError(types.CodeInternal, err.Error(), false, nil)
		}
		if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
			cleanup()
			return "", func() {}, types.NewError(types.CodeInternal, err.Error(), false, nil)
		}
		provided[clean] = true
	}

	box := e.Cfg.ProjectRoot

	// Link the baked dependency tree so imports resolve via relative remappings
	// that stay inside the workdir root (forge allows symlinked import roots).
	if !provided["lib"] {
		if _, statErr := os.Stat(filepath.Join(box, "lib")); statErr == nil {
			_ = os.Symlink(filepath.Join(box, "lib"), filepath.Join(dir, "lib"))
		}
	}
	if _, statErr := os.Stat(filepath.Join(box, "contracts")); statErr == nil {
		_ = os.Symlink(filepath.Join(box, "contracts"), filepath.Join(dir, ".oz"))
	}

	if !provided["foundry.toml"] {
		if err := os.WriteFile(filepath.Join(dir, "foundry.toml"), []byte(foundryTomlFor(evmVersion)), 0o644); err != nil {
			cleanup()
			return "", func() {}, types.NewError(types.CodeInternal, err.Error(), false, nil)
		}
	}
	if !provided["remappings.txt"] {
		remap := "@openzeppelin/contracts/=.oz/\n" +
			"forge-std/=lib/forge-std/src/\n" +
			"erc4626-tests/=lib/erc4626-tests/\n" +
			"halmos-cheatcodes/=lib/halmos-cheatcodes/src/\n"
		if err := os.WriteFile(filepath.Join(dir, "remappings.txt"), []byte(remap), 0o644); err != nil {
			cleanup()
			return "", func() {}, types.NewError(types.CodeInternal, err.Error(), false, nil)
		}
	}

	return dir, cleanup, nil
}

// safeRel rejects absolute paths and parent-directory escapes so an uploaded
// key can never write outside the ephemeral workdir.
func safeRel(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return "", false
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(clean) {
		return "", false
	}
	return clean, true
}
