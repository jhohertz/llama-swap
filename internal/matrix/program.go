package matrix

import (
	"fmt"
	"sort"
	"time"
)

// Definition is one named matrix DSL expression.
type Definition struct {
	Name string
	DSL  string
}

// Tie-breaker policies for candidate sets that score the same eviction
// cost. Eviction cost is always the primary key; the tie-breaker only
// orders equal-cost candidates.
const (
	// TieBreakerLexical keeps the first equal-cost candidate in
	// set-definition order. This is the historical behaviour and the
	// default.
	TieBreakerLexical = "lexical"
	// TieBreakerLRU prefers the equal-cost candidate that evicts the
	// longest-idle running model.
	TieBreakerLRU = "lru"
)

// Reclaim policies for the eviction objective. ReclaimMinimal (the default)
// minimises the total eviction cost of the running models dropped by the
// chosen set. ReclaimQueue, used while the scheduler has a pending queue of
// requests, charges eviction cost only for models the queue references and
// reclaims every running model the queue does not need, so the fleet
// converges on the work that is coming.
const (
	// ReclaimMinimal is the historical objective: minimise total eviction
	// cost. Upcoming is ignored.
	ReclaimMinimal = "minimal"
	// ReclaimQueue charges eviction cost only for models referenced by the
	// pending queue and additionally reclaims running models the queue does
	// not reference (the chosen set still permits them — subset semantics).
	// With an empty queue it behaves as ReclaimMinimal.
	ReclaimQueue = "queue"
)

// SolveOptions configures one Solve call.
type SolveOptions struct {
	// EvictCosts is the relative cost of evicting a running model; models
	// not listed cost 1.
	EvictCosts map[string]int
	// TieBreaker selects the policy for equal-cost candidates
	// (TieBreakerLexical or TieBreakerLRU; the empty string behaves as
	// lexical).
	TieBreaker string
	// Idle holds how long each running model has gone without finishing a
	// request. It is consulted only when TieBreaker is TieBreakerLRU; a
	// model absent from the map is treated as idle for zero.
	Idle map[string]time.Duration
	// Reclaim selects the eviction objective (ReclaimMinimal or
	// ReclaimQueue; the empty string behaves as minimal).
	Reclaim string
	// Upcoming is the pending request queue as an ordered, de-duplicated
	// list of model IDs. It is consulted only when Reclaim is
	// ReclaimQueue; with an empty list ReclaimQueue behaves as
	// ReclaimMinimal.
	Upcoming []string
}

// Resolver maps a DSL identifier to a real model name.
type Resolver func(string) (string, bool)

type compiledSet struct {
	name string
	dsl  string
	root *node
	// support has a bit for every model reachable in this set's expression,
	// using the program-wide modelBits registry. A query whose required
	// models are not all in support can skip evaluating this set entirely.
	support bitMask
}

// Program is an immutable, compiled matrix DSL program.
type Program struct {
	sets      []compiledSet
	modelBits map[string]int
	words     int
}

// Decision describes the selected matrix set and the running models it evicts.
type Decision struct {
	Evict     []string
	TargetSet []string
	SetName   string
	DSL       string
	TotalCost int
}

