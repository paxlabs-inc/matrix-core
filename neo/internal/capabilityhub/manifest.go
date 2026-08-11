// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package capabilityhub

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"matrix/mcl/mtx/ast"
	"matrix/mcl/mtx/canonical"
	"matrix/mcl/mtx/parser"
	"matrix/mcl/mtx/validator"
)

type loadedPackage struct {
	Slug              string
	Version           string
	Display           string
	Description       string
	Author            string
	CanonicalHash     string
	DeclaredTools     []string
	DeclaredSubSkills []string
}

func loadPackage(root, slug, version string) (*loadedPackage, error) {
	manifestPath := filepath.Join(root, slug, "SKILL.mtx")
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("capability manifest read: %w", err)
	}
	file, parseErrors := parser.New(body).Parse()
	if len(parseErrors) > 0 {
		return nil, fmt.Errorf("capability manifest parse: %s", parseErrors[0])
	}
	validationErrors := validator.ValidateSkill(file)
	if len(validationErrors) > 0 {
		return nil, fmt.Errorf("capability manifest validate: %s", validationErrors[0])
	}
	loaded := &loadedPackage{CanonicalHash: canonical.Hash(file)}
	for _, section := range file.Sections {
		switch section.Name {
		case "SKILL":
			for _, entry := range section.Entries {
				pair, ok := entry.(*ast.KVPair)
				if !ok {
					continue
				}
				switch strings.Join(pair.Key, ".") {
				case "id":
					loaded.Slug = manifestValue(pair.Value)
				case "version":
					loaded.Version = manifestValue(pair.Value)
				case "display":
					loaded.Display = manifestValue(pair.Value)
				case "description":
					loaded.Description = manifestValue(pair.Value)
				case "author":
					loaded.Author = manifestValue(pair.Value)
				}
			}
		case "TOOLS":
			loaded.DeclaredTools = manifestRefs(section)
		case "SUB_SKILLS":
			loaded.DeclaredSubSkills = manifestRefs(section)
		}
	}
	if loaded.Slug != slug || loaded.Version != version {
		return nil, fmt.Errorf("capability manifest identity mismatch: got %s@%s, expected %s@%s", loaded.Slug, loaded.Version, slug, version)
	}
	return loaded, nil
}

func manifestValue(value ast.Value) string {
	switch typed := value.(type) {
	case *ast.StringValue:
		return typed.Text
	case *ast.IdentValue:
		return typed.Name
	case *ast.SpaceListValue:
		return strings.Join(typed.Items, " ")
	default:
		return ""
	}
}

func manifestRefs(section *ast.Section) []string {
	var result []string
	for _, entry := range section.Entries {
		if ref, ok := entry.(*ast.RefEntry); ok && ref.URI != "" {
			result = append(result, ref.URI)
		}
	}
	return result
}
