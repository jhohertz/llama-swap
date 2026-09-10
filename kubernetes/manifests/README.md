# Sample/dev manifests for the llama-swap kubernetes backend (phase 0/1).
#
# Files:
#   rbac.yaml              - ServiceAccount + namespaced Role + RoleBinding
#   head-end.yaml          - dev llama-swap head-end (Deployment + Service +
#                            ConfigMap with wrapper-based model configs)
#   model-cache-pvc.yaml   - shared model cache PVC (dev) + one-off downloader
#
# kubernetes models are ordinary llama-swap models whose `cmd` runs the
# `kubeswap` wrapper (see PLAN.md §4) — no special backend keyword.
#
# All objects live in the `llama-swap` namespace. Nothing here is production.
apiVersion: v1
kind: Namespace
metadata:
  name: llama-swap
