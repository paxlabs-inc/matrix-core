// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package capabilityhub

import (
	"errors"
	"time"
)

type State string

const (
	StateQuarantine  State = "quarantine"
	StateVerified    State = "verified"
	StateActive      State = "active"
	StateDisabled    State = "disabled"
	StateUninstalled State = "uninstalled"
)

type SourceType string

const (
	SourceLibrary  SourceType = "library"
	SourceGitHub   SourceType = "github"
	SourceURL      SourceType = "url"
	SourceAuthored SourceType = "authored"
)

var (
	ErrNotFound             = errors.New("capability not found")
	ErrVersionConflict      = errors.New("capability version already has different content")
	ErrInvalidTransition    = errors.New("invalid capability lifecycle transition")
	ErrGrantRequired        = errors.New("capability permission grant required")
	ErrToolUnavailable      = errors.New("declared tool is unavailable")
	ErrVerificationRequired = errors.New("real capability verification is required")
	ErrUnsafePackage        = errors.New("unsafe capability package")
)

type Capability struct {
	Slug              string     `json:"slug"`
	Version           string     `json:"version"`
	Digest            string     `json:"digest"`
	CanonicalHash     string     `json:"canonical_hash"`
	Display           string     `json:"display"`
	Description       string     `json:"description"`
	Publisher         string     `json:"publisher,omitempty"`
	SourceType        SourceType `json:"source_type"`
	SourceRef         string     `json:"source_ref"`
	State             State      `json:"state"`
	Pinned            bool       `json:"pinned"`
	DeclaredTools     []string   `json:"declared_tools"`
	DeclaredSubSkills []string   `json:"declared_subskills"`
	Granted           []string   `json:"granted"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	VerifiedAt        *time.Time `json:"verified_at,omitempty"`
	ActivatedAt       *time.Time `json:"activated_at,omitempty"`
	LastError         string     `json:"last_error,omitempty"`
}

type ImportRequest struct {
	SourceDir  string
	SourceType SourceType
	SourceRef  string
}

type AuthoredRequest struct {
	Manifest  string
	Prose     string
	SourceRef string
}

type Query struct {
	Search string
	State  State
}

type LibraryItem struct {
	Slug          string   `json:"slug"`
	Version       string   `json:"version"`
	Display       string   `json:"display"`
	Description   string   `json:"description"`
	Publisher     string   `json:"publisher,omitempty"`
	CanonicalHash string   `json:"canonical_hash"`
	DeclaredTools []string `json:"declared_tools"`
}

type AuditEvent struct {
	ID        int64     `json:"id"`
	Slug      string    `json:"slug"`
	Version   string    `json:"version,omitempty"`
	Action    string    `json:"action"`
	Detail    string    `json:"detail,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Verification struct {
	AvailableTools map[string]string
	ReadOnlyTools  map[string]bool
	RunTool        func(name string, arguments map[string]any) (ToolTestResult, error)
}

type ToolTestResult struct {
	Content string
	IsError bool
}