// Compile parses, validates, resolves, and links an ordered collection of DSL
// definitions without expanding their Cartesian products.
func Compile(definitions []Definition, resolve Resolver) (*Program, error) {
	if len(definitions) == 0 {
		return nil, fmt.Errorf("matrix must define at least one set")
	}

	roots := make(map[string]*node, len(definitions))
	dslByName := make(map[string]string, len(definitions))
	deps := make(map[string][]string, len(definitions))
	setNames := make(map[string]bool, len(definitions))
	for _, definition := range definitions {
		setNames[definition.Name] = true
	}

	for _, definition := range definitions {
		root, err := parseDSL(definition.DSL)
		if err != nil {
			return nil, fmt.Errorf("set %q: %w", definition.Name, err)
		}

		var refs []string
		seenRefs := make(map[string]bool)
		if err := walk(root, func(n *node) error {
			switch n.kind {
			case nodeLeaf:
				model, ok := resolve(n.name)
				if !ok {
					return fmt.Errorf("set %q: unknown var or model %q", definition.Name, n.name)
				}
				n.name = model
			case nodeRef:
				if !setNames[n.name] {
					return fmt.Errorf("set %q references undefined set %q", definition.Name, n.name)
				}
				if !seenRefs[n.name] {
					seenRefs[n.name] = true
					refs = append(refs, n.name)
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}

		roots[definition.Name] = root
		dslByName[definition.Name] = definition.DSL
		deps[definition.Name] = refs
	}

	order, err := topologicalOrder(definitions, deps)
	if err != nil {
		return nil, err
	}

	for _, root := range roots {
		if err := walk(root, func(n *node) error {
			if n.kind == nodeRef {
				n.ref = roots[n.name]
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}

	sets := make([]compiledSet, 0, len(order))
	for _, name := range order {
		sets = append(sets, compiledSet{
			name: name,
			dsl:  dslByName[name],
			root: roots[name],
		})
	}

	program := &Program{sets: sets}
	program.computeSupport()
	return program, nil
}

// computeSupport builds the program-wide model registry and, for each set, a
// bitmask of every model its expression can mention. Support is a union over
// all leaves reachable through AND, OR, and references.
func (p *Program) computeSupport() {
	memo := make(map[*node]map[string]bool)
	var collect func(n *node) map[string]bool
	collect = func(n *node) map[string]bool {
		if models, ok := memo[n]; ok {
			return models
		}
		var models map[string]bool
		switch n.kind {
		case nodeLeaf:
			models = map[string]bool{n.name: true}
		case nodeRef:
			models = collect(n.ref)
		default:
			models = make(map[string]bool)
			for _, child := range n.children {
				for model := range collect(child) {
					models[model] = true
				}
			}
		}
		memo[n] = models
		return models
	}

	p.modelBits = make(map[string]int)
	setModels := make([]map[string]bool, len(p.sets))
	for i := range p.sets {
		setModels[i] = collect(p.sets[i].root)
		for model := range setModels[i] {
			if _, ok := p.modelBits[model]; !ok {
				p.modelBits[model] = len(p.modelBits)
			}
		}
	}

	p.words = (len(p.modelBits) + 63) / 64
	for i := range p.sets {
		support := make(bitMask, p.words)
		for model := range setModels[i] {
			bit := p.modelBits[model]
			support[bit/64] |= uint64(1) << uint(bit%64)
		}
		p.sets[i].support = support
	}
}

// supportsAll reports whether every model can appear in the set's expression.
// A set that fails this check can never produce a matching state for a query
// requiring those models.
func (p *Program) supportsAll(set *compiledSet, models []string) bool {
	for _, model := range models {
		bit, ok := p.modelBits[model]
		if !ok || !set.support.has(bit) {
			return false
		}
	}
	return true
}

func topologicalOrder(definitions []Definition, deps map[string][]string) ([]string, error) {
	state := make(map[string]int, len(definitions))
	order := make([]string, 0, len(definitions))

	var visit func(string) error
	visit = func(name string) error {
		switch state[name] {
		case 1:
			return fmt.Errorf("circular reference detected involving set %q", name)
		case 2:
			return nil
		}
		state[name] = 1
		for _, dependency := range deps[name] {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		state[name] = 2
		order = append(order, name)
		return nil
	}

	for _, definition := range definitions {
		if state[definition.Name] == 0 {
			if err := visit(definition.Name); err != nil {
				return nil, err
			}
		}
	}
	return order, nil
}

// Solve chooses the compatible set with the lowest eviction cost. It performs
// a fresh projection because the relevant model universe changes with the
// running set supplied by the scheduler.
//
// Candidate sets are ordered, best first: (1) when ReclaimQueue is active, the
// cost of evicting models the pending queue references — evicting only
// unqueued models scores zero; (2) the raw eviction cost (the historical
// objective); (3) the tie-breaker: lexical (the default) keeps the first
// candidate in set-definition order, lru prefers the candidate that evicts
// the longest-idle running model.
//
// The evict list is then derived from the winning set. Under ReclaimQueue it
// is extended beyond "running minus the set": every running model the set
// permits (subset semantics) but the queue does not reference is reclaimed as
// well, so a backlog drains into parallel loads instead of one eviction per
// request. Eviction cost still protects queued models; an unqueued model with
// a high evict_cost is reclaimed while the queue references no request for
// it and is reloaded if a fresh request arrives.
func (p *Program) Solve(target string, running []string, opts SolveOptions) Decision {
	if contains(running, target) {
		setName, dsl := p.findContaining(running)
		return Decision{
			TargetSet: running,
			SetName:   setName,
			DSL:       dsl,
		}
	}

	relevant := make([]string, 0, len(running)+1)
	relevant = append(relevant, target)
	relevant = append(relevant, running...)
	evaluator := newEvaluator(relevant)
	targetBit := evaluator.modelBits[target]

	// Skip every set when the target appears in no set's expression.
	globalTargetBit, targetKnown := p.modelBits[target]

	lru := opts.TieBreaker == TieBreakerLRU
	// Queue reclaim is active only with a non-empty queue; with an empty
	// queue the objective (and the evict list) fall back to minimal.
	queueReclaim := opts.Reclaim == ReclaimQueue && len(opts.Upcoming) > 0
	var queued map[string]bool
	if queueReclaim {
		queued = make(map[string]bool, len(opts.Upcoming))
		for _, model := range opts.Upcoming {
			queued[model] = true
		}
	}

	best := solveRank{}
	bestValid := false
	var bestSet *compiledSet
	var bestState projectedState
	for i := range p.sets {
		set := &p.sets[i]
		if !targetKnown || !set.support.has(globalTargetBit) {
			continue
		}
		for _, state := range evaluator.evaluate(set.root) {
			if !state.mask.has(targetBit) {
				continue
			}

			rank := solveRank{}
			var evicted []string
			for _, model := range running {
				if !state.mask.has(evaluator.modelBits[model]) {
					c := evictionCost(opts.EvictCosts, model)
					rank.cost += c
					if queueReclaim && queued[model] {
						rank.charged += c
					}
					evicted = append(evicted, model)
				}
			}
			rank.idle = idleRank(evicted, opts.Idle)

			if !bestValid || rank.less(best, queueReclaim, lru) {
				best = rank
				bestValid = true
				bestSet = set
				bestState = state
			}
		}
	}

	if bestSet == nil {
		return Decision{
			Evict:     append([]string(nil), running...),
			TargetSet: []string{target},
		}
	}

	var evict []string
	for _, model := range running {
		switch {
		case !bestState.mask.has(evaluator.modelBits[model]):
			// The set drops this model.
			evict = append(evict, model)
		case queueReclaim && !queued[model]:
			// The set permits this model (subset semantics) but the pending
			// queue does not reference it: reclaim the slot so queued work
			// can load in parallel.
			evict = append(evict, model)
		}
	}

	totalCost := 0
	for _, model := range evict {
		totalCost += evictionCost(opts.EvictCosts, model)
	}
	return Decision{
		Evict:     evict,
		TargetSet: flattenWitness(bestState.witness),
		SetName:   bestSet.name,
		DSL:       bestSet.dsl,
		TotalCost: totalCost,
	}
}

// solveRank scores one candidate set. Lower is better; the ordering depends
// on the reclaim and tie-breaker policies (see Solve).
type solveRank struct {
	// charged is the eviction cost of queued models (queue reclaim only).
	charged int
	// cost is the raw eviction cost of every model the set drops.
	cost int
	// idle is the longest idle time among the models the set drops (lru
	// tie-breaker only). Higher is better.
	idle time.Duration
}

// less reports whether r orders before o. A false result on an exact tie
// keeps the first candidate in set-definition order, so outcomes stay
// deterministic.
func (r solveRank) less(o solveRank, queueReclaim, lru bool) bool {
	if queueReclaim && r.charged != o.charged {
		return r.charged < o.charged
	}
	if r.cost != o.cost {
		return r.cost < o.cost
	}
	if lru && r.idle != o.idle {
		return r.idle > o.idle
	}
	return false
}

// CanContainAll reports whether one generated set contains every supplied model.
func (p *Program) CanContainAll(models []string) bool {
	if len(models) == 0 {
		return true
	}

	evaluator := newEvaluator(models)
	required := evaluator.fullMask()
	for i := range p.sets {
		set := &p.sets[i]
		if !p.supportsAll(set, models) {
			continue
		}
		for _, state := range evaluator.evaluate(set.root) {
			if required.subsetOf(state.mask) {
				return true
			}
		}
	}
	return false
}

func (p *Program) findContaining(models []string) (string, string) {
	if len(models) == 0 {
		return "", ""
	}

	evaluator := newEvaluator(models)
	required := evaluator.fullMask()
	for i := range p.sets {
		set := &p.sets[i]
		if !p.supportsAll(set, models) {
			continue
		}
		for _, state := range evaluator.evaluate(set.root) {
			if required.subsetOf(state.mask) {
				return set.name, set.dsl
			}
		}
	}
	return "", ""
}

func evictionCost(costs map[string]int, model string) int {
	if cost, ok := costs[model]; ok {
		return cost
	}
	return 1
}

// idleRank is a candidate's lru score: the longest idle time among the
// models it evicts. Candidates evicting nothing (or only models without idle
// data) rank zero.
func idleRank(evicted []string, idle map[string]time.Duration) time.Duration {
	var rank time.Duration
	for _, model := range evicted {
		if d := idle[model]; d > rank {
			rank = d
		}
	}
	return rank
}

func contains(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

type bitMask []uint64

func (m bitMask) has(bit int) bool {
	return m[bit/64]&(uint64(1)<<uint(bit%64)) != 0
}

func (m bitMask) subsetOf(other bitMask) bool {
	for i := range m {
		if m[i]&^other[i] != 0 {
			return false
		}
	}
	return true
}

func unionMask(left, right bitMask) bitMask {
	result := make(bitMask, len(left))
	for i := range left {
		result[i] = left[i] | right[i]
	}
	return result
}

type witness struct {
	model string
	left  *witness
	right *witness
}

type projectedState struct {
	mask    bitMask
	witness *witness
}

type evaluator struct {
	modelBits map[string]int
	words     int
	memo      map[*node][]projectedState
}

// evaluator projects every DSL combination onto the models relevant to one
// query. Choices that differ only by non-running models collapse to one state.
func newEvaluator(relevant []string) *evaluator {
	modelBits := make(map[string]int, len(relevant))
	for _, model := range relevant {
		if _, exists := modelBits[model]; !exists {
			modelBits[model] = len(modelBits)
		}
	}
	return &evaluator{
		modelBits: modelBits,
		words:     (len(modelBits) + 63) / 64,
		memo:      make(map[*node][]projectedState),
	}
}

func (e *evaluator) fullMask() bitMask {
	mask := make(bitMask, e.words)
	for bit := range len(e.modelBits) {
		mask[bit/64] |= uint64(1) << uint(bit%64)
	}
	return mask
}

func (e *evaluator) evaluate(root *node) []projectedState {
	if states, ok := e.memo[root]; ok {
		return states
	}

	var states []projectedState
	switch root.kind {
	case nodeLeaf:
		mask := make(bitMask, e.words)
		if bit, relevant := e.modelBits[root.name]; relevant {
			mask[bit/64] |= uint64(1) << uint(bit%64)
		}
		states = []projectedState{{
			mask:    mask,
			witness: &witness{model: root.name},
		}}
	case nodeRef:
		states = e.evaluate(root.ref)
	case nodeOr:
		for _, child := range root.children {
			for _, state := range e.evaluate(child) {
				states = addToFrontier(states, state)
			}
		}
	case nodeAnd:
		// Combining projected masks computes the same union as the full
		// Cartesian product, while addToFrontier removes choices that cannot
		// affect this query before the next child is processed.
		states = []projectedState{{mask: make(bitMask, e.words)}}
		for _, child := range root.children {
			var combined []projectedState
			for _, left := range states {
				for _, right := range e.evaluate(child) {
					state := projectedState{
						mask: unionMask(left.mask, right.mask),
						witness: &witness{
							left:  left.witness,
							right: right.witness,
						},
					}
					combined = addToFrontier(combined, state)
				}
			}
			states = combined
		}
	}

	e.memo[root] = states
	return states
}

func addToFrontier(states []projectedState, candidate projectedState) []projectedState {
	// Every relevant bit is either required or has a positive eviction cost.
	// A superset can therefore replace a subset without changing the optimum.
	for _, state := range states {
		if candidate.mask.subsetOf(state.mask) {
			return states
		}
	}

	kept := states[:0]
	for _, state := range states {
		if !state.mask.subsetOf(candidate.mask) {
			kept = append(kept, state)
		}
	}
	return append(kept, candidate)
}

func flattenWitness(root *witness) []string {
	seen := make(map[string]bool)
	var models []string
	var visit func(*witness)
	visit = func(current *witness) {
		if current == nil {
			return
		}
		if current.model != "" && !seen[current.model] {
			seen[current.model] = true
			models = append(models, current.model)
		}
		visit(current.left)
		visit(current.right)
	}
	visit(root)
	sort.Strings(models)
	return models
}
