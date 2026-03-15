#!/usr/bin/env bash
#
# k3s-dev-env.sh — Setup or teardown a single-node k3s cluster for testing the
# Tenstorrent device plugin on a host with real hardware (Ubuntu 22.04, Docker,
# Tenstorrent KMD and cards installed).
#
# Usage:
#   ./scripts/k3s-dev-env.sh setup    # Install k3s, build plugin, deploy DaemonSet
#   ./scripts/k3s-dev-env.sh teardown # Remove DaemonSet and uninstall k3s
#

set -euo pipefail

# --- Script directory and repo root (script lives in scripts/)
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Image tag used for local build and in the DaemonSet
DEVICE_PLUGIN_IMAGE="${DEVICE_PLUGIN_IMAGE:-k8s-device-plugin:local}"
K3S_KUBECONFIG="${K3S_KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}"

# --- Logging helpers
LOG_PREFIX="[k3s-dev-env]"

log_stage() {
  echo ""
  echo "$LOG_PREFIX >>> $*"
}

log_ok() {
  echo "$LOG_PREFIX     OK: $*"
}

log_fail() {
  echo "$LOG_PREFIX     FAIL: $*" >&2
}

die() {
  log_fail "$@"
  exit 1
}

# --- Ensure we have sudo
check_sudo() {
  if ! sudo -n true 2>/dev/null; then
    echo "$LOG_PREFIX This script needs sudo for installing k3s and importing images."
    echo "$LOG_PREFIX Please run with a user that has sudo access."
    exit 1
  fi
}

# --- Setup: install k3s, build plugin, deploy DaemonSet
do_setup() {
  check_sudo
  cd "$REPO_ROOT"

  # --- Stage 1: Install k3s
  log_stage "Stage 1/6 — Checking / installing k3s"
  if command -v k3s >/dev/null 2>&1; then
    log_ok "k3s already installed: $(k3s --version 2>/dev/null || true)"
  else
    log_ok "Downloading and installing k3s..."
    curl -sfL https://get.k3s.io | sudo sh -s - || die "k3s install failed. Check network and try again."
    log_ok "k3s installed."
  fi

  # Ensure k3s server is running
  if ! sudo systemctl is-active --quiet k3s 2>/dev/null; then
    log_ok "Starting k3s service..."
    sudo systemctl start k3s || die "Failed to start k3s. Check: sudo journalctl -u k3s -f"
  fi
  log_ok "k3s service is running."

  # --- Stage 2: Wait for node and configure kubectl
  log_stage "Stage 2/6 — Waiting for node and configuring kubectl"
  mkdir -p "$HOME/.kube"
  if [[ ! -f "$K3S_KUBECONFIG" ]]; then
    die "k3s kubeconfig not found at $K3S_KUBECONFIG. Is k3s running? Check: sudo systemctl status k3s"
  fi
  sudo cp "$K3S_KUBECONFIG" "$HOME/.kube/config"
  sudo chown "$(id -u):$(id -g)" "$HOME/.kube/config"
  export KUBECONFIG="$HOME/.kube/config"
  log_ok "KUBECONFIG set to $KUBECONFIG"

  for i in {1..30}; do
    if kubectl get nodes --no-headers 2>/dev/null | grep -q Ready; then
      log_ok "Node is Ready."
      break
    fi
    if [[ $i -eq 30 ]]; then
      die "Node did not become Ready in time. Check: kubectl get nodes && sudo journalctl -u k3s -f"
    fi
    sleep 2
  done

  # --- Stage 3: Build device plugin image
  log_stage "Stage 3/6 — Building device plugin Docker image"
  if ! command -v docker >/dev/null 2>&1; then
    die "Docker is not installed or not in PATH. Install Docker and ensure the user can run 'docker build'."
  fi
  docker build -t "$DEVICE_PLUGIN_IMAGE" . || die "Docker build failed. Fix build errors and re-run."
  log_ok "Image built: $DEVICE_PLUGIN_IMAGE"

  # --- Stage 4: Import image into k3s containerd
  log_stage "Stage 4/6 — Importing image into k3s"
  docker save "$DEVICE_PLUGIN_IMAGE" | sudo k3s ctr images import - || die "Failed to import image into k3s. Is k3s running?"
  log_ok "Image imported into k3s."

  # --- Stage 5: Deploy device plugin DaemonSet
  log_stage "Stage 5/6 — Deploying Tenstorrent device plugin DaemonSet"
  # Use a temp manifest with our local image so we don't modify the repo file (preserve YAML indentation)
  sed 's|^\([[:space:]]*image:\).*|\1 '"${DEVICE_PLUGIN_IMAGE}"'|' device-plugin-daemonset.yaml > /tmp/device-plugin-daemonset-local.yaml
  DAEMONSET_MANIFEST="/tmp/device-plugin-daemonset-local.yaml"
  kubectl apply -f "$DAEMONSET_MANIFEST" || die "Failed to apply DaemonSet. Check manifest: $DAEMONSET_MANIFEST"
  log_ok "DaemonSet applied."

  # --- Stage 6: Wait for DaemonSet to be ready
  log_stage "Stage 6/6 — Waiting for device plugin DaemonSet to be ready"
  kubectl rollout status daemonset/tenstorrent-device-plugin -n kube-system --timeout=120s || die "DaemonSet did not become ready. Check: kubectl get pods -n kube-system -l app=tenstorrent-device-plugin && kubectl describe pod -n kube-system -l app=tenstorrent-device-plugin"
  log_ok "Device plugin DaemonSet is ready."

  # --- Show allocatable and next steps
  echo ""
  echo "$LOG_PREFIX ============================================================"
  echo "$LOG_PREFIX Device plugin is running. Node allocatable resources:"
  echo "$LOG_PREFIX ============================================================"
  NODE="$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')"
  if command -v jq >/dev/null 2>&1; then
    kubectl get node "$NODE" -o json | jq -r '.status.allocatable | to_entries[] | "  \(.key): \(.value)"' 2>/dev/null || true
  else
    echo "  (install jq to see allocatable here, or run: kubectl get node $NODE -o yaml)"
  fi
  echo ""
  echo "$LOG_PREFIX To run a pod that requests a Tenstorrent device (edit resource name if needed):"
  echo ""
  echo "  kubectl apply -f $REPO_ROOT/example-workload.yaml"
  echo ""
  echo "$LOG_PREFIX Then check that the pod is running and has the device:"
  echo ""
  echo "  kubectl get pods"
  echo "  kubectl describe pod tt-metal-dev"
  echo ""
  echo "$LOG_PREFIX To tear down this environment later, run:"
  echo ""
  echo "  $0 teardown"
  echo ""
}

