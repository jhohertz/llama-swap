# llama-swap codebase analysis — where the kubernetes backend plugs in

Repo state: branch `kubernetes-operator`, commit `8fa8589`.

## 1. Request flow

```
HTTP in
  -> http.Server (llama-swap.go)          # signal handling, hot reload
  -> Server.ServeHTTP (internal/server/server.go)
       mux routes (modelPostJSONRoutes, ...)
  -> middleware chain (internal/chain): auth, profiles, selectors,
     request-context, inflight, filters, form-filters, metrics
  -> Server.localPeerHandler
       swaputil.FetchContext(r, cfg)      # extracts model id, aliases, streaming
       s.local.Handles(modelID)?          # group/matrix router (LocalRouter)
       s.peer.Handles(modelID)?           # peer router (reverse proxy only)
```

`FetchContext` returns `ReqContextData{ApiKey, Model, ModelID, Streaming,
SendLoadingState, Metadata}` — `Metadata` is a request-scoped string bag the
middleware may mutate (metrics copies it into the activity log). **This is a
ready-made slot for session-id / slot-id propagation through the pipeline.**

## 2. The router layer (local)

`internal/router/base.go` — `baseRouter` is a single-goroutine run loop over
channels (handlerCh, cancelCh, swapDoneCh, serveDoneCh, unloadCh, shutdownCh).
It owns:

- `processes map[string]process.Process` — one process object per configured
  model, created at router construction (fixed set).
- the grant/serve protocol: `ServeHTTP` sends a `HandlerReq` and parks on an
  *unbuffered* respond channel; the scheduler either grants a `HandleFunc`
  (wrapped process handler) or an error. In-flight counting is exact because of
  the unbuffered handshake.
- swap machinery: `doSwap(modelID, toStop)` stops evicted processes in parallel,
  then `target.EnsureReady(...)`, reports `SwapDone`.

Two seams, both interface-based:

- **`scheduler.Swapper`** (eviction policy): `EvictionFor(target, running) []string`.
  Implementations: `groupSwapper` (group.go), matrix solver (matrix.go). The
  scheduler (currently FIFO only, `routing.scheduler.use`) is orthogonal to the
  swapper. **A kubernetes backend needs no changes to either** — eviction is
  "stop these model IDs", and stopping a k8s process is a Deployment delete.
- **`process.Process`** (the backend):

```go
type Process interface {
    Run(timeout time.Duration) error
    WaitReady(ctx) error
    EnsureReady(ctx, timeout) error   // the one the router actually calls
    Stop(timeout) error
    State() ProcessState              // stopped|starting|ready|stopping|shutdown
    ServeHTTP(w, r)                   // reverse proxy to the backend
    Logger() *logmon.Monitor
}
```

Today there is exactly one implementation family: `process_command.go` (a
launched command tree; docker models are just `cmd: docker run ...` with
`cmdStop: docker stop ...`). **We do not add a second Process implementation.**
The `kubeswap` wrapper (PLAN §4) stands in where `docker` does: llama-swap
substitutes `${PORT}` into the `cmd` string, launches `kubeswap serve`, and the
wrapper plays the roles the interface expects —

| method | semantics, satisfied by the wrapper |
|---|---|
| `EnsureReady` | wrapper listens on `${PORT}` immediately; `/health` = 503 until it has created/adopted the Deployment+Service and the pod is Ready (readinessProbe) — llama-swap's `healthCheckTimeout` covers the long wait |
| `Stop` | `cmdStop: kubeswap delete ...` removes the Deployment; serve observes it and exits 0. Plain SIGTERM (no cmdStop, or head-end shutdown) does a best-effort slot save and exits **keeping** the Deployment, so the next wrapper adopts it |
| `State` | process liveness = wrapper liveness (exactly how docker models work today) |
| `ServeHTTP` | llama-swap proxies to `127.0.0.1:${PORT}`; the wrapper forwards to `http://<model>-svc.<ns>.svc.cluster.local:<port>` — the Service DNS name is stable before the pod exists, so the wrapper needs no discovery |
| `Logger` | wrapper tails pod logs (informer + `GetLogs`) onto its stdout → the existing logmon `/logs/stream/{model}` pipeline works unchanged |

The stop-flow details matter and are verified in `process_command.go
(killProcess)`: llama-swap runs `cmdStop` (only `${PID}` is substituted — model
ID/namespace must be literal in the string), waits up to `gracefulTimeout`
for the process to exit, then SIGKILLs the group. A `kubeswap serve` that
exits 0 when its Deployment disappears therefore stops cleanly without ever
needing the SIGKILL path.

