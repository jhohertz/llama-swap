# kubeswap

`kubeswap` is a standalone wrapper program that lets llama-swap manage
inference backends in a Kubernetes namespace the same way it manages docker
backends: the model's `cmd` launches `kubeswap serve`, which creates (or
adopts) a Deployment + Service for the model and proxies a local port to the
backend pod; the model's `cmdStop` runs `kubeswap delete` to tear the objects
down.

It provides four subcommands:

- `serve`: used as a model's `cmd`. Ensures the model's PVCs, Deployment and
  Service exist (adopting them if a previous run left them), then runs a
  forward proxy from `--listen` to the backend pod. The proxy answers
  `/health` with 503 until the pod is Ready, so llama-swap's health check
  gates on real readiness. Pod logs are forwarded to stderr. SIGTERM exits
  **without deleting anything** — the Deployment is kept so the next `serve`
  adopts it (survives head-end restarts and graceful reloads).
- `delete`: used as a model's `cmdStop`. Deletes the model's Deployment and
  Service (and the PVCs kubeswap itself created, with `--delete-volumes`),
  then waits for the pods to terminate so the GPU is released before
  llama-swap considers the stop done. A running `serve` process observes the
  deployment deletion and exits on its own.
- `gc`: one-shot garbage collection. Deletes managed workloads whose model
  is no longer in the given model list (`--models`) or llama-swap config
  (`--config path/to/config.yaml`). Use it to clean up leftovers from removed
  or renamed models.
- `status`: prints the managed workloads in the namespace.

## Why use this?

llama-swap's lifecycle engine (TTL, preload, eviction, config reload) works
on anything that looks like a process with a local proxy port. `kubeswap`
gives a Kubernetes backend exactly that shape, so all of llama-swap's model
management applies unchanged: a model config with `cmd: kubeswap serve ...`
swaps in and out of the GPU just like a docker backend does.

Each model gets one pod (one instance per model), scheduled onto a GPU node
via `--gpu` + `--node-selector`, with the model weights on a (usually shared)
PVC and slot state on an emptyDir.

## Prerequisites

- A Kubernetes cluster reachable with either:
  - an in-cluster ServiceAccount token (when llama-swap runs as a pod), or
  - a kubeconfig (`KUBECONFIG` or `~/.kube/config`) for a host-side
    llama-swap.
- RBAC for the namespace. A starter Role is in
  [`kubernetes/manifests/rbac.yaml`](../../kubernetes/manifests/rbac.yaml)
  (ServiceAccount + Role + RoleBinding: deployments/services create-get-list-watch-delete,
  PVCs get/create/delete, pods get/list/watch, pod logs, events).
- A StorageClass that supports `ReadWriteMany` if the model cache PVC should
  be reachable from every GPU node (e.g. longhorn, nfs). kubeswap creates
  missing PVCs with `--pvc-size` / `--pvc-class` / `--pvc-access-mode`;
  pre-created PVCs are adopted as-is and never relabeled.

## Installation

Build the binary from source:

```bash
go build -o kubeswap ./cmd/kubeswap
# or: make kubeswap
```

Or use the unified llama-swap image, which ships `/usr/local/bin/kubeswap`
built from the same revision.

## Usage in llama-swap

### As a model's `cmd`

Everything after `--` becomes the backend container's arguments.

#### GPU (AMD, `amd.com/gpu` device plugin)

```yaml
models:
  lfm25-230m:
    proxy: 127.0.0.1:${PORT}
    cmd: |
      kubeswap serve
      --listen 127.0.0.1:${PORT}
      --model lfm25-230m
      --namespace llama-swap
      --image ghcr.io/ggml-org/llama.cpp:server-vulkan
      --gpu amd.com/gpu=1
      --node-selector feature.node.kubernetes.io/amd-gpu=true
      --volume pvc:llama-swap-models:/models:ro
      --
      --model /models/LFM2.5-230M-Q4_0.gguf
      --port 8080
      --ctx-size 4096
      --threads 8
    cmdStop: kubeswap delete --model lfm25-230m --namespace llama-swap --wait 60s
    healthCheckTimeout: 120s
    ttl: 30m
```

#### GPU (NVIDIA, `nvidia.com/gpu` device plugin)

