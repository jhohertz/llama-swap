# Kubernetes mechanics — client, RBAC, templates, testing

## 1. Test cluster (verified)

- context `maximus` (current), k3s **v1.35.5**, 9 nodes (3 control-plane, 6
  workers), all `amd64`.
- **GPU: every worker has 1× AMD Radeon 8060S** (RDNA4, 96 GB VRAM, 40 CUs),
  allocatable as **`amd.com/gpu: 1`** (AMD device plugin); node label
  `feature.node.kubernetes.io/amd-gpu: "true"`.
- namespace `llama-swap` exists and is empty — our scratch space.
- Implications:
  - **Vulkan image is the natural fit** (`server-vulkan` bundles the RADV AMD
    ICD + lavapipe CPU fallback — verified by inspecting the image).
  - GPU request + nodeSelector are trivial to express in the template, but
    **CPU is the sanctioned fallback** (user decision): no GPU request →
    lavapipe; CI is CPU-only anyway, so keep the CPU path green always.
  - tiny models (LFM2.5-230M Q4, gemma-3-270m, Qwen3-0.6B class) regardless
    — fast loads, fit on GPU or CPU.
  - k3s specifics: no cloud provider, no default ingress; `kind` in CI will
    mirror it closely enough (CPU only).

## 2. Talking to the API

Options considered:

| option | pros | cons | verdict |
|---|---|---|---|
| **`k8s.io/client-go`** (typed clientset + informers) | standard; watch/retry/backoff/serialization/token-rotation for free; informer cache makes `State()` cheap and event-driven | big dependency tree | **chosen** — Kubernetes APIs are an approved technology in AGENTS.md |
| raw REST (`net/http` + SA token file) | zero deps | hand-roll: watch resume (resourceVersion), 409 retry, token rotation, pagination; more code, more bug surface | rejected (kept as reference for what client-go saves us from) |
| `kubectl` subprocess | no Go deps at all | ugly, slow, fragile parsing, no watch ergonomics | no |
| controller-runtime | the "operator" path | built for CRDs/webhooks — we explicitly have none; pulls in the worst of client-go plus scheme machinery | no (revisit only if we adopt CRDs in phase 5) |

Client details (client-go path — all inside the `kubeswap` wrapper binary,
not the llama-swap server):

- **In-cluster auth:** `rest.InClusterConfig()` — ServiceAccount token from
  `/var/run/secrets/kubernetes.io/serviceaccount` (the head-end pod's
  `automountServiceAccountToken`); API-server-rotated bound tokens (1h) handled
  by client-go's token-file watcher.
- **Out-of-cluster dev:** fall back to kubeconfig (`clientcmd`) when not in
  cluster, so a laptop `llama-swap` (host head-end) can manage the test
  cluster — mirrors how the docker backend uses the local docker socket.
- **Informers:** one shared `SharedInformerFactory` scoped to the managed
  namespace, informers on `deployments`, `pods` (+`persistentvolumeclaims` if
  we ever create them). `serve` readiness and the "my Deployment was deleted →
  exit" logic read the cache; the warm-up window is covered by a bounded
  direct `Get`. The llama-swap server itself never talks to the cluster.
- **Namespace-scoped only.** The client is built for one namespace; every call
  is namespaced. No cluster informer, no cluster RBAC.

## 3. RBAC (namespaced, minimal)

```yaml
apiVersion: v1
kind: ServiceAccount
metadata: { name: llama-swap, namespace: llama-swap }
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: { name: llama-swap, namespace: llama-swap }
rules:
  - apiGroups: [apps]
    resources: [deployments]
    verbs: [create, get, list, watch, delete]
  - apiGroups: [""]
    resources: [services]
    verbs: [create, get, list, watch, delete]
  - apiGroups: [""]
    resources: [persistentvolumeclaims]
    verbs: [get, create, delete]
  - apiGroups: [""]
    resources: [pods]
    verbs: [get, list, watch]
  - apiGroups: [""]
    resources: [pods/log]
    verbs: [get]            # -> logmon, so /logs/stream/{model} works
  - apiGroups: [""]
    resources: [events]
    verbs: [get, list]      # debuggability of provisioning failures
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: { name: llama-swap, namespace: llama-swap }
subjects: [{ kind: ServiceAccount, name: llama-swap, namespace: llama-swap }]
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: Role, name: llama-swap }
```

No `patch` on deployments in phase 1 (scale-up lands in phase 3 — add then).

## 4. Object templates

### 4.1 Labeling & naming (collision-proofing)

- Every object: `llama-swap.io/managed-by: llama-swap`,
  `llama-swap.io/model: <sanitized-id>`.
- Model IDs may contain `/` and `:` (e.g. `author/model`, peer FQNs) —
  Kubernetes names are DNS-1123 (lowercase, alnum, `-`). Sanitize for
  names/labels (`/`→`-`, `:`→`-`), keep the **original ID** in an annotation
  (`llama-swap.io/model-id`) so adoption and lookup are unambiguous.
  (Same collision concern the peer FQN code already handles.)
