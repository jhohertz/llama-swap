# llama-swap on Kubernetes — Phased Plan

Status: **draft for discussion** — no implementation yet.

Companion notes (read these first if you want the reasoning):

- [notes/codebase-analysis.md](notes/codebase-analysis.md) — how llama-swap works internally and why the wrapper approach fits
- [notes/slot-analysis.md](notes/slot-analysis.md) — sticky sessions vs KV-aware routing, the llama-server slot API, the gist, Paddler
- [notes/k8s-mechanics.md](notes/k8s-mechanics.md) — client library, RBAC, templates, test cluster

---

## 1. The ask, in one paragraph

Make llama-swap able to manage `llama.cpp` (and other inference) backends in a
dedicated Kubernetes namespace: on demand, launch a backend for a model; on
unload, tear it down; proxy API traffic to it. The key architectural decision
(resolved in discussion): **no new backend type and no new `Process`
implementation in llama-swap core.** Instead we ship a small wrapper binary
(`kubeswap`) that stands in exactly where `docker` stands in a model's `cmd`
today. llama-swap keeps treating the model as an ordinary launched command; the
wrapper owns the Deployment/Service lifecycle and a local proxy port. llama-swap
core changes are limited to phase-2 slot/session management, which then benefits
*every* llama.cpp backend (docker, process, kubeswap, peer) — a clean upstream
story. No CRDs, one instance per model initially, no load balancing in the
first pass.

## 2. Key findings from the analysis

1. **Docker models are just `cmd` + `cmdStop`.** `process_command.go` is the
   only `Process` implementation: llama-swap allocates a free local port,
   substitutes `${PORT}` into the `cmd` string, launches it, health-checks
   `127.0.0.1:${PORT}`, proxies there, and stops via `cmdStop` (with `${PID}`)
   or process-tree kill. A wrapper binary that accepts `--listen 127.0.0.1:${PORT}`
   is a perfect drop-in for the same machinery. **Phase 1 therefore needs no
   llama-swap core code at all** — schedulers (group/matrix), FIFO, TTL,
   `unloadTimeout`, preload hooks, eviction all work unchanged.

2. **llama-server already does KV-aware slot selection internally**
   (`--slot-prompt-similarity`/`-sps`, default 0.1, longest-common-prefix
   affinity), and the user's cluster already runs `--cache-idle-slots`. What it
   does *not* do: persist KV across process death, or expose which slot served
   an OAI request. So sticky slots (explicit session→slot binding via the
   `id_slot` body field) + disk save/restore (`POST /slots/{id}?action=save|restore`
   + `--slot-save-path`) is what we build in llama-swap core in phase 2 — for
   all llama.cpp backends. Full reasoning in [notes/slot-analysis.md](notes/slot-analysis.md).

3. **Paddler is a different category, not a competitor design.** It embeds
   llama.cpp (Rust bindings, its own continuous-batching scheduler); its
   "slots" are internal sequence slots and its balancer does *capacity*
   routing, not session stickiness. It can skip sticky sessions because it owns
   the KV; we proxy a black-box llama-server so we cannot. What we borrow:
   keep routing dumb (capacity-based), don't build a routing brain.

4. **`k8s.io/client-go` is an approved technology** (in AGENTS.md). It lives in
   the `kubeswap` wrapper binary (its own `main` package in the repo, built
   alongside llama-swap) — the llama-swap server binary stays free of the
   dependency. In-cluster the wrapper uses the ServiceAccount token; on a host
   it uses kubeconfig. Same binary, two auth modes.

5. **Test cluster (maximus) is a GPU cluster**: 6 workers, each with 1× AMD
   Radeon 8060S (RDNA4, 96 GB VRAM), allocatable as `amd.com/gpu: 1`.
   `ghcr.io/ggml-org/llama.cpp:server-vulkan` (verified: 310 MB, bundles the
   RADV AMD ICD *and* lavapipe CPU fallback) is the default backend image —
   one image for GPU and CPU. CPU is the sanctioned fallback everywhere
   (maximus trouble → drop GPU request; CI is CPU-only anyway).

## 3. Target architecture (phase 1)