Note the process is *pre-created per model* at router construction; `Run` is
lazy. That fits perfectly: construction is free (no cluster calls), the first
request launches the wrapper, and adoption of a leftover Deployment happens
inside the wrapper. Orphan cleanup (models not in config) is a separate
one-shot `kubeswap gc`, since with the wrapper approach llama-swap core never
looks at the cluster at all.

## 3. Peer router — the proxy half we learn from

`internal/router/peer.go` builds per-peer `httputil.ReverseProxy` with:
configurable timeouts (connect/keepalive/responseHeader/TLS/expectContinue/
idleConn), `X-Accel-Buffering: no` on SSE, careful client-cancel vs peer-error
handling, API-key injection, model-name rewriting (`ReplaceRequestModel`), and
inflight tracking. The `kubeswap` wrapper's local proxy should mirror these
semantics — but note the flow is client → llama-swap middleware → wrapper →
Service, so llama-swap's filters/middleware (auth, model rewrite, `id_slot`
injection in phase 2, inflight) all run *before* the wrapper. The wrapper can
therefore be a **dumb HTTP passthrough** with just the SSE/buffering and
cancel-handling parts worth copying.

## 4. What already exists that the plan leans on

- **In-flight accounting** — `internal/server/inflight.go` tracks every
  model-dispatched request with a cancel context, emits periodic snapshot
  events (250ms). Phase-3 scaling signal is available.
- **JSON body rewriting** — `internal/server/filters.go` already rewrites
  request bodies in place with `sjson` (useModelName → stripParams → setParams →
  setParamsByID, including "set-if-undefined" `key?` soft sets). `id_slot`
  injection (phase 2) is the same operation, run from the slot middleware
  rather than user config — and because it sits in llama-swap core it applies
  to *every* llama.cpp backend (docker, process, kubeswap, peer).
- **TTL / unloadTimeout** — per-model and global, applied by the scheduler;
  works per-process, so it works for k8s processes unchanged.
- **Preload hooks** — `hooks.onStartup` triggers loads at boot; k8s adoption +
  preload compose.
- **Store** — `internal/store` is SQLite (goose-migrated) for metrics/activity
  captures only. *Not* routing state; two head-ends sharing one file would
  collide (HA note).
- **Events** — `internal/event` (SSE to UI) + logmon; provisioning/unloading a
  Deployment should emit the same events so the UI's Work status behaves
  identically to docker/process models.
- **config hot reload** — `watch-config` rebuilds the whole `Server` and
  shuts down the old one gracefully. With the wrapper's semantics this works
  for free: the old server's shutdown SIGTERMs the old `kubeswap serve`
  processes, which keep their Deployments; the new server's wrappers adopt
  them on next launch. No unload-on-reload flag needed — preservation is the
  natural SIGTERM behavior (docker cmd processes, by contrast, die with the
  swap, which is why this decision was non-obvious before the wrapper).

## 5. Schedulers and k8s — answer to the prompt's question

"see if existing schedulers (matrix, group) work for the kube" → **yes, with
zero changes**, because:

- both routers drive models exclusively through `process.Process` +
  `Swapper.EvictionFor`;
- eviction of a k8s model = `Stop` = Deployment delete (may take a few seconds;
  `unloadTimeout` already covers slow stops — set it higher for k8s models,
  same as docker);
- the matrix DSL expresses co-residency constraints (in/out, groups) in terms of
  model IDs only. A k8s model is just a model ID.

One nuance: provisioning latency (image pull, model load) is much longer than
a docker start. The FIFO scheduler already queues requests during a swap and
has the "loading state" streaming UX (`SendLoadingState`, queue position) —
that UX is exactly what covers slow k8s provisioning. `healthCheckTimeout`
(default 30s) bounds readiness waits and must be generous for k8s (configurable
per model — it exists).

## 6. Upstream-friendliness assessment

- New code: `internal/process/` (or `internal/kube/`) + config fields + a small
  slot middleware (phase 2) + manifests/helm under `kubernetes/`. Nothing in
  the existing routers/scheduler/server is restructured.
- New dependency: `k8s.io/client-go` (biggest upstream objection candidate).
  It can be kept behind a build tag or a small interface so the default build
  stays dependency-free (see PLAN §7.1).
- The `Process` interface exists *for this* — a second implementation is a
  natural extension, not a fork in the road.
