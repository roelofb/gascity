#!/usr/bin/env bash
# Lint and schema-validate the gascity Helm chart. Mirrors what
# .github/workflows/chart-validate.yml runs in CI so local iteration stays
# close to the gate.
set -euo pipefail

CHART_DIR=${CHART_DIR:-charts/gascity}
NAMESPACE=${NAMESPACE:-gc}
RELEASE=${RELEASE:-gc-controller}
RENDERED=${RENDERED:-$(mktemp -t gascity-rendered.XXXXXX).yaml}
trap 'rm -f "$RENDERED"' EXIT

# Resolve tool binaries up-front so local wrappers (rtk, shims) that filter
# child stdout don't eat the rendered manifest on the redirect.
HELM=$(command -v helm)
KUBECONFORM=$(command -v kubeconform)

echo "==> helm lint $CHART_DIR"
"$HELM" lint "$CHART_DIR"

echo "==> helm template $CHART_DIR -> $RENDERED"
"$HELM" template "$RELEASE" "$CHART_DIR" \
  --namespace "$NAMESPACE" \
  --include-crds >"$RENDERED"

echo "==> kubeconform $RENDERED ($(grep -c '^kind:' "$RENDERED") resources)"
"$KUBECONFORM" \
  -strict \
  -summary \
  -schema-location default \
  -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' \
  "$RENDERED"

echo "==> OK"
