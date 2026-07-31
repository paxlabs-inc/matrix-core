package dependency

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"matrix/workforce/internal/contracts"
)

// Snapshot is a complete organization graph at one durable boundary.
type Snapshot struct {
	Nodes []Node
	Edges []Edge
}

// Resolve computes readiness, inherited priority, timeout actions, and incidents
// without mutating the supplied snapshot.
func Resolve(snapshot Snapshot, now time.Time) (Projection, error) {
	if !validUTC(now) {
		return Projection{}, fmt.Errorf("resolver now must be non-zero UTC")
	}
	nodes := make(map[NodeID]Node, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		if err := node.Validate(); err != nil {
			return Projection{}, fmt.Errorf("node %q: %w", node.ID, err)
		}
		if _, exists := nodes[node.ID]; exists {
			return Projection{}, fmt.Errorf("%w: duplicate node %q", ErrConflict, node.ID)
		}
		nodes[node.ID] = node
	}
	outgoing := make(map[NodeID][]Edge, len(nodes))
	incoming := make(map[NodeID][]Edge, len(nodes))
	for _, edge := range snapshot.Edges {
		if err := edge.Validate(); err != nil {
			return Projection{}, fmt.Errorf("edge %q -> %q: %w", edge.Prerequisite, edge.Dependent, err)
		}
		if _, exists := nodes[edge.Prerequisite]; !exists {
			return Projection{}, fmt.Errorf("%w: prerequisite %q", ErrNotFound, edge.Prerequisite)
		}
		if _, exists := nodes[edge.Dependent]; !exists {
			return Projection{}, fmt.Errorf("%w: dependent %q", ErrNotFound, edge.Dependent)
		}
		outgoing[edge.Prerequisite] = append(outgoing[edge.Prerequisite], edge)
		incoming[edge.Dependent] = append(incoming[edge.Dependent], edge)
	}
	if cycle := findCycle(nodes, outgoing); len(cycle) > 0 {
		return Projection{}, fmt.Errorf("%w: %v", ErrCycle, cycle)
	}

	effectivePriority := make(map[NodeID]int64, len(nodes))
	for id, node := range nodes {
		effectivePriority[id] = agedPriority(node, now)
	}
	order := reverseTopological(nodes, outgoing, incoming)
	for _, id := range order {
		for _, edge := range outgoing[id] {
			if effectivePriority[edge.Dependent] > effectivePriority[id] {
				effectivePriority[id] = effectivePriority[edge.Dependent]
			}
		}
	}

	resolved := make([]Node, 0, len(nodes))
	eligible := make([]Node, 0)
	for id, node := range nodes {
		if node.State == StatePending && !node.Contested {
			ready := true
			for _, edge := range incoming[id] {
				if nodes[edge.Prerequisite].State != StateCompleted {
					ready = false
					break
				}
			}
			if ready {
				node.State = StateEligible
			}
		}
		if node.State == StateEligible && !node.Contested {
			eligible = append(eligible, node)
		}
		resolved = append(resolved, node)
	}
	sortNodes(resolved, effectivePriority)
	sortNodes(eligible, effectivePriority)

	incidents := detectIncidents(nodes, incoming, outgoing, snapshot.Edges, now)
	edges := append([]Edge(nil), snapshot.Edges...)
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Dependent != edges[j].Dependent {
			return edges[i].Dependent < edges[j].Dependent
		}
		if edges[i].Prerequisite != edges[j].Prerequisite {
			return edges[i].Prerequisite < edges[j].Prerequisite
		}
		return edges[i].Kind < edges[j].Kind
	})
	return Projection{Nodes: resolved, Edges: edges, Eligible: eligible, Incidents: incidents}, nil
}

