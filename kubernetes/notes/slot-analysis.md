# Slot & sticky-session analysis

The question that drove this note: **do we need sticky sessions, or can we do
KV-aware routing (look at the prompt/KV and route there)?** Answer up front:
they are complementary layers, not rivals — and for our target (proxy in front
of a vanilla llama-server, one instance per model, homelab scale) **explicit
sticky slots with disk persistence** is the mechanism to build. KV-aware
routing is already done *inside* llama-server by default, and it only becomes
*our* job (as a cross-instance routing policy) in phase 3, where it sits on top
of sticky slot ownership rather than replacing it.

## 1. What Paddler actually does (verified from source, /tmp/kube-analysis/paddler)

- Paddler is **not a proxy to llama-server**. `paddler_agent` links
  `llama_cpp_bindings` directly — it embeds the llama.cpp engine and runs its
  **own continuous-batching scheduler** (`continuous_batch_scheduler/`).
- Its "slots" are **internal sequence slots** of that scheduler
  (`SequenceIdPool`, `SlotGuard` — a free-list of sequence IDs handed to
  ingesting/generating requests). Nothing to do with llama-server HTTP slots.
- The balancer's routing is **pure capacity**:
  `AgentControllerPool::take_least_busy_agent_controller()` = "least
  `slots_processing` among agents with a free slot". No session affinity, no
  prompt hashing, no prompt→agent map anywhere in `paddler_balancer`.
- KV preservation is a **byproduct of owning the engine**:
  1. while a sequence is active, its KV lives in the agent's llama.cpp context
     (continuous batching = prefill once, decode many);
  2. llama.cpp's context-level prompt cache lets *new* requests on the *same
     agent* hit cached prefixes — but only if the request happens to land on
     that agent.
- Consequence: if a conversation's continuation is dispatched to a *different*
  agent by the least-busy rule, it **re-prefills from scratch**. Paddler accepts
  that trade; it keeps no per-conversation binding, and it has no slot
  save/restore across agent (re)starts.

**Why we can't copy this:** Paddler *is* the inference engine; stickiness is
internal to it. We are a stateless proxy in front of a black-box llama-server
whose KV we can only touch through its HTTP API. So "which is better" is
partly a category error: Paddler's choice was forced by ownership, not
superior. What we *do* steal from Paddler is the philosophy: **routing stays
dumb (capacity-based); don't build a routing brain.**

## 2. What llama-server gives us for free (verified in ggml-org/llama.cpp `tools/server/`)

