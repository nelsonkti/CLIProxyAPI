package auth

import (
	"context"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// TestAuthWeight_ClampingAndDefaults verifies weight parsing/clamping rules:
// unset/blank/invalid/<1 → 1, >100 → 100, otherwise the parsed value.
func TestAuthWeight_ClampingAndDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		attr map[string]string
		want int
	}{
		{"nil_attributes", nil, 1},
		{"missing_weight", map[string]string{"priority": "10"}, 1},
		{"blank_weight", map[string]string{"weight": "   "}, 1},
		{"non_numeric", map[string]string{"weight": "abc"}, 1},
		{"zero_clamps_to_one", map[string]string{"weight": "0"}, 1},
		{"negative_clamps_to_one", map[string]string{"weight": "-5"}, 1},
		{"normal_value", map[string]string{"weight": "7"}, 7},
		{"max_value", map[string]string{"weight": "100"}, 100},
		{"over_max_clamps", map[string]string{"weight": "1000"}, 100},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := authWeight(&Auth{ID: "x", Attributes: tt.attr})
			if got != tt.want {
				t.Fatalf("authWeight() = %d, want %d", got, tt.want)
			}
		})
	}

	if got := authWeight(nil); got != 1 {
		t.Fatalf("authWeight(nil) = %d, want 1", got)
	}
}

// TestRoundRobinSelectorPick_WeightedDistribution verifies that over a full
// cycle (totalW picks), each auth is selected exactly its weight number of times.
func TestRoundRobinSelectorPick_WeightedDistribution(t *testing.T) {
	t.Parallel()

	selector := &RoundRobinSelector{}
	auths := []*Auth{
		{ID: "a", Attributes: map[string]string{"weight": "1"}},
		{ID: "b", Attributes: map[string]string{"weight": "3"}},
		{ID: "c", Attributes: map[string]string{"weight": "2"}},
	}
	// totalW = 6. Over one full cycle each auth should be hit weight-many times.
	const cycle = 6

	counts := make(map[string]int)
	for i := 0; i < cycle; i++ {
		got, err := selector.Pick(context.Background(), "mixed", "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		counts[got.ID]++
	}

	want := map[string]int{"a": 1, "b": 3, "c": 2}
	for id, w := range want {
		if counts[id] != w {
			t.Fatalf("auth %s selected %d times in one cycle, want %d (counts=%v)", id, counts[id], w, counts)
		}
	}
}

// TestRoundRobinSelectorPick_WeightedExactSequence pins the deterministic order
// produced by the cursor→weight-space mapping. auths are sorted by ID inside
// getAvailableAuths, so order is a,b,c with weights 1,3,2 (cumulative 1,4,6).
func TestRoundRobinSelectorPick_WeightedExactSequence(t *testing.T) {
	t.Parallel()

	selector := &RoundRobinSelector{}
	auths := []*Auth{
		{ID: "c", Attributes: map[string]string{"weight": "2"}},
		{ID: "a", Attributes: map[string]string{"weight": "1"}},
		{ID: "b", Attributes: map[string]string{"weight": "3"}},
	}

	// slot = index % 6; cumulative weights: a=[0,1), b=[1,4), c=[4,6).
	// index: 0→a, 1→b, 2→b, 3→b, 4→c, 5→c, then repeats.
	want := []string{"a", "b", "b", "b", "c", "c", "a", "b"}
	for i, id := range want {
		got, err := selector.Pick(context.Background(), "mixed", "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		if got.ID != id {
			t.Fatalf("Pick() #%d auth.ID = %q, want %q", i, got.ID, id)
		}
	}
}

// TestRoundRobinSelectorPick_WeightWithinPriorityTier verifies weighting only
// applies within the highest-priority tier; lower-priority auths are excluded
// regardless of their weight.
func TestRoundRobinSelectorPick_WeightWithinPriorityTier(t *testing.T) {
	t.Parallel()

	selector := &RoundRobinSelector{}
	auths := []*Auth{
		// Lower priority with a huge weight — must never be selected.
		{ID: "low", Attributes: map[string]string{"priority": "0", "weight": "100"}},
		// High priority tier with differing weights.
		{ID: "a", Attributes: map[string]string{"priority": "10", "weight": "1"}},
		{ID: "b", Attributes: map[string]string{"priority": "10", "weight": "3"}},
	}

	counts := make(map[string]int)
	const iterations = 40 // 10 full cycles of totalW=4
	for i := 0; i < iterations; i++ {
		got, err := selector.Pick(context.Background(), "mixed", "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		if got.ID == "low" {
			t.Fatalf("Pick() #%d selected lower-priority auth despite high weight", i)
		}
		counts[got.ID]++
	}

	if counts["low"] != 0 {
		t.Fatalf("low-priority auth selected %d times, want 0", counts["low"])
	}
	// Within the tier (totalW=4): a=1/4, b=3/4 → over 40 picks a=10, b=30.
	if counts["a"] != 10 || counts["b"] != 30 {
		t.Fatalf("within-tier distribution = a:%d b:%d, want a:10 b:30", counts["a"], counts["b"])
	}
}

// TestRoundRobinSelectorPick_UniformWeightFastPath verifies that explicit
// weight="1" on every auth behaves identically to plain round-robin
// (exercises the allUniform fast path).
func TestRoundRobinSelectorPick_UniformWeightFastPath(t *testing.T) {
	t.Parallel()

	selector := &RoundRobinSelector{}
	auths := []*Auth{
		{ID: "b", Attributes: map[string]string{"weight": "1"}},
		{ID: "a", Attributes: map[string]string{"weight": "1"}},
		{ID: "c", Attributes: map[string]string{"weight": "1"}},
	}

	want := []string{"a", "b", "c", "a", "b", "c"}
	for i, id := range want {
		got, err := selector.Pick(context.Background(), "gemini", "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		if got.ID != id {
			t.Fatalf("Pick() #%d auth.ID = %q, want %q", i, got.ID, id)
		}
	}
}

// TestRoundRobinSelectorPick_WeightClampedInSelection verifies that an
// out-of-range weight (>100) is clamped during selection, not used raw.
func TestRoundRobinSelectorPick_WeightClampedInSelection(t *testing.T) {
	t.Parallel()

	selector := &RoundRobinSelector{}
	auths := []*Auth{
		{ID: "a", Attributes: map[string]string{"weight": "1"}},
		{ID: "b", Attributes: map[string]string{"weight": "9999"}}, // clamped to 100
	}
	// totalW = 1 + 100 = 101. Over one cycle: a=1, b=100.
	counts := make(map[string]int)
	const cycle = 101
	for i := 0; i < cycle; i++ {
		got, err := selector.Pick(context.Background(), "mixed", "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		counts[got.ID]++
	}
	if counts["a"] != 1 || counts["b"] != 100 {
		t.Fatalf("clamped distribution = a:%d b:%d, want a:1 b:100", counts["a"], counts["b"])
	}
}
