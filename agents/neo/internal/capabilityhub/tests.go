// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package capabilityhub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type toolTestSuite struct {
	SchemaVersion int        `json:"schema_version"`
	Cases         []toolTest `json:"cases"`
}

type toolTest struct {
	Name           string         `json:"name"`
	Tool           string         `json:"tool"`
	Arguments      map[string]any `json:"arguments"`
	ExpectContains string         `json:"expect_contains,omitempty"`
}

func loadToolTests(packageDir string, declared []string) ([]toolTest, error) {
	path := filepath.Join(packageDir, "CAPABILITY_TESTS.json")
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: tool-bearing packages require CAPABILITY_TESTS.json", ErrVerificationRequired)
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var suite toolTestSuite
	if err := decoder.Decode(&suite); err != nil {
		return nil, fmt.Errorf("%w: invalid CAPABILITY_TESTS.json: %v", ErrVerificationRequired, err)
	}
	if suite.SchemaVersion != 1 || len(suite.Cases) == 0 || len(suite.Cases) > 64 {
		return nil, fmt.Errorf("%w: invalid capability test suite", ErrVerificationRequired)
	}
	declaredSet := make(map[string]struct{}, len(declared))
	covered := make(map[string]struct{}, len(declared))
	for _, uri := range declared {
		declaredSet[uri] = struct{}{}
	}
	for index := range suite.Cases {
		test := &suite.Cases[index]
		test.Name = strings.TrimSpace(test.Name)
		test.Tool = strings.TrimSpace(test.Tool)
		if test.Name == "" || len(test.Name) > 128 || test.Tool == "" {
			return nil, fmt.Errorf("%w: test name and tool are required", ErrVerificationRequired)
		}
		if _, ok := declaredSet[test.Tool]; !ok {
			return nil, fmt.Errorf("%w: test uses undeclared tool %s", ErrVerificationRequired, test.Tool)
		}
		if test.Arguments == nil {
			test.Arguments = map[string]any{}
		}
		covered[test.Tool] = struct{}{}
	}
	for _, uri := range declared {
		if _, ok := covered[uri]; !ok {
			return nil, fmt.Errorf("%w: declared tool %s has no real test", ErrVerificationRequired, uri)
		}
	}
	return suite.Cases, nil
}
