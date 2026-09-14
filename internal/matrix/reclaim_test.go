package matrix

import (
	"reflect"
	"sort"
	"testing"
	"time"
)

// sortedCopy sorts a copy for deterministic comparisons.
func sortedCopy(xs []string) []string {
	out := append([]string(nil), xs...)
	sort.Strings(out)
	return out
}

// newReclaimProgram builds a budget-3 matrix over models a..e plus target t:
// requesting t with three of a..e running admits sets that evict exactly one,
// so every candidate scores the same default raw cost of 1.
func newReclaimProgram(t *testing.T) *Program {
	t.Helper()
	p, err := Compile([]Definition{
		{Name: "pool", DSL: "(t | a | b | c | d | e)"},
		{Name: "all", DSL: "+pool & +pool & +pool"},
	}, func(ident string) (string, bool) { return ident, true })
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return p
}

// ReclaimMinimal (the default, including the empty string) ignores Upcoming:
// exactly one model is evicted, as under the historical objective.
func TestProgram_SolveReclaimMinimalIgnoresUpcoming(t *testing.T) {
	p := newReclaimProgram(t)
	for _, reclaim := range []string{"", ReclaimMinimal} {
		result := p.Solve("t", []string{"a", "b", "c"}, SolveOptions{
			Reclaim:  reclaim,
			Upcoming: []string{"d", "e"},
		})
		if len(result.Evict) != 1 {
			t.Fatalf("reclaim=%q: Evict=%v want exactly one eviction", reclaim, result.Evict)
		}
	}
}

// With a non-empty queue, running models the queue does not reference are
// reclaimed: the evict list covers every running model, not just the one the
// minimum-cost set drops.
func TestProgram_SolveReclaimQueueEvictsUnqueued(t *testing.T) {
	p := newReclaimProgram(t)
	result := p.Solve("t", []string{"a", "b", "c"}, SolveOptions{
		Reclaim:  ReclaimQueue,
		Upcoming: []string{"d", "e"}, // queued but not running
	})
	if !reflect.DeepEqual(sortedCopy(result.Evict), []string{"a", "b", "c"}) {
		t.Fatalf("Evict=%v want [a b c] (all unqueued running models are reclaimed)", result.Evict)
	}
	// The chosen set still permits a and b (subset semantics); it is the
	// evict list — the decision — that drops them.
	found := false
	for _, m := range result.TargetSet {
		if m == "t" {
			found = true
		}
	}
	if !found {
		t.Fatalf("TargetSet=%v must contain the target", result.TargetSet)
	}
}

// Eviction cost protects queued models: a queued model is never evicted while
// a candidate exists that keeps it.
func TestProgram_SolveReclaimQueueProtectsQueued(t *testing.T) {
	p := newReclaimProgram(t)
	// b is queued and expensive; a and c are not in the queue.
	result := p.Solve("t", []string{"a", "b", "c"}, SolveOptions{
		Reclaim:    ReclaimQueue,
		EvictCosts: map[string]int{"b": 10},
		Upcoming:   []string{"b", "d"},
	})
	if len(result.Evict) != 2 {
		t.Fatalf("Evict=%v want two evictions", result.Evict)
	}
	for _, m := range result.Evict {
		if m == "b" {
			t.Fatalf("Evict=%v must not evict queued model b", result.Evict)
		}
	}
	if !reflect.DeepEqual(sortedCopy(result.Evict), []string{"a", "c"}) {
		t.Fatalf("Evict=%v want [a c]", result.Evict)
	}
}

// The charged cost is primary: an expensive queued model is kept even though
// evicting two cheaper unqueued models is available.
func TestProgram_SolveReclaimQueueChargedPrimary(t *testing.T) {
	p := newReclaimProgram(t)
	// a is queued and costs 10. Keeping a evicts b and c (raw 2, charged 0);
	// dropping a evicts one of b or c (raw 1, charged 10). Charged wins.
	result := p.Solve("t", []string{"a", "b", "c"}, SolveOptions{
		Reclaim:    ReclaimQueue,
		EvictCosts: map[string]int{"a": 10},
		Upcoming:   []string{"a"},
	})
	if !reflect.DeepEqual(sortedCopy(result.Evict), []string{"b", "c"}) {
		t.Fatalf("Evict=%v want [b c] (queued a is kept despite the higher raw cost)", result.Evict)
	}
}

// Queue reclaim with an empty queue behaves exactly as minimal: the evict
// list is the set complement only.
func TestProgram_SolveReclaimQueueEmptyQueueIsMinimal(t *testing.T) {
	p := newReclaimProgram(t)
	result := p.Solve("t", []string{"a", "b", "c"}, SolveOptions{
		Reclaim:  ReclaimQueue,
		Upcoming: nil,
	})
	if len(result.Evict) != 1 {
		t.Fatalf("Evict=%v want exactly one eviction (empty queue falls back to minimal)", result.Evict)
	}
}

// Reclaim plus lru: the lru tie-breaker still orders the candidate sets (the
// chosen set keeps the two models it evicts the idlest of), and the reclaim
// extension applies on top of the winner.
func TestProgram_SolveReclaimQueueWithLRU(t *testing.T) {
	p := newReclaimProgram(t)
	result := p.Solve("t", []string{"a", "b", "c"}, SolveOptions{
		Reclaim:    ReclaimQueue,
		TieBreaker: TieBreakerLRU,
		Idle: map[string]time.Duration{
			"a": 10 * time.Minute,
			"b": time.Hour,
			"c": time.Minute,
		},
		Upcoming: []string{"d"},
	})
	// All candidates charge 0 and cost 1; lru picks the set evicting the
	// longest-idle model (b), then the reclaim extension drops a and c as
	// well: everything unqueued goes.
	if !reflect.DeepEqual(sortedCopy(result.Evict), []string{"a", "b", "c"}) {
		t.Fatalf("Evict=%v want [a b c]", result.Evict)
	}
	if !reflect.DeepEqual(sortedCopy(result.TargetSet), []string{"a", "c", "t"}) {
		t.Fatalf("TargetSet=%v want [a c t] (the lru winner keeps a and c)", result.TargetSet)
	}
}