```
                     +------------------------------------------------+
   clients          |  llama-swap head-end (Deployment, 1 replica)   |
  (OpenAI/Anthropic |  image: mostlygeek/llama-swap:unified-vulkan   |
   compatible)      |  contains BOTH binaries:                       |
        |           |   - llama-swap   (server: router/sched/proxy)  |
        v           |   - kubeswap     (wrapper; client-go)          |
  POST /v1/...      +-------+----------------------------------------+
                    |        | per-model, on demand (cmd launch, like docker)
                    |        v
                    |  kubeswap serve --listen 127.0.0.1:${PORT}     |  (process child,
                    |   --model X --ns llama-swap --image I -- ...   |   dies with pod)
                    |     - creates/adopts Deployment+Service        |
                    |     - proxies ${PORT} -> <model>-svc.ns.svc    |
                    +-------+----------------------------------------+
                            | in-cluster API (SA token) create/delete/watch
                            v
                 kube-apiserver, namespace: llama-swap
                 label: llama-swap.io/managed-by=llama-swap
                            |
                     +-----+------+
                     | Deployment   |  pod: llama-server (server-vulkan)
                     | Service      |  PVC/emptyDir: models, slots
                     +--------------+
```

- One Deployment + one ClusterIP Service per model; every object carries
  `llama-swap.io/managed-by: llama-swap` + `llama-swap.io/model: <sanitized>`
  (the leftover/orphan handle).