// WouldCycle reports whether adding prerequisite -> dependent creates a cycle.
func WouldCycle(snapshot Snapshot, prerequisite, dependent NodeID) (bool, error) {
	if prerequisite == dependent {
		return true, nil
	}
	nodes := make(map[NodeID]Node, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		nodes[node.ID] = node
	}
	if _, ok := nodes[prerequisite]; !ok {
		return false, fmt.Errorf("%w: prerequisite %q", ErrNotFound, prerequisite)
	}
	if _, ok := nodes[dependent]; !ok {
		return false, fmt.Errorf("%w: dependent %q", ErrNotFound, dependent)
	}
	outgoing := make(map[NodeID][]Edge)
	for _, edge := range snapshot.Edges {
		outgoing[edge.Prerequisite] = append(outgoing[edge.Prerequisite], edge)
	}
	stack := []NodeID{dependent}
	seen := map[NodeID]bool{}
	for len(stack) > 0 {
		last := len(stack) - 1
		current := stack[last]
		stack = stack[:last]
		if current == prerequisite {
			return true, nil
		}
		if seen[current] {
			continue
		}
		seen[current] = true
		for _, edge := range outgoing[current] {
			stack = append(stack, edge.Dependent)
		}
	}
	return false, nil
}

// AuthorizedSlice filters a projection without leaking unauthorized graph nodes.
func AuthorizedSlice(
	projection Projection,
	seatID contracts.SeatID,
	departmentID contracts.DepartmentID,
) Projection {
	allowed := make(map[NodeID]bool)
	filtered := Projection{}
	for _, node := range projection.Nodes {
		if node.OwnerSeatID != nil && *node.OwnerSeatID == seatID ||
			node.OwnerDepartmentID != nil && *node.OwnerDepartmentID == departmentID ||
			node.OwnerSeatID == nil && node.OwnerDepartmentID == nil {
			allowed[node.ID] = true
			filtered.Nodes = append(filtered.Nodes, node)
		}
	}
	for _, node := range projection.Eligible {
		if allowed[node.ID] {
			filtered.Eligible = append(filtered.Eligible, node)
		}
	}
	for _, edge := range projection.Edges {
		if allowed[edge.Prerequisite] && allowed[edge.Dependent] {
			filtered.Edges = append(filtered.Edges, edge)
		}
	}
	for _, incident := range projection.Incidents {
		include := false
		for _, id := range incident.NodeIDs {
			include = include || allowed[id]
		}
		if include {
			filtered.Incidents = append(filtered.Incidents, incident)
		}
	}
	return filtered
}

func agedPriority(node Node, now time.Time) int64 {
	const agingInterval = 15 * time.Minute
	age := now.Sub(node.CreatedAt)
	if age < 0 {
		age = 0
	}
	return int64(node.BasePriority)*1_000_000 + int64(age/agingInterval)
}

func sortNodes(nodes []Node, priorities map[NodeID]int64) {
	sort.Slice(nodes, func(i, j int) bool {
		left, right := priorities[nodes[i].ID], priorities[nodes[j].ID]
		if left != right {
			return left > right
		}
		if deadlineBefore(nodes[i].Deadline, nodes[j].Deadline) {
			return true
		}
		if deadlineBefore(nodes[j].Deadline, nodes[i].Deadline) {
			return false
		}
		if !nodes[i].CreatedAt.Equal(nodes[j].CreatedAt) {
			return nodes[i].CreatedAt.Before(nodes[j].CreatedAt)
		}
		return nodes[i].ID < nodes[j].ID
	})
}

func deadlineBefore(left, right *time.Time) bool {
	if left == nil {
		return false
	}
	if right == nil {
		return true
	}
	return left.Before(*right)
}

func findCycle(nodes map[NodeID]Node, outgoing map[NodeID][]Edge) []NodeID {
	color := make(map[NodeID]uint8, len(nodes))
	path := make([]NodeID, 0, len(nodes))
	var visit func(NodeID) []NodeID
	visit = func(id NodeID) []NodeID {
		color[id] = 1
		path = append(path, id)
		for _, edge := range outgoing[id] {
			switch color[edge.Dependent] {
			case 0:
				if cycle := visit(edge.Dependent); len(cycle) > 0 {
					return cycle
				}
			case 1:
				start := 0
				for path[start] != edge.Dependent {
					start++
				}
				return append(append([]NodeID(nil), path[start:]...), edge.Dependent)
			}
		}
		path = path[:len(path)-1]
		color[id] = 2
		return nil
	}
	ids := make([]NodeID, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if color[id] == 0 {
			if cycle := visit(id); len(cycle) > 0 {
				return cycle
			}
		}
	}
	return nil
}