```yaml
models:
  qwen25:
    proxy: 127.0.0.1:${PORT}
    cmd: |
      kubeswap serve
      --listen 127.0.0.1:${PORT}
      --model qwen25
      --namespace llama-swap
      --image ghcr.io/ggml-org/llama.cpp:server-cuda
      --gpu nvidia.com/gpu=1
      --volume pvc:llama-swap-models:/models:ro
      --
      -hf bartowski/Qwen2.5-0.5B-Instruct-GGUF:Q4_K_M
      --port 8080
      --ctx-size 4096
    cmdStop: kubeswap delete --model qwen25 --namespace llama-swap
    healthCheckTimeout: 120s
    ttl: 30m
```

#### CPU (no `--gpu`, no node selector; lavapipe Vulkan works)

```yaml
models:
  smollm2:
    proxy: 127.0.0.1:${PORT}
    cmd: |
      kubeswap serve
      --listen 127.0.0.1:${PORT}
      --model smollm2
      --namespace llama-swap
      --image ghcr.io/ggml-org/llama.cpp:server-vulkan
      --volume pvc:llama-swap-models:/models:ro
      --
      --model /models/SmolLM2-135M-Instruct-Q4_K_M.gguf
      --port 8080
      --ctx-size 2048
      --threads 4
    cmdStop: kubeswap delete --model smollm2 --namespace llama-swap
    ttl: 30m
```

### Naming

The Deployment, Service and PVCs are named from the model ID, translated to
lowercase alphanumerics and dashes (e.g. `author/model:v1` →
`author-model-v1`). The original ID is preserved in the
`llama-swap.io/model-id` annotation; every object is labeled
`llama-swap.io/managed-by=llama-swap` and `llama-swap.io/model=<sanitized>`.
Use the **configured model ID** in `cmdStop`/`gc`, not the sanitized name.

### Volumes

`--volume` takes `kind:name:path[:ro]`, repeatable:

- `pvc:llama-swap-models:/models:ro` — existing or auto-created PVC
- `emptydir:slots:/slots` — per-pod scratch (KV slot state)
- `hostpath:/data/models:/models:ro` — path on the scheduled node

PVCs that already exist are adopted as-is (a shared model cache is typically
pre-created once, RWX, by the operator); missing ones are created with
`--pvc-size` (default `1Gi`), `--pvc-class` (default: cluster default) and
`--pvc-access-mode` (`rwo` or `rwx`). `delete --delete-volumes` only removes
PVCs kubeswap created itself (labeled); pre-created claims survive.

### Head-end in a cluster vs on the host

Both work. In-cluster, llama-swap runs as a pod with the `llama-swap`
ServiceAccount and `kubeswap` uses the pod's token; the proxy targets the
backend **pod IP** directly, so no extra network exposure is needed. On the
host, `kubeswap` falls back to the standard kubeconfig loading rules. If your
CNI does not let the head-end reach pod IPs, point `--upstream` at a
reachable URL (e.g. a manual port-forward) — the wrapper then uses it
verbatim instead of discovering the pod IP.

### Garbage collection

After removing models from your config, collect the orphaned workloads:

```bash
kubeswap gc --namespace llama-swap --config /etc/llama-swap/config/config.yaml
# or with an explicit allow-list:
kubeswap gc --namespace llama-swap --models lfm25-230m --models qwen25
```

A convenient pattern is an initContainer on the head-end that runs `gc` once
per start (the head-end config is already a ConfigMap mount):

```yaml
initContainers:
- name: kubeswap-gc
  image: <llama-swap image>
  command: ["/usr/local/bin/kubeswap", "gc", "--namespace", "llama-swap",
            "--config", "/etc/llama-swap/config/config.yaml"]
```

### Adoption and strict mode

If a Deployment with the model's name already exists, `serve` adopts it
(backend survives head-end restarts and config reloads). With `--strict`, a
deployment whose image/args/env/resources drifted from the config is
deleted and recreated instead of adopted.

### Troubleshooting

- `kubeswap status` — quick view of models, pods and readiness.
- `serve` forwards pod logs to stderr with a `[pod/<name>]` prefix, which
  llama-swap records in its log monitor.
- While the pod is not Ready, the proxy answers every request with 503 and
  JSON `{"status":"not-ready","reason":"..."}` (pod phase, waiting reason,
  restart counts) — the reason is visible in llama-swap's health checks.
- If log forwarding is refused by RBAC it is disabled once with a warning
  (transient "container still starting" errors are retried automatically).
