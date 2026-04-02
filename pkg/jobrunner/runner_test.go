package jobrunner

import (
	"sort"
	"testing"
)

func TestRoundRobinSchedule(t *testing.T) {
	tests := []struct {
		name      string
		nodes     []string
		wantPairs int
		wantNil   bool
	}{
		{name: "single node", nodes: []string{"a"}, wantNil: true},
		{name: "empty", nodes: nil, wantNil: true},
		{name: "2 nodes", nodes: []string{"a", "b"}, wantPairs: 1},
		{name: "3 nodes", nodes: []string{"a", "b", "c"}, wantPairs: 3},
		{name: "4 nodes", nodes: []string{"a", "b", "c", "d"}, wantPairs: 6},
		{name: "5 nodes", nodes: []string{"a", "b", "c", "d", "e"}, wantPairs: 10},
		{name: "6 nodes", nodes: []string{"a", "b", "c", "d", "e", "f"}, wantPairs: 15},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rounds := roundRobinSchedule(tt.nodes)
			if tt.wantNil {
				if rounds != nil {
					t.Fatalf("expected nil, got %d rounds", len(rounds))
				}
				return
			}
			if rounds == nil {
				t.Fatal("unexpected nil rounds")
			}

			// Collect all pairs and verify total count = N*(N-1)/2
			allPairs := make(map[NodePair]bool)
			for _, round := range rounds {
				for _, p := range round {
					if allPairs[p] {
						t.Errorf("duplicate pair: %s↔%s", p.Server, p.Client)
					}
					allPairs[p] = true
				}
			}
			if len(allPairs) != tt.wantPairs {
				t.Errorf("got %d total pairs, want %d", len(allPairs), tt.wantPairs)
			}

			// Verify pairs within each round are disjoint (no node appears twice)
			for ri, round := range rounds {
				seen := make(map[string]bool)
				for _, p := range round {
					if seen[p.Server] {
						t.Errorf("round %d: node %s appears in multiple pairs", ri, p.Server)
					}
					if seen[p.Client] {
						t.Errorf("round %d: node %s appears in multiple pairs", ri, p.Client)
					}
					seen[p.Server] = true
					seen[p.Client] = true
				}
			}

			// Verify consistent ordering (Server < Client lexicographically)
			for _, round := range rounds {
				for _, p := range round {
					if p.Server > p.Client {
						t.Errorf("pair not consistently ordered: Server=%s > Client=%s", p.Server, p.Client)
					}
				}
			}
		})
	}
}

func TestRoundRobinScheduleCoversAllPairs(t *testing.T) {
	nodes := []string{"node-a", "node-b", "node-c", "node-d"}
	rounds := roundRobinSchedule(nodes)

	// Build expected set of all N*(N-1)/2 pairs
	expected := make(map[string]bool)
	sort.Strings(nodes)
	for i := 0; i < len(nodes); i++ {
		for j := i + 1; j < len(nodes); j++ {
			expected[nodes[i]+"↔"+nodes[j]] = true
		}
	}

	got := make(map[string]bool)
	for _, round := range rounds {
		for _, p := range round {
			key := p.Server + "↔" + p.Client
			got[key] = true
		}
	}

	for k := range expected {
		if !got[k] {
			t.Errorf("missing pair: %s", k)
		}
	}
	for k := range got {
		if !expected[k] {
			t.Errorf("unexpected pair: %s", k)
		}
	}
}
