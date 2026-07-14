// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// taskgraph.go — Mechanism 4 of the epistemic core (epistemic-core req.7):
// the reified task graph. The task is live structure — goal → subgoals →
// premises referenced → evidence discharged — seeded from the plan and
// maintained across steps, so convergence is COMPUTED as evidence-set delta
// instead of inferred from text similarity. The existing todo surface is a
// projection of this graph: the model's todo calls mutate the graph's
// subgoals, and the rendered checklist and the graph share one vocabulary
// (tools.TodoItem / tools.TodoStatus — req.7.3).
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"matrix/neo/internal/tools"
)

// graphSubgoal is one subgoal node: a todo item plus the actions linked to it
// and the evidence discharged under it.
type graphSubgoal struct {
	Text     string
	Status   tools.TodoStatus
	Actions  int      // dispatched actions linked to this subgoal (req.7.1)
	Evidence []string // evidence digests discharged under this subgoal
}

// taskGraph is the run-state graph. Plain agent-core state (no new cortex
// memory types); reset per turn with the rest of the epistemic run state.
type taskGraph struct {
	Goal     string
	Subgoals []*graphSubgoal

	// evidenceSet is the global evidence identity set; convergence is its
	// delta: an action either grows it or it doesn't (req.7.2).
	evidenceSet map[string]struct{}

	// actionsSinceGrowth counts consecutive dispatched actions that grew no
	// evidence — the computed non-convergence meter (read by the forced
	// revision gate, task 3.2).
	actionsSinceGrowth int
}

// ensureGraph lazily seeds the graph from the active goal.
func (a *Agent) ensureGraph() *taskGraph {
	if a.turn.graph == nil {
		a.turn.graph = &taskGraph{Goal: a.activeGoal, evidenceSet: map[string]struct{}{}}
	}
	return a.turn.graph
}

// current returns the subgoal actions link to: the one in_progress, else nil
// (actions then link to the goal root).
func (g *taskGraph) current() *graphSubgoal {
	for _, s := range g.Subgoals {
		if s.Status == tools.TodoInProgress {
			return s
		}
	}
	return nil
}

// projectTodo folds a todo-call item list into the graph's subgoals (the todo
// tool is a projection of the graph — one vocabulary): items merge by text,
// statuses update, order follows the latest call.
func (g *taskGraph) projectTodo(items []tools.TodoItem) {
	byText := make(map[string]*graphSubgoal, len(g.Subgoals))
	for _, s := range g.Subgoals {
		byText[s.Text] = s
	}
	next := make([]*graphSubgoal, 0, len(items))
	for _, it := range items {
		if s, ok := byText[it.Text]; ok {
			s.Status = it.Status
			next = append(next, s)
			continue
		}
		next = append(next, &graphSubgoal{Text: it.Text, Status: it.Status})
	}
	g.Subgoals = next
}

// evidenceKey computes the identity of one piece of discharged evidence. A
// probe that missed its expectation discharges NOTHING (five differently-
// argued guesses grow no evidence); a successful action's evidence is keyed on
// the tool and its result content, so re-fetching the same thing is not
// growth.
func evidenceKey(name, content string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(content)))
	return name + "\x00" + hex.EncodeToString(sum[:8])
}

// graphObserve links one dispatched action to the graph and computes the
// evidence delta (req.7.1/7.2). probeMissed marks a probe-class call whose
// expectation mismatched — it discharges no evidence by definition. Renders
// the resident graph slot the step anything changes (req.7.4).
func (a *Agent) graphObserve(name string, args map[string]interface{}, content string, isErr, probeMissed bool) {
	if !a.epistemicPremisesOn() && !a.epistemicPredictionsOn() {
		return
	}
	g := a.ensureGraph()

	// The todo surface is a projection of the graph: a todo call mutates
	// subgoals and links no action of its own.
	if name == tools.TodoTool {
		g.projectTodo(tools.ParseTodoItems(args))
		a.renderGraphTail()
		return
	}

	sub := g.current()
	if sub != nil {
		sub.Actions++
	}
	grew := false
	if !isErr && !probeMissed {
		key := evidenceKey(name, content)
		if _, seen := g.evidenceSet[key]; !seen {
			g.evidenceSet[key] = struct{}{}
			grew = true
			if sub != nil {
				sub.Evidence = append(sub.Evidence, key)
			}
		}
	}
	if grew {
		g.actionsSinceGrowth = 0
	} else {
		g.actionsSinceGrowth++
		// Measured non-convergence (req.7.2): N consecutive actions with an
		// unchanged evidence set force the same tools-stripped revision step
		// as the mismatch bound — caught at action N, not token one million.
		if window := a.cfg.EpistemicConvergenceWindow; window > 0 && g.actionsSinceGrowth >= window &&
			a.turn.revisionPending == "" && a.turn.revisionsThisTurn < maxRevisionsPerTurn {
			a.turn.revisionPending = fmt.Sprintf(
				"Measured non-convergence: your last %d actions added NO new evidence to the task — you are moving without progressing. This step has NO tools on purpose: state what you actually know, what the recent actions were supposed to establish, and write a revised plan whose next action will verifiably add evidence.",
				g.actionsSinceGrowth)
		}
	}
	a.renderGraphTail()
}

// renderGraphTail renders the graph into its fixed tail slot (req.7.4): what
// is proven (evidence), what is assumed (standing assumptions on the ledger),
// and what remains (subgoals). Empty graph renders nothing.
func (a *Agent) renderGraphTail() {
	g := a.turn.graph
	if g == nil || (len(g.Subgoals) == 0 && len(g.evidenceSet) == 0) {
		a.turn.graphTail = ""
		return
	}
	var b strings.Builder
	b.WriteString("\nTask graph (your position, computed — not how it feels):\n")
	if goal := strings.TrimSpace(g.Goal); goal != "" {
		fmt.Fprintf(&b, "Goal: %s\n", truncate(goal, 200))
	}
	for _, s := range g.Subgoals {
		fmt.Fprintf(&b, "- [%s] %s (actions: %d, evidence: %d)\n", s.Status, s.Text, s.Actions, len(s.Evidence))
	}
	assumptions := len(a.turn.ledger.assumptions())
	fmt.Fprintf(&b, "Proven: %d evidence items. Assumed: %d unchecked premises. ", len(g.evidenceSet), assumptions)
	if g.actionsSinceGrowth > 0 {
		fmt.Fprintf(&b, "Convergence: your last %d action(s) added NO new evidence — if the next action won't either, revise the approach instead of dispatching it.\n", g.actionsSinceGrowth)
	} else {
		b.WriteString("Convergence: evidence is growing.\n")
	}
	a.turn.graphTail = b.String()
}