- The wrapper is a **child process of the head-end** (like `docker run` is
  today): if the head-end pod dies, the wrapper dies, but the Deployment
  survives in the cluster — and on restart the wrapper **adopts** it (phase 1
  acceptance #4).
- Out-of-cluster mode (head-end on a homelab host): identical, except the
  wrapper authenticates with kubeconfig and the model's `cmd` is the same
  string. One wrapper, both worlds.

### Sample model config (proposal — this is all that changes in llama-swap config)

No new `backend:` keyword. A kubernetes model looks like this:

```yaml
models:
  lfm25-230m:
    cmd: >-
      kubeswap serve
      --listen 127.0.0.1:${PORT}
      --model lfm25-230m
      --namespace llama-swap
      --image ghcr.io/ggml-org/llama.cpp:server-vulkan
      --gpu amd.com/gpu=1
      --node-selector feature.node.kubernetes.io/amd-gpu=true
      --volume llama-swap-models:/models:ro
      --volume slots:/slots
      --
      --model /models/LFM2.5-230M-Q4_0.gguf
      --port 8080
      -ngl 99
      -np 2
      --cache-ram 8
      --slot-save-path /slots
    healthCheckTimeout: 120s   # image pull + model load is slow
    ttl: 30m
    cmdStop: kubeswap delete --model lfm25-230m --namespace llama-swap
```

- Everything after `--` is passed through as the container's args.
- All-flags first (user preference); a `--config file.yaml` (or env) is a
  convenience for complex cases, flags win.
- `cmdStop` does the **explicit teardown**: `kubeswap delete --model <id>
  --namespace <ns>`. The running `kubeswap serve` watches its own Deployment
  and exits 0 when it sees the deletion (llama-swap's stop flow runs CmdStop,
  waits for exit, SIGKILLs only if it hangs — verified in
  `process_command.go`).
- **SIGTERM is *not* teardown** (user decision #6, "adopt back on restart"): on
  SIGTERM the wrapper does a best-effort phase-2 slot save and exits, leaving
  the Deployment running for the next wrapper to adopt. That keeps backends
  alive across head-end restarts/reloads. If `cmdStop` is omitted entirely,
  Stop degrades to SIGTERM: the Deployment lingers and the *next* launch
  adopts it (self-healing; `kubeswap gc` reclaims true orphans).
- CPU fallback: omit `--gpu`/`--node-selector` — lavapipe takes over.

## 4. The `kubeswap` wrapper (phase 1 deliverable)

A small Go binary (own `main` under `cmd/kubeswap/`, same module, client-go
dependency). Subcommands:

| command | purpose |
|---|---|
| `kubeswap serve [flags] -- <container args...>` | long-running: lifecycle + local proxy (the `cmd`) |
| `kubeswap delete --model <id> --namespace <ns> [--delete-volumes]` | one-shot: delete the model's managed objects (used as `cmdStop`; serve exits when it observes the deletion) |
| `kubeswap gc --namespace <ns> --config-models <list\|file>` | one-shot: delete managed objects whose model isn't in the given list (orphan cleanup); logs what it did |
| `kubeswap status --namespace <ns>` | one-shot: table of managed deployments (debug) |

`serve` behavior:

1. **Auth**: in-cluster SA token if available, else kubeconfig
   (`KUBECONFIG`/`~/.kube/config`). Namespace-scoped client only.
2. **Adopt-or-create**: look for a Deployment labeled for `--model`.
   Exists → adopt (watch it; log adoption). Missing → create PVCs (if
   requested) → Deployment → Service. Idempotent 409 handling throughout.
3. **Local proxy**: listen on `--listen` (the `${PORT}` llama-swap gave it);
   forward to `http://<model>-svc.<ns>.svc.cluster.local:8080`. Until the pod
   is Ready, answer `/health` with 503 (llama-swap's health-check keeps
   polling until `healthCheckTimeout`) and other requests with a 503 error
   body. Also watches its Deployment: if it's deleted externally (`kubeswap
   delete` / `gc`) → exit 0 (that's how `cmdStop` stops the process). Reuse
   the peer router's proxy semantics (SSE `X-Accel-Buffering: no`,
   client-cancel handling) — factor that code or re-implement minimally.
4. **Readiness**: informer on pods for the Deployment; pod Ready ⇒ proxy live.
   CrashLoopBackOff ⇒ surface pod status message in logs (and on `/health`
   body) so llama-swap's load error is readable.
5. **SIGTERM**: best-effort phase-2 slot saves (see phase 2) → exit 0.
   **Does not delete the Deployment** — adoption-on-restart depends on the
   backend surviving the head-end. Teardown is explicit (`kubeswap delete`
   via `cmdStop`, volumes kept unless `--delete-volumes`). SIGKILL: nothing
   (hence periodic saves in phase 2 + adoption on next start).
6. **Deployment spec**: rendered from flags (image, args, env, resources,
   nodeSelector, tolerations, volumes, probes). Defaults: llama-server
   readiness/liveness on `:8080/health`, `strategy: Recreate`,
   `terminationGracePeriodSeconds` from a `--grace` flag (default 30s).
   Pure function `flags -> objects`, unit-testable without a cluster.

Naming (resolved): service/labels derive from the **configured model name**,
offending chars translated to dashes (`author/model` → `author-model`); the
original name is kept in a `llama-swap.io/model-id` annotation so lookup is
unambiguous.

RBAC: same namespaced Role as in [notes/k8s-mechanics.md](notes/k8s-mechanics.md)
(deployments, services, PVCs, pods, pods/log, events). In-cluster, the wrapper
runs inside the head-end pod under the head-end's ServiceAccount.

## 4b. Phase 1 implementation status

Implemented and verified on the maximus cluster (k3s v1.35.5, AMD
Radeon 8060S via `amd.com/gpu`, `llama-swap` namespace):

- `cmd/kubeswap/` — `serve` / `delete` / `gc` / `status`; unit-tested
  (naming, parsing, object rendering, fake-clientset lifecycle, proxy
  handler). `make kubeswap` builds the host binary.
- Built into the unified image: `docker/unified/install-kubeswap.sh` + a
  build stage in `runtime.Dockerfile`, installed to `/usr/local/bin/kubeswap`.
- Docs: `cmd/kubeswap/README.md`; manifests under `kubernetes/manifests/`
  (`rbac.yaml` verified per-verb with `kubectl auth can-i --as` for every call
  the wrapper makes; `head-end.yaml` now carries a `kubeswap gc` initContainer;
  `model-cache-pvc.yaml` switched to `longhorn` RWX with the verified
  downloader pod).

Spike results (all pass):

1. `kubeswap serve` from the host created the Deployment+Service (model ID
   `LiquidAI/LFM2.5-230M` sanitized to `liquidai-lfm2-5-230m`), the pod pulled
   `ggml-org/llama.cpp:server-vulkan`, got the 1× `amd.com/gpu`, loaded
   LFM2.5-230M-Q4_0 from the RWX longhorn PVC, readiness 503→200, and a
   proxied `/v1/chat/completions` returned a completion. Pod logs were
   forwarded with a `[pod/<name>]` prefix.
2. `kubeswap delete` (cmdStop) removed the Deployment/Service, waited for the
   pod to terminate (GPU released), and the running `serve` observed the
   deletion and exited 0 on its own.
3. Restarting `serve` against the same model **adopted** the existing
   Deployment (no duplicate objects, no second GPU allocation).
4. `kubeswap gc --config` kept the configured model and collected a leftover
   one; the leftover's `serve` process self-exited. `kubeswap status` prints
   the managed workloads.
5. RBAC: the Role from `kubernetes/manifests/rbac.yaml` covers every API call
   the wrapper makes (verified per-verb against the real cluster).

Still open for phase 1 hardening:

- End-to-end run with llama-swap itself driving `cmd`/`cmdStop` in-cluster
  (the in-cluster head-end + initContainer gc from `head-end.yaml`) — the
  host-side flow above covers the same wrapper paths.
- `-np` slot/session management (phase 2), sticky routing (phase 3).

## 5. Phases

### Phase 0 — Foundations (small, de-risks everything)

- `cmd/kubeswap/` skeleton: flags, client-go wiring (in-cluster + kubeconfig),
  flag→objects renderer, unit tests (no cluster needed for the renderer).
- RBAC + sample manifests under `kubernetes/manifests/` (starter set already
  drafted here).
- Spike on maximus/`llama-swap` ns: hand-run `kubeswap serve` with the
  LFM2.5-230M-Q4_0 model, GPU (`amd.com/gpu: 1`) and CPU fallback; verify
  image pull, Service DNS, `/health` 503→200 transition, SIGTERM teardown.
- `kubeswap gc` + orphan scenario: leave a leftover Deployment, re-run,
  confirm adoption and orphan cleanup.

### Phase 1 — "docker, but in kube" (no llama-swap core changes)

- Finish the wrapper (serve/gc/status) per §4.
- Build `kubeswap` into the unified images (it must be present in the head-end
  image for the in-cluster flow; also published as a standalone binary for
  host-side head-ends).
- Docs: model-config examples (GPU + CPU), RBAC setup, in-cluster vs host
  head-end.
- **Acceptance (maximus/llama-swap ns, tiny models):**
  1. `POST /v1/chat/completions` for an unloaded model provisions the backend
     (wrapper as `cmd`) and streams a completion.
  2. Swap group: model B's request evicts model A — `cmdStop` deletes its
     Deployment, model B serves. `ttl` expiry and `POST /api/models/unload` too.
  3. Kill the head-end pod with a backend running: on restart the wrapper
     adopts the live Deployment (no duplicate, no double-GPU allocation); a
     request for an unconfigured leftover model is cleaned by `kubeswap gc`.
  4. `group` and `matrix` routers both exercise the above.
  5. CPU path: same config minus `--gpu`/node-selector runs via lavapipe.

### Phase 2 — Slot/session management in llama-swap core (all backends)

Goal: a session's KV cache survives across requests and across model
unload/reload/crash, for **any** llama.cpp backend (docker, process, kubeswap,
peer) — not just kubernetes. This is the only llama-swap core code in the
whole plan, and it's a feature llama.cpp users generally want.

- **Scope guard:** applies when the backend speaks the llama-server slot API
  (opt-in per model, e.g. `slots: true` or auto-detected via `GET /slots`);
  non-llama backends are untouched.
- **Session identity — the open investigation (user: "grey area, investigate
  more later"):**
  - Explicit optional identifier (header or body field) is fine as an
    override, but **client headers must not be required**.
  - Primary direction: **emulate llama's own KV-prefix matching at the
    proxy** — like llm-d's router running the model's tokenizer over incoming
    prompts and hashing token blocks (`h_i = hash(t_i | h_{i-1})`), so a new
    request is bound to the session (and thus the slot) with the longest
    shared prefix chain. This is exactly the affinity llama-server computes
    internally (`-sps` LCP), replicated at the proxy so we can *name* and
    *persist* the session. Needs the model's tokenizer on the proxy side
    (GGUF ships it; availability for each backend type to be checked).
  - Fallbacks while that's TBD: hash of a stable prompt prefix (first
    message(s), raw text — no tokenizer); config-fixed session per model
    (`-np 1` homelab default = one session anyway).
  - Design the registry so the *derivation* is pluggable:
    `sessionKey := derive(sessionID?)` with (explicit) > (prefix-hash) >
    (config fallback) precedence.
- **Slot registry** (per model, in the head-end): `sessionKey -> slot`,
  `slot -> {lastActive, savedAt}`. Bounded by the model's `-np`. Single
  instance ⇒ no instance axis yet (phase 3 adds it).
- **Injection:** proxy sets `id_slot` in the upstream JSON body (llama-server
  accepts it on every completion endpoint incl. OAI-compat — verified).
  Reuse the existing sjson filter machinery.
- **Lifecycle:**
  - session's first request: slot has no live KV and a saved file exists →
    `POST /slots/{id}?action=restore` (best-effort, bounded).
  - idle timeout / unload (llama-swap calls this **before** stopping the
    process — it knows the boundary) → `POST /slots/{id}?action=save`
    (async, best-effort).
  - periodic save (default ~60s per dirty slot) as SIGKILL insurance — the
    gist proved the need (`cmdStop`/SIGTERM never runs on SIGKILL). The
    wrapper's SIGTERM handler also does a best-effort save (idempotent).
  - model load: after `/health` OK, restore every slot with a saved file.
- **State dir:** emptyDir **for now** (user decision) — for kubeswap models
  that's `--volume slots:/slots` with an emptyDir; for docker/process models
  a local dir on the host. **PVC (and the cross-node RWX-shared-KV question)
  is explicitly deferred as optimization** — note: with emptyDir, slot state
  dies on pod reschedule, which is acceptable at this phase; the save/restore
  API and file layout are PVC-ready so the swap is a volume-config change.
- **Policy (resolved):** `-np 1` with a second *distinct* session → **deny**
  with a clear error (no evict-oldest). Same session continuing → always
  allowed.
- **File layout:** `<session-safe-name>.<slot>.slot.bin` + `.meta` completion
  marker (gist convention — never restore a torn write); temp-file+rename on
  save; skip save if token count unchanged.
- Small API surface: `GET /api/sessions`, `POST /api/sessions/{id}/save`,
  `POST /api/sessions/{id}/release`.
- **Acceptance:**
  1. Two sessions, `-np 2`: both keep KV; neither evicts the other.
  2. Unload → reload → same session: restore path, `n_restored` logged, no
     full re-prefill (measure before/after).
  3. SIGKILL the backend: within one periodic-save interval of KV survives
     restart (kubeswap: via PVC only if configured; else accepted loss —
     this is why phase 3 wants PVCs).
  4. Works identically for a docker `cmd` llama-server model (backend-
     agnostic proof).

### Phase 3 — Multiple instances of a model (load balancing)

Goal: a model may run N instances; sessions stay sticky to the instance holding
their slot; scaling follows demand.

- One instance = one `kubeswap serve` process (llama-swap already models one
  process per model ID; N instances = N model entries/aliases sharing the
  model file — aliases exist today, the mechanics to be worked out).
- Slot registry gains the instance axis: `sessionKey -> (instance, slot)`.
  Known session → its owner instance; unknown session → **least-loaded
  instance with a free slot** (Paddler-style capacity routing — no routing
  brain).
- Scale-up signal: llama-swap's in-flight tracker (`internal/server/inflight.go`)
  vs total slots (replicas × `-np`).
- Scale-down / instance loss: session's slot is gone → assign a free slot on
  any instance + `restore` from **PVC** — this is where per-instance PVCs
  (StatefulSet + `volumeClaimTemplates` when N>1) become required; the
  wrapper's volume flags must render either way.
- Acceptance: 1→2 under load; stickiness holds; kill one instance → its
  sessions reattach via restore; no request loss.

### Phase 4 — Packaging

- **Helm chart** under `kubernetes/chart/`: head-end Deployment+Service
  (single replica by design), ServiceAccount+Role+RoleBinding, ConfigMap (or
  Secret) with the model config, optional model-cache PVC, optional Ingress.
- **Images:** head-end uses `mostlygeek/llama-swap:unified-vulkan` (contains
  both `llama-swap` and `kubeswap` after phase 1). Backend pods default to
  `ggml-org/llama.cpp:server-vulkan`. No new Dockerfile in phase 1.
- **CI:** `kind`, CPU-only (lavapipe), models ≤ 300 MB; phase-1 acceptance as
  Go tests behind an env flag; renderer unit tests need no cluster.

### Phase 5 — High availability (design now, build later)

- Multiple head-ends: leader election (`coordination.k8s.io` Lease); leader
  launches wrappers, followers serve read APIs + proxy (or pure L7-forward to
  the leader — simplest honest option).
- Wrapper adoption already gives most of the HA story for *backends*: a
  restarted head-end re-launches wrappers that find and adopt live
  Deployments. The durable state genuinely needing a home is the **slot
  registry** (phase 2) — small JSON; candidate CRD or shared-PVC blob.

## 6. Test plan (cluster: `maximus`, namespace `llama-swap`)

Cluster reality (verified): 9 nodes — 3 control-plane + 6 workers; **every
worker has 1× AMD Radeon 8060S** (RDNA4, 96 GB VRAM, 40 CUs) with
**`amd.com/gpu: 1`** allocatable (AMD device plugin; node label
`feature.node.kubernetes.io/amd-gpu: "true"`). The Vulkan image is the right
default for it. **If anything GPU-related misbehaves, test on CPU** (drop
`--gpu`/node-selector; lavapipe kicks in) — CI is CPU-only anyway, so the CPU
path stays exercised constantly.

- **Keep models tiny** (user decision): small quants load in seconds and fit
  anywhere:
  1. **`LiquidAI/LFM2.5-230M-GGUF` — `LFM2.5-230M-Q4_0.gguf` (149 MB)** —
     primary (our own model family).
  2. **`unsloth/gemma-3-270m-it-GGUF` — `gemma-3-270m-it-Q4_K_M.gguf`
     (253 MB)** (ggml-org's repo only ships Q8_0) — second model, for
     swap/eviction tests.
  3. **Qwen3-0.6B** (unsloth/GGUF q4 class) — optional third, for
     multi-session slot tests.
  - Rule of thumb: ≤ ~1 GB GGUF, q4 or smaller quant. unsloth QAT quants are
    fine where available.
- 6 GPUs ⇒ up to 6 concurrent backend pods for parallel scenarios (one pod per
  GPU via the device plugin).
- Model store wiring comes later (user will wire it in); for now the model
  cache PVC + one-off downloader job (`kubernetes/manifests/`).
- `kubernetes/manifests/` holds: rbac.yaml, head-end.yaml (dev), model-cache
  PVC + downloader, sample model configs.
- Every phase's acceptance list runs here first, then in CI on `kind`.

## 7. Decisions taken (from discussion)

| # | question | decision |
|---|---|---|
| 1 | how k8s integrates with llama-swap | **wrapper binary in `cmd`** (`kubeswap`), standing in for `docker`; no new `Process` impl, no `backend:` keyword; client-go confined to the wrapper |
| 2 | wrapper config | all-flags first (`--` passthrough for container args); optional config file later if needed |
| 3 | client session identity | no required client headers; explicit ID allowed as optional override; **primary = derived at the proxy by emulating llama's KV-prefix (LCP/tokenizer-hash) matching — open investigation** |
| 4 | `-np 1` + second session | **deny** with a clear error (no eviction) |
| 5 | slot state storage | **emptyDir for now**; PVC / cross-node RWX (shared KV) deferred as optimization |
| 6 | startup leftovers | **adopt** (backend keeps running across head-end restart) |
| 7 | object naming | derive from configured model name, offending chars → dashes; original in annotation |
| 8 | head-end config in-cluster | ConfigMap (or Secret)-mounted `config.yaml`; `--watch-config` is enough for now |

## 8. Remaining open questions

1. **Session↔slot binding via KV-prefix emulation** (the big one):
   - tokenizer availability at the proxy for each backend type (GGUF file
     path known from config for docker/kubeswap models; what about peers?);
   - hashing scheme: raw-text prefix hash (cheap, no tokenizer) vs
     token-block hash chain (llm-d style, robust across tokenization);
   - matching threshold semantics (mirror `-sps` 0.1?);
   - cost: tokenizer + hashing on the hot path per request.
2. **Slot opt-in detection:** explicit per-model flag vs auto-detect `GET
   /slots` (auto risks false positives on non-llama backends that happen to
   expose `/slots`).
3. **`kubeswap gc` trigger:** head-end initContainer (once per start),
   llama-swap `hooks` entry, or just documented manual/cron? (initContainer
   runs before the wrapper exists — fine for GC.)
4. **Phase-3 instance mechanics:** aliases vs a new `instances: N` concept in
   model config.
5. **Wrapper proxy fidelity:** how much of the peer router's behavior (API-key
   injection, model-name rewriting, inflight) does the wrapper need? (Arguably
   the wrapper should be a *dumb* TCP/HTTP passthrough and let llama-swap's
   existing middleware do the clever bits — but requests flow
   llama-swap → wrapper → pod, so middleware already ran. Likely: dumb
   passthrough.)

## 9. Explicit non-goals (first pass)

- No CRDs, no controller-runtime, no continuous reconcile loop (reconciliation
  happens on request, on startup via wrapper adoption, and on `gc`).
- No multi-instance head-end / leader election (phase 5).
- No per-model load balancing / slot-aware LB (phase 3); one instance per model.
- No PVCs for slot state (phase 2 uses emptyDir; PVCs enter in phase 3).
- No model-store integration yet (user will wire it in as templates evolve).
