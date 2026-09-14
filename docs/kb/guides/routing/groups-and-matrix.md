---
title: Running several models at once with groups and matrix
summary: Choosing between the group and matrix routers, and how each decides what gets unloaded.
category: guides
tags: [routing, groups, matrix, concurrency, swap, vram]
config_keys: [routing, routing.router.use, routing.router.settings.groups, routing.router.settings.matrix, routing.router.settings.matrix.eviction_tiebreaker, routing.router.settings.matrix.reclaim]
updated: 2026-09-14
---

# Running several models at once: groups and matrix

Out of the box llama-swap runs one model at a time. The `routing` section
changes that. There are two engines and you pick one:

```yaml
routing:
  router:
    use: group     # or: matrix
```

## `group` — the default, simpler

You define named groups of models and set two flags per group.

```yaml
routing:
  router:
    use: group
    settings:
      groups:
        # default behaviour: one model at a time, instance-wide
        main:
          swap: true         # only one member runs at a time
          exclusive: true    # running a member unloads every other group
          members: [llama, qwen]

        # these three run together, but any other group evicts them
        small:
          swap: false        # all members can be loaded at once
          exclusive: false   # loading one doesn't unload other groups
          members: [embeddings, reranker, whisper]

        # never unloaded by anything else
        forever:
          persistent: true
          swap: false
          exclusive: false
          members: [always-on-embeddings]
```

The three flags:

- **`swap`** (default `true`) — how members behave among *themselves*. `true`
  means one at a time; `false` means they all coexist.
- **`exclusive`** (default `true`) — how the group affects *other* groups.
  `true` means loading a member unloads everything else.
- **`persistent`** (default `false`) — other groups can never unload this one.

**A model can belong to only one group**, and every member must be a real model
ID.

The classic setup is one exclusive group for the big LLMs and one non-exclusive
group for small always-useful models like embeddings and rerankers.

## `matrix` — more work, far more flexible

The matrix router takes a list of model combinations that are allowed to run
concurrently and solves for the cheapest way to satisfy each request.

```yaml
routing:
  router:
    use: matrix
    settings:
      matrix:
        vars:
          g: gemma-model
          q: qwen-model
          v: voxtral-model

        evict_costs:
          v: 50              # vLLM, slow cold start — avoid evicting
          llama-70B: 30

        sets:
          standard:    "(g | q) & v"
          creative:    "(g | q) & stable-diffusion"
          full:        "llama-70B"
```

`sets` values are expressions:

| operator | meaning |
| --- | --- |
| `&` | AND — these run together |
| `\|` | OR — alternatives |
| `()` | grouping |
| `+name` | inline another set's expression |

`"(g \| q) & v"` expands to `[gemma, voxtral]` and `[qwen, voxtral]`.
Parenthesize mixed expressions so their intended capacity rule is obvious, and
test every requested combination: an expression that excludes a needed set can
make the router evict a model unexpectedly.

How the solver works when a request for model X arrives:

1. If X is already running, forward the request.
2. Otherwise collect every set containing X.
3. For each set, sum the `evict_costs` of running models *not* in that set.
4. Pick the lowest-cost set; equal-cost sets are ordered by
   `eviction_tiebreaker` (definition order by default).
5. Evict the models outside it, start X, forward the request.

Two things worth internalising:

- **Subsets are permitted.** A set `[a, b, c]` also allows `[a, b]`, `[a]` and
  so on. Only the requested model is started; the rest are not preloaded.
- **A model in no set can only run alone.**

`evict_costs` (default 1) is how you express "this one is painful to reload".
Give slow cold-starting backends a high cost.

When you deploy the head end with the kubeswap Helm chart, the chart's
`config.matrix` values can *generate* this whole section from the model
roster instead of you writing the DSL (see the kubeswap-kubernetes
article's matrix-builder section); hand-written routing and the builder are
mutually exclusive.

### Choosing which model to evict (`eviction_tiebreaker`)

`evict_costs` orders candidates by how much eviction would hurt. When two
candidates score **exactly the same cost**, the solver still has to pick one.
`settings.matrix.eviction_tiebreaker` chooses how:

```yaml
settings:
  matrix:
    eviction_tiebreaker: lru   # default: lexical
```

- **`lexical`** (default): the first equal-cost candidate in set-definition
  order wins. Deterministic and the historical behaviour; if you never set
  the key, nothing changes.
- **`lru`**: the candidate that evicts the **longest-idle** model wins
  (idle = time since the model became ready or last finished a request). A
  cost tie between evicting a model idle for an hour and one idle for a
  minute evicts the hour-old one.

Cost is always the primary key: lru never evicts a high-cost model in favour
of a cheaper idle one — it only orders candidates that cost the same. When
idle times are also equal, the decision falls back to definition order, so
outcomes stay deterministic either way.

The group router has no equal-cost choices to make (it evicts its whole
swap set), so this setting only applies to matrix.

### Steering evictions while a queue is pending (`reclaim`)

The tie-breaker orders candidates once the eviction count is fixed;
`reclaim` adds a preference that only applies while requests are queued:
**turn over idle models, protect models the queue still needs.**

The eviction *count* is always the minimum the budget forces — the solver's
set is always budget-sized, so the fleet stays full and every eviction is
met by exactly one queued connection taking the freed slot. What the queue
changes is *which* idle model turns over:

```yaml
settings:
  matrix:
    reclaim: queue   # default: minimal
```

- **`minimal`** (default): minimise total eviction cost — the historical
  behaviour.
- **`queue`**: while the pending request queue is **non-empty**, eviction
  cost is charged only for models the queue references. Idle models the
  queue does not reference turn over first — even at a high
  `evict_cost` — while a model the queue still references is protected.
  With an empty queue the objective falls back to minimal, so interactive
  traffic is unaffected.

Because each queued target evicts a single idle model, different targets
evict *different* models, and the scheduler runs their swaps in parallel
(its collision check only blocks intersecting evict sets). A burst of
requests therefore drains one-for-one — no serial swap per request, and no
emptied fleet while the backlog works through.

What `queue` does **not** do:

- evict a model the queue references (its `evict_cost` is the protection;
  only budget pressure the queue cannot avoid forces it out, at the least
  painful cost);
- evict more than the request needs — the fleet stays full; an eviction is
  only made when a queued connection takes the slot;
- evict a model mid-request (the scheduler waits for in-flight requests
  to drain, as always);
- reach into the group router (same as `eviction_tiebreaker`).

The trade: an unqueued model with a high `evict_cost` **is** turned over
while the queue references nothing for it, and a fresh request for it pays
the reload. That is the point of the knob — during a backlog, the queue is
the best available prediction of what comes next.

## Which one?

Use **group** if your setup is describable as "these run together, those swap
out". It is easier to read and easier to get right.

Use **matrix** when the combinations depend on which models are involved — a
70B that needs every GPU alone, versus several small models that fit together,
versus a mid-size LLM plus TTS. Groups cannot express that; matrix can.

## Request ordering

Queued requests are FIFO. You can give some models priority:

```yaml
routing:
  scheduler:
    use: fifo
    settings:
      fifo:
        priority:
          interactive-model: 10
          batch-model: 1
```

Higher numbers are serviced first. Models default to 0.

## Related

- `reference/config/routing` — the full annotated section
- `guides/model-runtime/ttl-and-unloading` — reclaiming VRAM from idle models
- `guides/operations/kubeswap-kubernetes` — the kubeswap Helm chart, whose
  `config.matrix` builds this section from the model roster
