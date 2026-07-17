// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
)

type Class string

const (
	Preventable Class = "preventable"
	Detectable  Class = "detectable"
	Irreducible Class = "irreducibly_uncertain"
	Unknown     Class = "unknown"
)

type Registry struct {
	Version    string   `json:"version"`
	Operations []string `json:"synthetic_operations"`
	Corpus     []Corpus `json:"corpus"`
	Hazards    []Hazard `json:"hazards"`
}

type Corpus struct {
	ID        string   `json:"id"`
	Source    string   `json:"source"`
	HazardIDs []string `json:"hazard_ids"`
}

type Hazard struct {
	ID        string   `json:"id"`
	Operation string   `json:"operation"`
	Source    string   `json:"source_boundary"`
	Trigger   string   `json:"trigger"`
	Evidence  []string `json:"evidence"`
	Class     Class    `json:"class"`
	Owner     string   `json:"owner"`
	Control   Control  `json:"control"`
	Recovery  Recovery `json:"recovery"`
	Outcomes  []string `json:"outcomes"`
	Verifier  string   `json:"verifier"`
	Status    string   `json:"status"`
}

type Control struct {
	Kind  string `json:"kind"`
	Phase string `json:"phase"`
	Ref   string `json:"ref"`
}

type Recovery struct {
	Initial  string   `json:"initial"`
	Allowed  []string `json:"allowed"`
	Terminal []string `json:"terminal"`
	Bound    int      `json:"bound"`
}

type agentManifest struct {
	Servers []struct {
		Alias string `json:"alias"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	} `json:"servers"`
}

func Load(registryPath string) (*Registry, error) {
	b, err := os.ReadFile(registryPath)
	if err != nil {
		return nil, err
	}
	var r Registry
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("decode registry: %w", err)
	}
	return &r, nil
}

func ManifestOperations(manifestPath string) ([]string, error) {
	b, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	var m agentManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("decode agent manifest: %w", err)
	}
	var out []string
	for _, server := range m.Servers {
		for _, tool := range server.Tools {
			out = append(out, server.Alias+"__"+tool.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (r *Registry) Check(manifestOperations []string) error {
	var problems []string
	if strings.TrimSpace(r.Version) == "" {
		problems = append(problems, "registry version is empty")
	}
	operations := append(append([]string{}, manifestOperations...), r.Operations...)
	sort.Strings(operations)

	seenIDs := map[string]bool{}
	covered := map[string]bool{}
	outcomes := map[string]bool{}
	for i, h := range r.Hazards {
		label := fmt.Sprintf("hazard[%d]", i)
		if h.ID == "" {
			problems = append(problems, label+" has no id")
		} else if seenIDs[h.ID] {
			problems = append(problems, "duplicate hazard id "+h.ID)
		}
		seenIDs[h.ID] = true
		if h.Operation == "" || h.Source == "" || h.Trigger == "" || len(h.Evidence) == 0 ||
			h.Owner == "" || len(h.Outcomes) == 0 || h.Recovery.Initial == "" ||
			len(h.Recovery.Terminal) == 0 || h.Verifier == "" || h.Status == "" {
			problems = append(problems, h.ID+" is missing a required registry field")
		}
		switch h.Class {
		case Preventable, Detectable, Irreducible:
		case Unknown:
			problems = append(problems, h.ID+" remains unknown")
		default:
			problems = append(problems, h.ID+" has invalid class "+string(h.Class))
		}
		if h.Class == Preventable && (h.Control.Kind == "" || h.Control.Phase != "pre_execution" || h.Control.Ref == "") {
			problems = append(problems, h.ID+" preventable hazard lacks a pre_execution control")
		}
		for _, outcome := range h.Outcomes {
			outcomes[h.ID+"\x00"+outcome] = true
		}
		for _, terminal := range h.Recovery.Terminal {
			if !outcomes[h.ID+"\x00"+terminal] {
				problems = append(problems, h.ID+" recovery terminal has no registered outcome: "+terminal)
			}
		}
		matched := false
		for _, operation := range operations {
			ok, err := path.Match(h.Operation, operation)
			if err != nil {
				problems = append(problems, h.ID+" has invalid operation selector: "+err.Error())
				break
			}
			if ok {
				covered[operation] = true
				matched = true
			}
		}
		if !matched {
			problems = append(problems, h.ID+" selector matches no shipped operation: "+h.Operation)
		}
	}
	for _, operation := range operations {
		if !covered[operation] {
			problems = append(problems, "uncovered operation "+operation)
		}
	}
	seenCorpus := map[string]bool{}
	for i, item := range r.Corpus {
		label := fmt.Sprintf("corpus[%d]", i)
		if item.ID == "" || item.Source == "" || len(item.HazardIDs) == 0 {
			problems = append(problems, label+" is missing id, source, or hazard_ids")
			continue
		}
		if seenCorpus[item.ID] {
			problems = append(problems, "duplicate corpus id "+item.ID)
		}
		seenCorpus[item.ID] = true
		for _, id := range item.HazardIDs {
			if !seenIDs[id] {
				problems = append(problems, item.ID+" references unknown hazard "+id)
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("O1 registry check failed:\n- %s", strings.Join(problems, "\n- "))
	}
	return nil
}