| feature | where | effect |
|---|---|---|
| **LCP slot affinity** | `get_available_slot()` in `server-context.cpp`; flag `-sps/--slot-prompt-similarity` (**default 0.1**) | a new request reuses an *idle* slot whose cached prompt shares a longest-common-prefix ≥ 10% of the request length. **KV-aware slot selection, built in, on by default.** |
| **Explicit slot pin** | `task.id_slot = json_value(data, "id_slot", -1)` — parsed for *all* completion endpoints incl. OAI-compat (`/v1/chat/completions`) | the client (us, the proxy) can say `{"id_slot": N}` in the body and pin the request to slot N. **This is the "inject a slot into the query" hook.** |
| **Idle-slot KV preservation** | `--cache-idle-slots` + `--cache-ram N` (the user's currently-enabled flag) | when an idle slot's KV must be cleared, it's published into the prompt cache (RAM copy; with `--kv-unified` the slot is then cleared, without it the KV stays in VRAM and a RAM copy is published) instead of being discarded — prefix reuse across slots. |
| **Slot state** | `GET /slots` | per-slot id, state (idle/processing), prompt size, cache stats. The proxy can inspect slots. |
| **Slot save/restore/erase** | `POST /slots/{id}?action=save|restore|erase` with `{"filename": ...}`, server must run with `--slot-save-path DIR` | persist a slot's KV to disk / reload it. Response: `n_saved/n_restored`, `n_written/n_read`, timings. |
| **slot in responses** | `id_slot` appears only in the *non-OAI* result JSON | **OAI-compat responses do NOT reveal which slot served the request.** The proxy therefore cannot learn slot assignments passively — it must own the mapping (injection). |

Notes on behavior that shape the design:

- `try_clear_idle_slots()` (kv_unified): when a new task needs room, the server
  may **purge an idle slot's prompt** (one at a time, first-found — no LRU).
  So even with `--cache-idle-slots`, an idle session's *dedicated* slot can be
  cleared; what survives is the prompt-cache copy, usable by LCP affinity if
  the prefix still matches. Explicit save-to-disk is the only durable form.
- With `-np 1` (the homelab default) LCP affinity is meaningless — one slot,
  everything shares it, and a second conversation's prefill overwrites the
  first's KV. **Slot awareness only exists if the proxy allocates and pins.**

## 3. The gist (urmuzov/c68ce96f...) — prior art for the lifecycle half

`slot-wrapper`: a static Go wrapper run as the model's `cmd`:

```
start  -> exec llama-server, wait /health, restore slots (POST /slots/{i}?action=restore)
run    -> stay foreground so llama-swap sees a live process
SIGTERM-> save slots (POST /slots/{i}?action=save), forward SIGTERM, wait
```

Plus `--save-every 15s` periodic saves (crash protection: `cmdStop` never runs
on SIGKILL) and a `.meta` completion marker so a torn save file is never
restored. It demonstrated the full save/restore dance against stock
llama-server and measured the payoff: 700k-token context ≈ 2400s prefill vs
≈20s restore.

**What the plan takes from it:**
- the API calls, filenames, `.meta` marker convention, temp-file+rename for
  periodic saves (no torn writes), skip-if-token-count-unchanged;
- the insight that save/restore belongs to the *proxy lifecycle*, so we move it
  out of a per-model sidecar binary into llama-swap itself (no extra process,
  no extra hop, works for peer/docker/k8s backends alike — a clean upstream
  story);
- periodic save as crash insurance, gated on the slot being idle.

**What it lacks (our phase 2 adds):**
- per-*session* identity — it saves "the slots of model X" at process
  boundaries; it can't keep two concurrent sessions' KV separate or restore one
  session without the other;
- injection — it relies on llama-server's default slot assignment;
- cross-restart routing of a known session to its known slot.

## 4. Design conclusion

Three layers, each answering a different failure mode:

```
layer 0  llama-server internals (free):
         -sps LCP affinity + --cache-idle-slots
         -> while the model is up, conversations usually hit warm KV.
         -> we get this by template defaults, not code.

layer 1  proxy-side sticky slots (phase 2, works with 1 instance):
         session-id (header, or derived) -> slot registry (in head-end)
         -> proxy injects id_slot into every upstream body
         -> explicit save on idle/disconnect/unload + periodic save
         -> explicit restore on first request / model load
         state on a per-model PVC.
         -> survives: model swap, head-end restart, pod crash (within one
            save interval), -np >= #active sessions.

layer 2  cross-instance slot ownership (phase 3, multi-instance):
         registry becomes session -> (instance, slot)
         routing: known session -> its owner instance (sticky)
                  unknown session -> least-loaded instance with a free slot (Paddler-style)
         owner lost (scale-down/crash) -> pick new instance, restore slot from
         its PVC (reassignment; saved state makes this cheap)
```

Why layer 1 and not "just KV-aware routing":

1. **Single instance (our phase 1–2 reality):** routing has nowhere to choose —
   stickiness is the whole feature.
2. **OAI responses hide the slot** — passive KV-aware tracking is impossible
   through the API surface clients actually use; we must own the mapping.
3. **Persistence:** in-memory prompt cache (even with --cache-idle-slots) dies
   with the process. Disk save/restore is the only thing that survives unload /
   crash / node failure — and it requires knowing *which* slot, i.e. the sticky
   mapping.
4. **Determinism at -np 1..N:** pinning prevents one conversation's prefill
   from evicting another's slot (the `try_clear_idle_slots` / overwrite hazard).
5. **KV-aware routing still appears, where it belongs:** inside each
   llama-server (layer 0, free) and as the *fallback* policy for unknown
   sessions in phase 3 (least-loaded, which is Paddler's policy, not a prompt
   hash).

Session-ID strategy (open question in PLAN §7.3, recommendation):
- explicit header `X-Llama-Swap-Session: <opaque>` (clients/agents that can set
  it — most do, via httpx fetch kwargs / custom headers);
- fallback: hash of a stable prompt prefix (e.g. first system/user message,
  truncated) so stateless clients still get warm slots;
- non-text endpoints (embeddings, audio, images) bypass slot logic entirely.

Open risks to watch during phase 2:
- save/restore are synchronous inside llama-server's task queue — a multi-GB
  save can block other requests on that instance; keep saves off the request
  path (idle-only, async from the proxy), bound with timeouts;
- slot count vs `-np` vs `concurrencyLimit` must agree (one slot per
  concurrent session; the limit already exists per model);
- session-registry size: unbounded sessions -> cap (LRU by lastActive,
  evict = save+release slot), configurable.