func reverseTopological(
	nodes map[NodeID]Node,
	outgoing map[NodeID][]Edge,
	incoming map[NodeID][]Edge,
) []NodeID {
	degrees := make(map[NodeID]int, len(nodes))
	queue := make([]NodeID, 0)
	for id := range nodes {
		degrees[id] = len(incoming[id])
		if degrees[id] == 0 {
			queue = append(queue, id)
		}
	}
	sort.Slice(queue, func(i, j int) bool { return queue[i] < queue[j] })
	order := make([]NodeID, 0, len(nodes))
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		order = append(order, id)
		for _, edge := range outgoing[id] {
			degrees[edge.Dependent]--
			if degrees[edge.Dependent] == 0 {
				queue = append(queue, edge.Dependent)
				sort.Slice(queue, func(i, j int) bool { return queue[i] < queue[j] })
			}
		}
	}
	for left, right := 0, len(order)-1; left < right; left, right = left+1, right-1 {
		order[left], order[right] = order[right], order[left]
	}
	return order
}

func detectIncidents(
	nodes map[NodeID]Node,
	incoming map[NodeID][]Edge,
	outgoing map[NodeID][]Edge,
	edges []Edge,
	now time.Time,
) []Incident {
	incidents := make([]Incident, 0)
	for id, node := range nodes {
		if node.Kind != NodeGoal && len(incoming[id]) == 0 && len(outgoing[id]) == 0 &&
			!node.State.Terminal() {
			incidents = append(incidents, incident(
				node.OrganizationID, IncidentOrphan, []NodeID{id},
				"Work is disconnected from every prerequisite and consumer.", now,
			))
		}
	}
	for _, edge := range edges {
		if edge.Kind != EdgeDelegation || nodes[edge.Prerequisite].State == StateCompleted {
			continue
		}
		if edge.SLAAt != nil && now.After(*edge.SLAAt) {
			incidents = append(incidents, incident(
				edge.OrganizationID, IncidentSLABreach,
				[]NodeID{edge.Prerequisite, edge.Dependent},
				"Delegated work breached its response SLA.", now,
			))
		}
		if edge.ExpiresAt != nil && !now.Before(*edge.ExpiresAt) {
			incidents = append(incidents, incident(
				edge.OrganizationID, IncidentDelegationExpired,
				[]NodeID{edge.Prerequisite, edge.Dependent},
				"Delegated work expired and requires its configured timeout action.", now,
			))
		}
	}
	pending := make([]NodeID, 0)
	externalUnblock := false
	var organizationID contracts.OrganizationID
	for id, node := range nodes {
		if organizationID == "" {
			organizationID = node.OrganizationID
		}
		if node.State == StatePending || node.State == StateWaiting || node.State == StateContested {
			pending = append(pending, id)
			for _, edge := range incoming[id] {
				if nodes[edge.Prerequisite].State == StateEligible ||
					nodes[edge.Prerequisite].State == StateLeased {
					externalUnblock = true
				}
			}
		}
	}
	if len(pending) > 1 && !externalUnblock {
		sort.Slice(pending, func(i, j int) bool { return pending[i] < pending[j] })
		incidents = append(incidents, incident(
			organizationID, IncidentDeadlock, pending,
			"No pending node has an eligible or leased prerequisite that can advance it.", now,
		))
	}
	sort.Slice(incidents, func(i, j int) bool { return incidents[i].ID < incidents[j].ID })
	return incidents
}

func incident(
	organizationID contracts.OrganizationID,
	kind IncidentKind,
	nodeIDs []NodeID,
	explanation string,
	now time.Time,
) Incident {
	sorted := append([]NodeID(nil), nodeIDs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	input := string(organizationID) + "|" + string(kind)
	for _, id := range sorted {
		input += "|" + string(id)
	}
	sum := sha256.Sum256([]byte(input))
	return Incident{
		ID:             "incident:" + hex.EncodeToString(sum[:16]),
		OrganizationID: organizationID,
		Kind:           kind,
		NodeIDs:        sorted,
		Explanation:    explanation,
		CreatedAt:      now,
	}
}
