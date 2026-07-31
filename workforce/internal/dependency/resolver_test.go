package dependency

import (
	"errors"
	"math/rand/v2"
	"testing"
	"time"

	"matrix/workforce/internal/contracts"
)

func TestResolve_OrdersEligibleNodesAndInheritsPriority_Deterministically(t *testing.T) {
	now := time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
	seat := contracts.SeatID("seat:developer:executor")
	department := contracts.DepartmentID("department:developer")
	nodes := []Node{
		testNode("root", NodeGoal, StateCompleted, 1, now.Add(-time.Hour), &seat, nil),
		testNode("blocker", NodeIntent, StatePending, 0, now.Add(-30*time.Minute), &seat, &department),
		testNode("urgent", NodeIntent, StatePending, 900, now.Add(-time.Minute), &seat, &department),
		testNode("aged", NodeIntent, StatePending, 900, now.Add(-time.Hour), &seat, &department),
		testNode("ordinary", NodeIntent, StatePending, 100, now.Add(-time.Hour), &seat, &department),
	}
	edges := []Edge{
		testEdge("root", "blocker", now),
		testEdge("blocker", "urgent", now),
		testEdge("root", "aged", now),
	}
	projection, err := Resolve(Snapshot{Nodes: nodes, Edges: edges}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Eligible) != 3 {
		t.Fatalf("eligible count = %d, want 3", len(projection.Eligible))
	}
	if projection.Eligible[0].ID != "aged" {
		t.Fatalf("starvation aging did not order equal-priority work: %+v", projection.Eligible)
	}
	if projection.Eligible[1].ID != "blocker" {
		t.Fatalf("priority inheritance did not promote blocker: %+v", projection.Eligible)
	}
	slice := AuthorizedSlice(projection, seat, department)
	if len(slice.Nodes) != len(projection.Nodes) || len(slice.Edges) != len(projection.Edges) {
		t.Fatalf("authorized slice lost owned graph: %+v", slice)
	}
}

func TestResolve_RejectsCycleAndMissingEndpoint_BeforeEligibility(t *testing.T) {
	now := time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
	nodes := []Node{
		testNode("a", NodeIntent, StatePending, 0, now, nil, nil),
		testNode("b", NodeIntent, StatePending, 0, now, nil, nil),
	}
	_, err := Resolve(Snapshot{
		Nodes: nodes,
		Edges: []Edge{testEdge("a", "b", now), testEdge("b", "a", now)},
	}, now)
	if !errors.Is(err, ErrCycle) {
		t.Fatalf("cycle error = %v, want ErrCycle", err)
	}
	_, err = Resolve(Snapshot{
		Nodes: nodes,
		Edges: []Edge{testEdge("missing", "a", now)},
	}, now)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing endpoint error = %v, want ErrNotFound", err)
	}
	_, err = Resolve(Snapshot{
		Nodes: nodes,
		Edges: []Edge{testEdge("a", "missing", now)},
	}, now)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing dependent error = %v, want ErrNotFound", err)
	}
}

