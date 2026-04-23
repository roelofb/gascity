#!/usr/bin/env bash
# Idempotent cluster bootstrap: installs ingress-nginx, cert-manager,
# sealed-secrets, VolumeSnapshotClass, and ClusterIssuers onto the current
# kube-context. Re-runnable; each helm upgrade is --install, and kubectl
# apply is declarative.
set -euo pipefail

BOOTSTRAP_DIR=${BOOTSTRAP_DIR:-"$(cd "$(dirname "$0")" && pwd)"}

echo "==> helm repos"
helm repo add jetstack https://charts.jetstack.io >/dev/null
helm repo add ingress-nginx https://kubernetes.github.io/ingress-nginx >/dev/null
helm repo add sealed-secrets https://bitnami-labs.github.io/sealed-secrets >/dev/null
helm repo update >/dev/null

echo "==> cert-manager"
helm upgrade --install cert-manager jetstack/cert-manager \
  --namespace cert-manager \
  --create-namespace \
  -f "$BOOTSTRAP_DIR/values/cert-manager.yaml" \
  --wait

echo "==> ingress-nginx"
helm upgrade --install ingress-nginx ingress-nginx/ingress-nginx \
  --namespace ingress-nginx \
  --create-namespace \
  -f "$BOOTSTRAP_DIR/values/ingress-nginx.yaml" \
  --wait

echo "==> sealed-secrets"
helm upgrade --install sealed-secrets sealed-secrets/sealed-secrets \
  --namespace kube-system \
  -f "$BOOTSTRAP_DIR/values/sealed-secrets.yaml" \
  --wait

echo "==> VolumeSnapshotClass"
kubectl apply -f "$BOOTSTRAP_DIR/volumesnapshotclass.yaml"

echo "==> ClusterIssuers"
kubectl apply -f "$BOOTSTRAP_DIR/cluster-issuer.yaml"

echo "==> LB IP (add a *.<domain> A record pointing here)"
kubectl -n ingress-nginx get svc ingress-nginx-controller \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}' || true
echo