# --- Teardown: remove DaemonSet and uninstall k3s
do_teardown() {
  check_sudo
  cd "$REPO_ROOT"

  log_stage "Teardown — Removing device plugin and k3s"

  # Try to remove Kubernetes resources if kubectl and cluster still exist
  export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"
  if [[ -f "$KUBECONFIG" ]] && kubectl get nodes >/dev/null 2>&1; then
    log_ok "Deleting device plugin DaemonSet and example workload..."
    kubectl delete -f device-plugin-daemonset.yaml --ignore-not-found --timeout=30s 2>/dev/null || true
    kubectl delete -f example-workload.yaml --ignore-not-found --timeout=10s 2>/dev/null || true
    log_ok "Kubernetes resources removed."
  else
    log_ok "No cluster reachable (or no kubeconfig); skipping resource deletion."
  fi

  # Uninstall k3s
  if command -v k3s >/dev/null 2>&1; then
    if [[ -x /usr/local/bin/k3s-uninstall.sh ]]; then
      log_ok "Running k3s uninstall script..."
      sudo /usr/local/bin/k3s-uninstall.sh || die "k3s uninstall script failed."
      log_ok "k3s uninstalled."
    else
      log_fail "k3s binary found but /usr/local/bin/k3s-uninstall.sh not found. Uninstall manually if needed."
    fi
  else
    log_ok "k3s not installed; nothing to uninstall."
  fi

  # Optional: remove local kubeconfig copy so we don't leave a stale config
  if [[ -f "$HOME/.kube/config" ]]; then
    if grep -q "rancher" "$HOME/.kube/config" 2>/dev/null; then
      rm -f "$HOME/.kube/config"
      log_ok "Removed local kubeconfig ($HOME/.kube/config)."
    fi
  fi

  echo ""
  echo "$LOG_PREFIX Teardown complete. k3s and the device plugin environment have been removed."
  echo ""
}

# --- Main
usage() {
  echo "Usage: $0 { setup | teardown }"
  echo ""
  echo "  setup    — Install k3s, build the device plugin image, deploy the DaemonSet,"
  echo "             and wait for it to be ready. Prints a command to run a test pod."
  echo "  teardown — Delete the DaemonSet and uninstall k3s (and clean kubeconfig)."
  echo ""
  echo "Optional environment variables:"
  echo "  DEVICE_PLUGIN_IMAGE  — Docker image tag for the plugin (default: k8s-device-plugin:local)"
  echo "  K3S_KUBECONFIG       — Path to k3s kubeconfig (default: /etc/rancher/k3s/k3s.yaml)"
  exit 1
}

case "${1:-}" in
  setup)
    do_setup
    ;;
  teardown)
    do_teardown
    ;;
  *)
    usage
    ;;
esac
