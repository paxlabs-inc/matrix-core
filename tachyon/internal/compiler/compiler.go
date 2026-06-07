package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/paxlabs-inc/tachyon-tools/internal/forgeutil"
	"github.com/paxlabs-inc/tachyon-tools/internal/registry"
	"github.com/paxlabs-inc/tachyon-tools/pkg/types"
)

// Compiler wraps forge build and artifact normalization.
type Compiler struct {
	ForgePath    string
	ArtifactsDir string
}

// ProjectID derives a stable id from project root.
func ProjectID(root string) string {
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:8])
}

// Compile runs forge build and indexes artifacts.
func (c *Compiler) Compile(req types.CompileRequest, reg *registry.Registry) (types.CompileResponse, *types.Error) {
	root := strings.TrimSpace(req.ProjectRoot)
	if root == "" {
		return types.CompileResponse{}, types.NewError(types.CodeInvalidRequest, "project_root required", false, nil)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return types.CompileResponse{}, types.NewError(types.CodeInvalidRequest, err.Error(), false, nil)
	}
	projectID := strings.TrimSpace(req.ProjectID)
	if projectID == "" {
		projectID = ProjectID(abs)
	}

	args := []string{"build", "--skip", "test"}

	stdout, stderr, runErr := forgeutil.RunWithTimeout(c.ForgePath, abs, 15*time.Minute, args...)
	if runErr != nil {
		code := types.CodeCompilerForgeFailed
		if strings.Contains(strings.ToLower(stderr+stdout), "solc") {
			code = types.CodeCompilerSolcFailed
		}
		return types.CompileResponse{}, types.NewError(code, forgeutil.FormatForgeError(stdout, stderr, runErr), true, map[string]string{
			"stdout": strings.TrimSpace(stdout),
			"stderr": strings.TrimSpace(stderr),
		})
	}

	artifacts, warn, err := c.collectArtifacts(abs, projectID, req.Targets)
	if err != nil {
		return types.CompileResponse{}, types.NewError(types.CodeInternal, err.Error(), false, nil)
	}

	for _, a := range artifacts {
		rec := registry.ArtifactRecord{
			ProjectID:        projectID,
			Name:             a.Name,
			Path:             a.Path,
			ABI:              a.ABI,
			Bytecode:         a.Bytecode,
			DeployedBytecode: a.DeployedBytecode,
			UpdatedAt:        time.Now().UTC().Format(time.RFC3339),
		}
		if a.Compiler != nil {
			b, _ := json.Marshal(a.Compiler)
			rec.Compiler = b
		}
		if reg != nil {
			_ = reg.PutArtifact(rec)
		}
		if c.ArtifactsDir != "" {
			_ = c.mirrorArtifact(abs, a)
		}
	}

	return types.CompileResponse{
		ProjectID: projectID,
		Artifacts: artifacts,
		Warnings:  warn,
	}, nil
}

func (c *Compiler) collectArtifacts(root, projectID string, targets []string) ([]types.Artifact, []string, error) {
	outDir := filepath.Join(root, "out")
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return nil, nil, fmt.Errorf("read out/: %w", err)
	}

	targetSet := map[string]struct{}{}
	for _, t := range targets {
		targetSet[strings.TrimSpace(t)] = struct{}{}
	}

	var artifacts []types.Artifact
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		dir := filepath.Join(outDir, ent.Name())
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".json") || strings.HasSuffix(f.Name(), ".metadata.json") {
				continue
			}
			name := strings.TrimSuffix(f.Name(), ".json")
			if len(targetSet) > 0 {
				if _, ok := targetSet[name]; !ok {
					continue
				}
			}
			art, err := parseForgeArtifact(filepath.Join(dir, f.Name()), ent.Name(), name)
			if err != nil {
				continue
			}
			artifacts = append(artifacts, art)
		}
	}
	return artifacts, nil, nil
}

type forgeArtifact struct {
	ABI              json.RawMessage `json:"abi"`
	Bytecode         struct {
		Object string `json:"object"`
	} `json:"bytecode"`
	DeployedBytecode struct {
		Object string `json:"object"`
	} `json:"deployedBytecode"`
	Metadata json.RawMessage `json:"metadata"`
}

func parseForgeArtifact(path, sourceFile, name string) (types.Artifact, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return types.Artifact{}, err
	}
	var raw forgeArtifact
	if err := json.Unmarshal(b, &raw); err != nil {
		return types.Artifact{}, err
	}
	relPath := strings.TrimSuffix(sourceFile, ".sol") + ".sol"
	if !strings.HasPrefix(relPath, "contracts/") {
		relPath = filepath.Join("contracts", filepath.Base(sourceFile))
	}
	var compiler *types.CompilerSettings
	if len(raw.Metadata) > 0 {
		var meta struct {
			Compiler struct {
				Version string `json:"version"`
				Settings struct {
					Optimizer struct {
						Enabled bool `json:"enabled"`
						Runs    int  `json:"runs"`
					} `json:"optimizer"`
				} `json:"settings"`
			} `json:"compiler"`
		}
		if err := json.Unmarshal(raw.Metadata, &meta); err == nil && meta.Compiler.Version != "" {
			compiler = &types.CompilerSettings{
				Version: meta.Compiler.Version,
				Optimizer: &types.OptimizerConfig{
					Enabled: meta.Compiler.Settings.Optimizer.Enabled,
					Runs:    meta.Compiler.Settings.Optimizer.Runs,
				},
			}
		}
	}
	return types.Artifact{
		Name:             name,
		Path:             relPath,
		ABI:              raw.ABI,
		Bytecode:         raw.Bytecode.Object,
		DeployedBytecode: raw.DeployedBytecode.Object,
		Compiler:         compiler,
	}, nil
}

func (c *Compiler) mirrorArtifact(root string, art types.Artifact) error {
	destDir := filepath.Join(root, c.ArtifactsDir)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(art, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(destDir, art.Name+".json"), b, 0o644)
}