func TestResolve_ReportsOrphanDeadlockSLAAndExpiry_WithStableIdentity(t *testing.T) {
	now := time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
	expiry := now.Add(-time.Minute)
	sla := now.Add(-time.Hour)
	created := now.Add(-2 * time.Hour)
	nodes := []Node{
		testNode("delegated", NodeDelegation, StateWaiting, 0, created, nil, nil),
		testNode("consumer", NodeIntent, StatePending, 0, created, nil, nil),
		testNode("orphan", NodeIntent, StatePending, 0, created, nil, nil),
	}
	edge := Edge{
		OrganizationID:         "organization:test",
		Prerequisite:           "delegated",
		Dependent:              "consumer",
		Kind:                   EdgeDelegation,
		RequiredResponseSchema: "workforce.response.v1",
		ExpiresAt:              &expiry,
		TimeoutAction:          contracts.TimeoutEscalate,
		SLAAt:                  &sla,
		CreatedAt:              created,
	}
	left, err := Resolve(Snapshot{Nodes: nodes, Edges: []Edge{edge}}, now)
	if err != nil {
		t.Fatal(err)
	}
	right, err := Resolve(Snapshot{Nodes: nodes, Edges: []Edge{edge}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(left.Incidents) < 4 {
		t.Fatalf("incident count = %d, want orphan, deadlock, SLA, expiry", len(left.Incidents))
	}
	for index := range left.Incidents {
		if left.Incidents[index].ID != right.Incidents[index].ID {
			t.Fatalf("incident identity changed: %q != %q",
				left.Incidents[index].ID, right.Incidents[index].ID)
		}
	}
}

func TestProperty_AcyclicInsertionPreservesTopologicalEligibility(t *testing.T) {
	now := time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
	for seed := uint64(1); seed <= 100; seed++ {
		random := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
		nodes := make([]Node, 12)
		for index := range nodes {
			state := StatePending
			if index < 3 {
				state = StateCompleted
			}
			nodes[index] = testNode(
				NodeID("node-"+string(rune('a'+index))),
				NodeIntent,
				state,
				int32(random.IntN(2001)-1000),
				now.Add(-time.Duration(index)*time.Minute),
				nil,
				nil,
			)
		}
		edges := make([]Edge, 0)
		for from := 0; from < len(nodes); from++ {
			for to := from + 1; to < len(nodes); to++ {
				if random.IntN(5) == 0 {
					edges = append(edges, testEdge(nodes[from].ID, nodes[to].ID, now))
				}
			}
		}
		projection, err := Resolve(Snapshot{Nodes: nodes, Edges: edges}, now)
		if err != nil {
			t.Fatalf("seed %d: resolve: %v", seed, err)
		}
		for _, eligible := range projection.Eligible {
			for _, edge := range edges {
				if edge.Dependent == eligible.ID {
					prerequisite := nodes[indexOf(nodes, edge.Prerequisite)]
					if prerequisite.State != StateCompleted {
						t.Fatalf("seed %d: %q eligible behind %q in state %q",
							seed, eligible.ID, prerequisite.ID, prerequisite.State)
					}
				}
			}
		}
	}
}

func TestNodeAndEdgeValidation_RejectsEveryInvalidBoundary(t *testing.T) {
	now := time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
	valid := testNode("node", NodeIntent, StatePending, 0, now, nil, nil)
	nodeCases := []Node{
		func() Node { value := valid; value.ID = ""; return value }(),
		func() Node { value := valid; value.OrganizationID = ""; return value }(),
		func() Node { value := valid; value.Kind = "unknown"; return value }(),
		func() Node { value := valid; value.State = "unknown"; return value }(),
		func() Node { value := valid; value.Title = ""; return value }(),
		func() Node { value := valid; value.BasePriority = 1001; return value }(),
		func() Node { value := valid; value.CreatedAt = time.Time{}; return value }(),
		func() Node { value := valid; value.UpdatedAt = now.Add(-time.Second); return value }(),
		func() Node { value := valid; deadline := now; value.Deadline = &deadline; return value }(),
		func() Node { value := valid; value.State = StateCancelled; return value }(),
		func() Node { value := valid; value.CancellationReason = "unexpected"; return value }(),
		func() Node { value := valid; value.Version = 0; return value }(),
	}
	for index, candidate := range nodeCases {
		if err := candidate.Validate(); err == nil {
			t.Fatalf("node case %d accepted: %+v", index, candidate)
		}
	}
	for _, kind := range []NodeKind{
		NodeGoal, NodeIntent, NodeDelegation, NodeHandoff, NodeArtifact,
		NodeApproval, NodeTerminalOutcome,
	} {
		if !kind.Valid() {
			t.Fatalf("valid node kind rejected: %s", kind)
		}
	}
	for _, state := range []NodeState{
		StatePending, StateEligible, StateLeased, StateWaiting, StateCompleted,
		StateCancelled, StateFailed, StateContested,
	} {
		if !state.Valid() {
			t.Fatalf("valid node state rejected: %s", state)
		}
	}
	for _, kind := range []EdgeKind{
		EdgeDependency, EdgeDelegation, EdgeHandoff, EdgeArtifact,
		EdgeApproval, EdgeCorrection,
	} {
		if !kind.Valid() {
			t.Fatalf("valid edge kind rejected: %s", kind)
		}
	}
	if NodeKind("unknown").Valid() || NodeState("unknown").Valid() ||
		EdgeKind("unknown").Valid() {
		t.Fatal("unknown enum accepted")
	}

	validEdge := testEdge("a", "b", now)
	edgeCases := []Edge{
		func() Edge { value := validEdge; value.OrganizationID = ""; return value }(),
		func() Edge { value := validEdge; value.Prerequisite = ""; return value }(),
		func() Edge { value := validEdge; value.Dependent = ""; return value }(),
		func() Edge { value := validEdge; value.Dependent = "a"; return value }(),
		func() Edge { value := validEdge; value.Kind = "unknown"; return value }(),
		func() Edge { value := validEdge; value.CreatedAt = time.Time{}; return value }(),
		func() Edge { value := validEdge; value.RequiredResponseSchema = "forbidden"; return value }(),
		func() Edge { value := validEdge; value.Kind = EdgeDelegation; return value }(),
	}
	for index, candidate := range edgeCases {
		if err := candidate.Validate(); err == nil {
			t.Fatalf("edge case %d accepted: %+v", index, candidate)
		}
	}
	expires := now.Add(time.Hour)
	sla := now.Add(30 * time.Minute)
	delegation := Edge{
		OrganizationID: "organization:test", Prerequisite: "a", Dependent: "b",
		Kind: EdgeDelegation, RequiredResponseSchema: "response.v1",
		ExpiresAt: &expires, TimeoutAction: contracts.TimeoutCancel,
		SLAAt: &sla, CreatedAt: now,
	}
	if err := delegation.Validate(); err != nil {
		t.Fatalf("valid delegation rejected: %v", err)
	}
	badDelegation := delegation
	badDelegation.SLAAt = badDelegation.ExpiresAt
	expired := now.Add(-time.Minute)
	badDelegation.ExpiresAt = &expired
	if err := badDelegation.Validate(); err == nil {
		t.Fatal("misordered delegation accepted")
	}
}

func TestResolve_DeadlineAndAuthorizationBranches_AreClosed(t *testing.T) {
	now := time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
	soon, later := now.Add(time.Hour), now.Add(2*time.Hour)
	seatA, seatB := contracts.SeatID("seat:a"), contracts.SeatID("seat:b")
	departmentA, departmentB := contracts.DepartmentID("department:a"), contracts.DepartmentID("department:b")
	nodes := []Node{
		testNode("public", NodeGoal, StatePending, 0, now, nil, nil),
		testNode("seat-a", NodeIntent, StatePending, 0, now, &seatA, &departmentA),
		testNode("seat-b", NodeIntent, StatePending, 0, now, &seatB, &departmentB),
		testNode("later", NodeIntent, StatePending, 0, now, &seatA, &departmentA),
	}
	nodes[1].Deadline = &soon
	nodes[3].Deadline = &later
	projection, err := Resolve(Snapshot{Nodes: nodes}, now)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Eligible[0].ID != "seat-a" {
		t.Fatalf("deadline ordering = %q, want seat-a", projection.Eligible[0].ID)
	}
	slice := AuthorizedSlice(projection, seatA, departmentA)
	if len(slice.Nodes) != 3 {
		t.Fatalf("authorized nodes = %d, want public plus department A", len(slice.Nodes))
	}
	if _, err := Resolve(Snapshot{Nodes: nodes}, time.Time{}); err == nil {
		t.Fatal("zero resolver time accepted")
	}
	if cycle, err := WouldCycle(Snapshot{Nodes: nodes}, "missing", "public"); err == nil || cycle {
		t.Fatalf("missing prerequisite result = %v, %v", cycle, err)
	}
	if cycle, err := WouldCycle(Snapshot{Nodes: nodes}, "public", "missing"); err == nil || cycle {
		t.Fatalf("missing dependent result = %v, %v", cycle, err)
	}
	if cycle, err := WouldCycle(Snapshot{Nodes: nodes}, "public", "public"); err != nil || !cycle {
		t.Fatalf("self-cycle result = %v, %v", cycle, err)
	}
	diamondNodes := []Node{
		testNode("d", NodeIntent, StatePending, 0, now, nil, nil),
		testNode("a", NodeIntent, StatePending, 0, now, nil, nil),
		testNode("b", NodeIntent, StatePending, 0, now, nil, nil),
		testNode("c", NodeIntent, StatePending, 0, now, nil, nil),
		testNode("x", NodeIntent, StatePending, 0, now, nil, nil),
	}
	diamondEdges := []Edge{
		testEdge("d", "a", now), testEdge("d", "b", now),
		testEdge("a", "c", now), testEdge("b", "c", now),
	}
	if cycle, err := WouldCycle(Snapshot{Nodes: diamondNodes, Edges: diamondEdges}, "x", "d"); err != nil || cycle {
		t.Fatalf("diamond traversal result = %v, %v", cycle, err)
	}
}

func TestResolve_RejectsDuplicateAndMalformedGraphAndHandlesFutureCreation(t *testing.T) {
	now := time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
	node := testNode("node", NodeIntent, StatePending, 0, now.Add(time.Minute), nil, nil)
	if _, err := Resolve(Snapshot{Nodes: []Node{node, node}}, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate node error = %v, want ErrConflict", err)
	}
	invalid := testEdge("node", "other", now)
	invalid.Kind = "unknown"
	if _, err := Resolve(Snapshot{
		Nodes: []Node{node, testNode("other", NodeIntent, StatePending, 0, now, nil, nil)},
		Edges: []Edge{invalid},
	}, now); err == nil {
		t.Fatal("invalid edge accepted")
	}
	if _, err := Resolve(Snapshot{Nodes: []Node{node}}, now); err != nil {
		t.Fatalf("future-created node failed bounded aging: %v", err)
	}
	badToken := testNode("bad token", NodeIntent, StatePending, 0, now, nil, nil)
	if err := badToken.Validate(); err == nil {
		t.Fatal("invalid token character accepted")
	}
	invalidNode := node
	invalidNode.Title = ""
	if _, err := Resolve(Snapshot{Nodes: []Node{invalidNode}}, now); err == nil {
		t.Fatal("resolver accepted invalid node")
	}
	multiEdges := []Edge{
		testEdge("node", "other", now),
		func() Edge {
			edge := testEdge("node", "other", now)
			edge.Kind = EdgeArtifact
			return edge
		}(),
	}
	if _, err := Resolve(Snapshot{
		Nodes: []Node{node, testNode("other", NodeIntent, StatePending, 0, now, nil, nil)},
		Edges: multiEdges,
	}, now); err != nil {
		t.Fatalf("multiple typed edges rejected: %v", err)
	}
	leased := testNode("leased", NodeIntent, StateLeased, 0, now, nil, nil)
	waiting := testNode("waiting", NodeIntent, StatePending, 0, now, nil, nil)
	if projection, err := Resolve(Snapshot{
		Nodes: []Node{leased, waiting},
		Edges: []Edge{testEdge("leased", "waiting", now)},
	}, now); err != nil || len(projection.Incidents) != 0 {
		t.Fatalf("leased unblock path reported deadlock: %+v, %v", projection, err)
	}
}

func testNode(
	id NodeID,
	kind NodeKind,
	state NodeState,
	priority int32,
	created time.Time,
	seat *contracts.SeatID,
	department *contracts.DepartmentID,
) Node {
	reason := ""
	if state == StateCancelled {
		reason = "cancelled"
	}
	var terminal *contracts.RecordID
	if state == StateCompleted || state == StateFailed {
		value := contracts.RecordID("receipt:" + string(id))
		terminal = &value
	}
	return Node{
		ID:                 id,
		OrganizationID:     "organization:test",
		Kind:               kind,
		OwnerSeatID:        seat,
		OwnerDepartmentID:  department,
		Title:              string(id),
		State:              state,
		BasePriority:       priority,
		CreatedAt:          created,
		UpdatedAt:          created,
		CancellationReason: reason,
		TerminalRecordID:   terminal,
		Version:            1,
	}
}

func testEdge(prerequisite, dependent NodeID, created time.Time) Edge {
	return Edge{
		OrganizationID: "organization:test",
		Prerequisite:   prerequisite,
		Dependent:      dependent,
		Kind:           EdgeDependency,
		CreatedAt:      created,
	}
}

func indexOf(nodes []Node, id NodeID) int {
	for index := range nodes {
		if nodes[index].ID == id {
			return index
		}
	}
	return -1
}