- Service name: `<sanitized>-svc` (keeps `http://<svc>.<ns>.svc` predictable —
  the process's proxy URL is derivable *before* anything exists).

### 4.2 Deployment (phase 1)

```yaml
metadata:
  name: qwen06
  namespace: llama-swap
  labels: { llama-swap.io/managed-by: llama-swap, llama-swap.io/model: qwen06 }
  annotations: { llama-swap.io/model-id: qwen06 }
spec:
  replicas: 1
  strategy: { type: Recreate }          # 1 instance per model; no overlapping pods
  selector: { matchLabels: { app: qwen06, llama-swap.io/managed-by: llama-swap } }
  template:
    metadata: { labels: <same + app> }
    spec:
      terminationGracePeriodSeconds: <from unloadTimeout>
      initContainers: [ ]               # e.g. HF download for testing
      containers:
        - name: server
          image: <deploy.image>
          args: <deploy.args>           # --port fixed by template, e.g. 8080
          env: <deploy.env>
          ports: [{ containerPort: 8080 }]
          readinessProbe:
            httpGet: { path: /health, port: 8080 }
            initialDelaySeconds: 2
            periodSeconds: 5
          livenessProbe:
            httpGet: { path: /health, port: 8080 }
            initialDelaySeconds: 30     # give model load a long runway
            periodSeconds: 15
          resources: <deploy.resources> # e.g. limits: {"amd.com/gpu": "1"}
          volumeMounts: <from deploy.volumes>
      volumes: <from deploy.volumes>
      nodeSelector: <deploy.nodeSelector>
      tolerations: <deploy.tolerations>
```

- `strategy: Recreate` — a backend is a single instance; `RollingUpdate` would
  briefly run two pods fighting for the same GPU/PVC.
- `terminationGracePeriodSeconds` tied to `unloadTimeout` so the pod's
  preStop/shutdown (llama-server SIGTERM → clean slot flush, if any) fits inside
  llama-swap's unload budget.

### 4.3 Volumes

- **Model cache:** shared PVC (e.g. `llama-swap-models`), mounted read-only in
  pods; for tests, an init container downloads the tiny GGUF into an `emptyDir`
  (model store integration is explicitly later).
- **Slot state (phase 2):** per-model PVC `<sanitized>-slots`, RW, mounted at
  the `--slot-save-path` dir. **Kept on unload** (`keepPvcOnDelete: true`
  default) — deleting the Deployment must not trash saved KV. PVC creation is
  idempotent (get-or-create with 409 handling) so a user can pre-create it
  (size/storageClass) and llama-swap adopts it.
- **Phase 3:** per-replica slot PVCs → StatefulSet + `volumeClaimTemplates`
  (the prompt's "needs statefulset when balancing becomes in scope" — confirmed;
  the template renderer must treat Deployment/StatefulSet as two renderings of
  the same `deploy` block).

### 4.4 Service

```yaml
metadata: { name: qwen06-svc, namespace: llama-swap, labels: <managed> }
spec:
  type: ClusterIP
  selector: { app: qwen06, llama-swap.io/managed-by: llama-swap }
  ports: [{ name: http, port: 8080, targetPort: 8080 }]
```

No LoadBalancer/NodePort: clients talk to the head-end; the Service exists so
the head-end has a stable proxy target.

## 5. Leftover discovery & adoption (wrapper-side)

With the wrapper, adoption is **per-model and happens inside `kubeswap serve`
when llama-swap launches it** (not a head-end-wide sweep):

1. `serve` lists Deployments labeled `llama-swap.io/managed-by: llama-swap`
   matching its `--model`.
   - one exists → *adopt*: watch it, proxy to it, log adoption. If the live
     spec differs from the rendered flags (user edited it, image changed)
     → default = adopt + log diff; `--strict` = delete + re-render.
   - none exists → create PVC(s) (if requested) → Deployment → Service.
2. **Orphans** (model not in config, no wrapper to adopt) are the one thing
   that needs a sweep: `kubeswap gc` (one-shot, idempotent). Trigger options
   in PLAN §8.3: head-end initContainer (once per start — recommended), a
   llama-swap `hooks` entry, or manual/cron. `gc` deletes Deployments+Services
   for models not in the given list; keeps volumes by default.
3. Config reload / head-end restart uses the same per-model adoption: old
   wrappers SIGTERM (keep Deployments), new wrappers adopt them (see codebase
   note §4).

## 6. Health of the health check

- llama-swap's `healthCheckTimeout` (default 30s) bounds the readiness wait.
  k8s provisioning (pull + model load) is minutes, not seconds → raise it per
  model in the wrapper's `cmd` config (e.g. 120s); `serve` answers `/health`
  503 until pod-ready so the whole window is spent usefully polling, and the
  request-side "loading state" streaming (queue position) covers UX.
- Pod failures (CrashLoopBackOff): the wrapper keeps returning 503 with the
  pod status message in the body/logs; once `healthCheckTimeout` expires
  llama-swap reports the error to the caller (`pods/log` + events are in RBAC
  for exactly this).
- Head-end crash mid-provision: the wrapper is a child and dies, but the
  Deployment is declarative and complete (or not started at all); on restart
  the new wrapper adopts or creates cleanly — nothing is half-owned.

## 7. CI / local loop

- `kind` cluster in a GitHub job (k3s is close enough; `kind` is the CI
  standard). **CPU-only in CI** (no GPU pass): the same
  `ghcr.io/ggml-org/llama.cpp:server-vulkan` image runs via lavapipe; keep CI
  models at ≤ 300 MB (LFM2.5-230M-Q4_0, see PLAN §5).
- The phase-1 acceptance list (PLAN §4/Phase 1) becomes Go tests that run
  against the kind cluster, gated behind an env flag (no cluster in normal
  `make test-dev`); unit tests cover the pure `deploy spec -> k8s object`
  renderer with no cluster at all.
