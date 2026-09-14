package router

import (
	"time"

	matrixdsl "github.com/mostlygeek/llama-swap/internal/matrix"
)

// matrixSolver contains pure swap-decision logic with no Process dependencies.
// It is safe for concurrent reads after construction.
type matrixSolver struct {
	program    *matrixdsl.Program
	evictCosts map[string]int
	// tieBreaker orders equal-cost candidates; see config.EvictionTieBreaker*.
	tieBreaker string
	// reclaim selects the eviction objective; see config.Reclaim*.
	reclaim string
}

func newMatrixSolver(program *matrixdsl.Program, evictCosts map[string]int, tieBreaker, reclaim string) *matrixSolver {
	return &matrixSolver{
		program:    program,
		evictCosts: evictCosts,
		tieBreaker: tieBreaker,
		reclaim:    reclaim,
	}
}

type solveResult = matrixdsl.Decision

// Solve decides the evictions for requestedModel among runningModels. idle
// maps a running model to how long it has gone without finishing a request;
// it is consulted only when the solver was built with the lru tie-breaker.
// upcoming is the pending request queue (excluding requestedModel); it is
// consulted only when the solver was built with the queue reclaim objective.
func (s *matrixSolver) Solve(requestedModel string, runningModels []string, idle map[string]time.Duration, upcoming []string) solveResult {
	return s.program.Solve(requestedModel, runningModels, matrixdsl.SolveOptions{
		EvictCosts: s.evictCosts,
		TieBreaker: s.tieBreaker,
		Idle:       idle,
		Reclaim:    s.reclaim,
		Upcoming:   upcoming,
	})
}
