package types

import "encoding/json"

// CompileRequest builds Solidity contracts via forge.
type CompileRequest struct {
	ProjectRoot string   `json:"project_root,omitempty"`
	ProjectID   string   `json:"project_id,omitempty"`
	Targets     []string `json:"targets,omitempty"`
	Optimize    *bool    `json:"optimize,omitempty"`
	ViaIR       *bool    `json:"via_ir,omitempty"`
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
