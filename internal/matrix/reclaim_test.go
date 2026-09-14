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

// A non-empty queue does not widen the evict list: the count stays minimal
// (one for a full budget-3 fleet) so the fleet remains full and each queued
// connection meets exactly one idle model. It is the *choice* of idle model
// the queue steers, not the count.
func TestProgram_SolveReclaimQueueKeepsMinimalCount(t *testing.T) {
	p := newReclaimProgram(t)
	result := p.Solve("t", []string{"a", "b", "c"}, SolveOptions{
		Reclaim:  ReclaimQueue,
		Upcoming: []string{"d", "e"}, // queued but not running
	})
	if len(result.Evict) != 1 {
		t.Fatalf("Evict=%v want exactly one eviction (the queue must not widen the evict list)", result.Evict)
	}
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

// Eviction cost protects queued models: a running model still in the queue is
// kept and an idle unqueued model is evicted instead. The count is still one.
func TestProgram_SolveReclaimQueueProtectsQueued(t *testing.T) {
	p := newReclaimProgram(t)
	// b is queued and expensive; a and c are not in the queue.
	result := p.Solve("t", []string{"a", "b", "c"}, SolveOptions{
		Reclaim:    ReclaimQueue,
		EvictCosts: map[string]int{"b": 10},
		Upcoming:   []string{"b", "d"},
	})
	if len(result.Evict) != 1 {
		t.Fatalf("Evict=%v want one eviction (turn over one idle model)", result.Evict)
	}
	if result.Evict[0] == "b" {
		t.Fatalf("Evict=%v must not evict queued model b", result.Evict)
	}
}

// The charged cost is primary: an expensive queued model is kept and a single
// cheaper unqueued model is evicted instead.
func TestProgram_SolveReclaimQueueChargedPrimary(t *testing.T) {
	p := newReclaimProgram(t)
	// a is queued and costs 10. Keeping a evicts one of b or c (charged 0);
	// dropping a evicts a itself (charged 10). Charged wins: a is kept.
	result := p.Solve("t", []string{"a", "b", "c"}, SolveOptions{
		Reclaim:    ReclaimQueue,
		EvictCosts: map[string]int{"a": 10},
		Upcoming:   []string{"a"},
	})
	if len(result.Evict) != 1 {
		t.Fatalf("Evict=%v want one eviction", result.Evict)
	}
	if result.Evict[0] == "a" {
		t.Fatalf("Evict=%v must keep queued a despite its higher raw cost", result.Evict)
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

// Reclaim plus lru: the lru tie-breaker orders the candidate sets and the
// single evicted model is the longest-idle running one. The queue does not
// widen the evict list.
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
	// All candidates charge 0 and cost 1; lru evicts the longest-idle model
	// (b) and only that one, so a and c keep running.
	if !reflect.DeepEqual(result.Evict, []string{"b"}) {
		t.Fatalf("Evict=%v want [b] (the single longest-idle running model)", result.Evict)
	}
	if !reflect.DeepEqual(sortedCopy(result.TargetSet), []string{"a", "c", "t"}) {
		t.Fatalf("TargetSet=%v want [a c t] (the lru winner keeps a and c)", result.TargetSet)
	}
}

// The parallel-drain property the queue reclaim exists for: with a full fleet
// and a pending queue, each queued target evicts a *singleton* — one idle
// model — so distinct targets can have disjoint evict sets and the
// scheduler's non-intersection check lets their swaps run in parallel. Each
// idle model in the tie is met by exactly one queued connection. The old
// over-eviction made every target evict the whole fleet, so all targets
// collided and serialized onto a single running model.
func TestProgram_SolveReclaimQueueDisjointSingletons(t *testing.T) {
	p := newReclaimProgram(t)
	running := []string{"a", "b", "c"}
	// Two queued targets decided against the same full fleet. Their idle maps
	// differ so the lru tie-breaker points each at a different idle model.
	d := p.Solve("d", running, SolveOptions{
		Reclaim:    ReclaimQueue,
		TieBreaker: TieBreakerLRU,
		Idle:       map[string]time.Duration{"a": 3 * time.Minute, "b": time.Minute, "c": 2 * time.Minute},
		Upcoming:   []string{"e"},
	})
	e := p.Solve("e", running, SolveOptions{
		Reclaim:    ReclaimQueue,
		TieBreaker: TieBreakerLRU,
		Idle:       map[string]time.Duration{"b": 5 * time.Minute, "a": time.Minute, "c": time.Minute},
		Upcoming:   []string{"d"},
	})
	if !reflect.DeepEqual(d.Evict, []string{"a"}) {
		t.Fatalf("target d evict=%v want [a] (its idlest running model)", d.Evict)
	}
	if !reflect.DeepEqual(e.Evict, []string{"b"}) {
		t.Fatalf("target e evict=%v want [b] (its idlest running model)", e.Evict)
	}
	// Disjoint singletons: the two swaps can run in parallel.
	for _, m := range d.Evict {
		for _, n := range e.Evict {
			if m == n {
				t.Fatalf("evict sets %v and %v intersect; they must be disjoint", d.Evict, e.Evict)
			}
		}
	}
}
