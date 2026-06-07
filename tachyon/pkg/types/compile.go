package types

import "encoding/json"

// CompileRequest builds Solidity contracts via forge.
//
// Sources, when non-empty, makes the request self-contained: the engine
// materializes the map (workdir-relative path -> file content) into an
// ephemeral Foundry project, links the box's baked dependency tree
// (forge-std + the @openzeppelin/contracts/ corpus), runs forge there, and
// derives a deterministic ProjectID from the source set. This is how a SHARED
// tachyond compiles a caller's own contracts without those files ever living
// on the box. Convention: contracts under src/, tests under test/; OZ
// (@openzeppelin/contracts/...) and forge-std/... resolve automatically.
type CompileRequest struct {
	ProjectRoot string            `json:"project_root,omitempty"`
	ProjectID   string            `json:"project_id,omitempty"`
	Sources     map[string]string `json:"sources,omitempty"`
	Targets     []string          `json:"targets,omitempty"`
	Optimize    *bool             `json:"optimize,omitempty"`
	ViaIR       *bool             `json:"via_ir,omitempty"`
}

// CompilerSettings mirrors solc optimizer metadata.
type CompilerSettings struct {
	Version   string           `json:"version,omitempty"`
	Optimizer *OptimizerConfig `json:"optimizer,omitempty"`
}

// OptimizerConfig holds solc optimizer flags.
type OptimizerConfig struct {
	Enabled bool `json:"enabled"`
	Runs    int  `json:"runs,omitempty"`
}

// Artifact is a normalized contract build output for agents.
type Artifact struct {
	Name             string            `json:"name"`
	Path             string            `json:"path,omitempty"`
	ABI              json.RawMessage   `json:"abi"`
	Bytecode         string            `json:"bytecode"`
	DeployedBytecode string            `json:"deployedBytecode,omitempty"`
	Compiler         *CompilerSettings `json:"compiler,omitempty"`
}

// CompileResponse is returned by compile verb.
type CompileResponse struct {
	ProjectID string     `json:"project_id"`
	Artifacts []Artifact `json:"artifacts"`
	Warnings  []string   `json:"warnings,omitempty"`
}
