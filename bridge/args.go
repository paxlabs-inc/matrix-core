// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package bridge

import (
	"fmt"
	"strconv"
	"strings"

	"matrix/cortex"
	"matrix/cortex/memory"
	"matrix/cortex/query"
)

// buildQuery translates an MCL arg dict into a query.Query.
//
// Unknown keys are an error so SKILL.mtx typos are caught at compile
// time rather than silently dropped (research/04 §12 "no silent
// degradation" discipline).
func (a *Adapter) buildQuery(args map[string]string) (query.Query, error) {
	q := query.Query{
		Limit:       a.defaultLimit,
		Form:        a.defaultForm,
		LateBinding: a.lateBinding,
	}

	var conjuncts []query.Predicate

	for k, v := range args {
		switch k {
		case "type":
			t, ok := parseTypeName(v)
			if !ok {
				return q, fmt.Errorf("%w: %q", ErrUnknownType, v)
			}
			q.Type = append(q.Type, t)

		case "tag":
			if v != "" {
				conjuncts = append(conjuncts, query.HasTag{Tag: v})
			}

		case "near":
			q.Near = v

		case "limit":
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return q, fmt.Errorf("bad limit %q (want positive int)", v)
			}
			q.Limit = n

		case "form":
			f, ok := parseForm(v)
			if !ok {
				return q, fmt.Errorf("unknown form %q (want short|medium|full)", v)
			}
			q.Form = f

		case "late":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return q, fmt.Errorf("bad late=%q (want true|false)", v)
			}
			q.LateBinding = b

		case "include_tombstoned":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return q, fmt.Errorf("bad include_tombstoned=%q (want true|false)", v)
			}
			q.IncludeTombstoned = b

		default:
			return q, fmt.Errorf("unknown find arg %q", k)
		}
	}

	if len(conjuncts) == 1 {
		q.Where = conjuncts[0]
	} else if len(conjuncts) > 1 {
		q.Where = query.And{Children: conjuncts}
	}

	return q, nil
}

// buildContextOpts translates an MCL arg dict into a cortex.ContextOpts.
func (a *Adapter) buildContextOpts(args map[string]string) (cortex.ContextOpts, error) {
	opts := cortex.ContextOpts{}

	for k, v := range args {
		switch k {
		case "verb":
			vb, ok := memory.ParseVerb(v)
			if !ok {
				return opts, fmt.Errorf("unknown verb %q", v)
			}
			opts.Verb = vb

		case "objects":
			m, err := parseObjects(v)
			if err != nil {
				return opts, err
			}
			opts.Objects = m

		case "budget_tokens":
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return opts, fmt.Errorf("bad budget_tokens %q", v)
			}
			opts.BudgetTokens = n

		case "outcome_limit":
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return opts, fmt.Errorf("bad outcome_limit %q", v)
			}
			opts.OutcomeLimit = n

		case "form":
			f, ok := parseForm(v)
			if !ok {
				return opts, fmt.Errorf("unknown form %q (want short|medium|full)", v)
			}
			opts.Form = f

		default:
			return opts, fmt.Errorf("unknown context arg %q", k)
		}
	}

	return opts, nil
}

// parseTypeName maps the canonical Type string to memory.Type. Returns
// (TypeZero, false) on no match. Mirrors the cortex-shell CLI helper but
// lives in the bridge so MCL surfaces are not coupled to the CLI package.
func parseTypeName(name string) (memory.Type, bool) {
	switch name {
	case "Identity":
		return memory.TypeIdentity, true
	case "Fact":
		return memory.TypeFact, true
	case "Preference":
		return memory.TypePreference, true
	case "Belief":
		return memory.TypeBelief, true
	case "Event":
		return memory.TypeEvent, true
	case "Goal":
		return memory.TypeGoal, true
	case "Constraint":
		return memory.TypeConstraint, true
	case "Capability":
		return memory.TypeCapability, true
	case "Pattern":
		return memory.TypePattern, true
	default:
		return 0, false
	}
}

// parseForm maps a SKILL.mtx form= value to a query.FormKind.
func parseForm(s string) (query.FormKind, bool) {
	switch s {
	case "short":
		return query.FormShort, true
	case "medium":
		return query.FormMedium, true
	case "full":
		return query.FormFull, true
	default:
		return "", false
	}
}

// parseObjects parses a Context objects= argument into the ContextOpts
// Objects map. Accepted shapes:
//
//	"service:foo,agent:bar"
//	"service:foo;agent:bar"
//	"service:foo"             (single)
//
// kind must be a closed v1 ObjKind name (see memory.ParseObjKind);
// unknown kinds return an error so SKILL.mtx typos surface at compile.
//
// Whitespace around delimiters is tolerated; whitespace inside `ref`
// is preserved verbatim.
func parseObjects(s string) (map[string]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}

	// Prefer ';' if present, else ','. Both work; mixed delimiters
	// would be ambiguous so we reject that.
	if strings.ContainsRune(s, ';') && strings.ContainsRune(s, ',') {
		return nil, fmt.Errorf("objects: mixed ',' and ';' separators not allowed")
	}
	sep := ","
	if strings.ContainsRune(s, ';') {
		sep = ";"
	}

	pairs := strings.Split(s, sep)
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		idx := strings.IndexByte(p, ':')
		if idx <= 0 || idx == len(p)-1 {
			return nil, fmt.Errorf("objects: bad pair %q (want kind:ref)", p)
		}
		kind := strings.TrimSpace(p[:idx])
		ref := strings.TrimSpace(p[idx+1:])
		if _, ok := memory.ParseObjKind(kind); !ok {
			return nil, fmt.Errorf("objects: unknown obj_kind %q", kind)
		}
		out[kind] = ref
	}
	return out, nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
