// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"fmt"
	"sort"
	"strings"
)

type PersistentGoal struct {
	ID        string
	Objective string
}

type OpenLoop struct {
	ID      string
	Summary string
}

func (a *Agent) TurnObjective() string {
	if a == nil {
		return ""
	}
	return a.currentObjective()
}

func (a *Agent) currentObjective() string {
	if a.cfg.SessionCurrentIntent && a.turn != nil {
		return strings.TrimSpace(a.turn.turnObjective)
	}
	return strings.TrimSpace(a.activeGoal)
}

func (a *Agent) setTurnObjective(objective string) {
	if a == nil || !a.cfg.SessionCurrentIntent || a.turn == nil {
		return
	}
	objective = strings.TrimSpace(objective)
	a.turn.turnObjective = objective
	a.turn.objective = objective
	a.activeGoal = objective
	if a.turn.graph != nil {
		a.turn.graph = nil
		a.turn.graphTail = ""
	}
}

func (a *Agent) RegisterPersistentGoal(objective string) string {
	if a == nil || !a.cfg.SessionCurrentIntent {
		return ""
	}
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return ""
	}
	a.intentMu.Lock()
	defer a.intentMu.Unlock()
	if a.persistentGoal != nil && a.persistentGoal.Objective == objective {
		return a.persistentGoal.ID
	}
	a.intentSequence++
	id := fmt.Sprintf("persistent-goal-%d", a.intentSequence)
	a.persistentGoal = &PersistentGoal{ID: id, Objective: objective}
	return id
}

func (a *Agent) CurrentPersistentGoal() (PersistentGoal, bool) {
	if a == nil || !a.cfg.SessionCurrentIntent {
		return PersistentGoal{}, false
	}
	a.intentMu.RLock()
	defer a.intentMu.RUnlock()
	if a.persistentGoal == nil {
		return PersistentGoal{}, false
	}
	return *a.persistentGoal, true
}

func (a *Agent) CompletePersistentGoal(id string) bool {
	return a.clearPersistentGoal(id)
}

func (a *Agent) AbandonPersistentGoal(id string) bool {
	return a.clearPersistentGoal(id)
}

func (a *Agent) SupersedePersistentGoal(id, objective string) (string, bool) {
	if a == nil || !a.cfg.SessionCurrentIntent {
		return "", false
	}
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return "", false
	}
	a.intentMu.Lock()
	defer a.intentMu.Unlock()
	if a.persistentGoal == nil || a.persistentGoal.ID != strings.TrimSpace(id) {
		return "", false
	}
	a.intentSequence++
	newID := fmt.Sprintf("persistent-goal-%d", a.intentSequence)
	a.persistentGoal = &PersistentGoal{ID: newID, Objective: objective}
	return newID, true
}

func (a *Agent) clearPersistentGoal(id string) bool {
	if a == nil || !a.cfg.SessionCurrentIntent {
		return false
	}
	a.intentMu.Lock()
	defer a.intentMu.Unlock()
	if a.persistentGoal == nil || a.persistentGoal.ID != strings.TrimSpace(id) {
		return false
	}
	a.persistentGoal = nil
	return true
}

func (a *Agent) RegisterOpenLoop(summary string) string {
	if a == nil || !a.cfg.SessionCurrentIntent {
		return ""
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return ""
	}
	a.intentMu.Lock()
	defer a.intentMu.Unlock()
	for _, loop := range a.openLoops {
		if loop.Summary == summary {
			return loop.ID
		}
	}
	a.intentSequence++
	id := fmt.Sprintf("open-loop-%d", a.intentSequence)
	if a.openLoops == nil {
		a.openLoops = make(map[string]OpenLoop)
	}
	a.openLoops[id] = OpenLoop{ID: id, Summary: summary}
	return id
}

func (a *Agent) OpenLoops() []OpenLoop {
	if a == nil || !a.cfg.SessionCurrentIntent {
		return nil
	}
	a.intentMu.RLock()
	defer a.intentMu.RUnlock()
	loops := make([]OpenLoop, 0, len(a.openLoops))
	for _, loop := range a.openLoops {
		loops = append(loops, loop)
	}
	sort.Slice(loops, func(i, j int) bool { return loops[i].ID < loops[j].ID })
	return loops
}

func (a *Agent) CompleteOpenLoop(id string) bool {
	return a.clearOpenLoop(id)
}

func (a *Agent) AbandonOpenLoop(id string) bool {
	return a.clearOpenLoop(id)
}

func (a *Agent) SupersedeOpenLoop(id, summary string) (string, bool) {
	if a == nil || !a.cfg.SessionCurrentIntent {
		return "", false
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return "", false
	}
	a.intentMu.Lock()
	defer a.intentMu.Unlock()
	if _, ok := a.openLoops[strings.TrimSpace(id)]; !ok {
		return "", false
	}
	delete(a.openLoops, strings.TrimSpace(id))
	a.intentSequence++
	newID := fmt.Sprintf("open-loop-%d", a.intentSequence)
	a.openLoops[newID] = OpenLoop{ID: newID, Summary: summary}
	return newID, true
}

func (a *Agent) clearOpenLoop(id string) bool {
	if a == nil || !a.cfg.SessionCurrentIntent {
		return false
	}
	a.intentMu.Lock()
	defer a.intentMu.Unlock()
	id = strings.TrimSpace(id)
	if _, ok := a.openLoops[id]; !ok {
		return false
	}
	delete(a.openLoops, id)
	return true
}

func (a *Agent) resetSessionIntent() {
	if a == nil {
		return
	}
	a.intentMu.Lock()
	a.persistentGoal = nil
	a.openLoops = nil
	a.intentSequence = 0
	a.intentMu.Unlock()
}
