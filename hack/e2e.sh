#!/usr/bin/env bash
# End-to-end test: external-dns with this webhook in a kind cluster, writing to
# the real NodeRings DNS API.
#
#   NODERINGS_TOKEN=... hack/e2e.sh
#
# It creates a throwaway zone (<random>.example.com by default), installs the
# official external-dns chart with the webhook sidecar, and checks on the
# NodeRings nameserver that an Ingress creates A and CNAME records, that a
# changed target updates them, and that deleting the Ingress removes them. The
# zone and the kind cluster are deleted at the end (KEEP_CLUSTER=1 keeps the
# cluster). It uses its own kubeconfig file and never touches ~/.kube/config.
set -euo pipefail

: "${NODERINGS_TOKEN:?set NODERINGS_TOKEN}"
API="${NODERINGS_API_URL:-https://api.noderings.com}"
CLUSTER="${CLUSTER:-noderings-edns-e2e}"
IMAGE="${IMAGE:-ghcr.io/noderings/external-dns-noderings-webhook:e2e}"
NAMESERVER="${NAMESERVER:-ans1.nrings.io}"
CHART_VERSION="${CHART_VERSION:-1.24.0}"
ZONE="edns-e2e-$(openssl rand -hex 3).${NODERINGS_TEST_DOMAIN:-example.com}"
WORK="$(mktemp -d)"
export KUBECONFIG="$WORK/kubeconfig"
ZONE_ID=""

log() { printf '\n== %s\n' "$*"; }

api() { # method path [json]
  local args=(-fsS -X "$1" -H "Authorization: Bearer $NODERINGS_TOKEN" -H "Content-Type: application/json")
  [[ -n "${NODERINGS_ORGANIZATION_ID:-}" ]] && args+=(-H "X-Organization-ID: $NODERINGS_ORGANIZATION_ID")
  [[ $# -ge 3 ]] && args+=(-d "$3")
  curl "${args[@]}" "$API$2"
}

cleanup() {
  local rc=$?
  log "Cleanup"
  if [[ -n "$ZONE_ID" ]]; then
    api DELETE "/v1/dns/zones/$ZONE_ID?force=true" >/dev/null && echo "deleted zone $ZONE"
  fi
  if [[ -z "${KEEP_CLUSTER:-}" ]]; then
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 && echo "deleted kind cluster $CLUSTER"
  fi
  rm -rf -- "$WORK"
  [[ $rc -eq 0 ]] && printf '\nEND TO END PASSED\n' || printf '\nEND TO END FAILED (exit %d)\n' "$rc"
}
trap cleanup EXIT

# wait_dns name type expected: poll the NodeRings nameserver until the answer matches.
wait_dns() {
  local name=$1 type=$2 want=$3 got=""
  for _ in $(seq 1 60); do
    got="$(dig +short @"$NAMESERVER" "$name" "$type" | sort | paste -sd, -)"
    if [[ "$got" == "$want" ]]; then
      echo "   $name $type -> ${got:-<none>}"
      return 0
    fi
    sleep 5
  done
  echo "   $name $type: want '${want:-<none>}', got '${got:-<none>}'" >&2
  kubectl -n external-dns logs deploy/external-dns --all-containers --tail=60 >&2 || true
  return 1
}

log "Zone $ZONE"
ZONE_ID="$(api POST /v1/dns/zones "{\"name\":\"$ZONE\"}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["zone"]["id"])')"
echo "   id $ZONE_ID"

log "kind cluster $CLUSTER"
kind get clusters 2>/dev/null | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER" --wait 120s
kind export kubeconfig --name "$CLUSTER" >/dev/null
kind load docker-image "$IMAGE" --name "$CLUSTER"

log "external-dns $CHART_VERSION with the NodeRings webhook"
kubectl create namespace external-dns --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n external-dns create secret generic noderings-token --from-literal=token="$NODERINGS_TOKEN" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
helm repo add external-dns https://kubernetes-sigs.github.io/external-dns/ >/dev/null 2>&1 || true
helm repo update external-dns >/dev/null
# The documented values, pointed at this run's zone and the local image.
sed "s/example\.com/$ZONE/g" "$(dirname "$0")/../examples/values.yaml" > "$WORK/values.yaml"
helm upgrade --install external-dns external-dns/external-dns --version "$CHART_VERSION" -n external-dns \
  -f "$WORK/values.yaml" \
  --set provider.webhook.image.repository="${IMAGE%:*}" --set provider.webhook.image.tag="${IMAGE##*:}" \
  --set provider.webhook.image.pullPolicy=Never \
  --set txtOwnerId=e2e --set interval=10s --set sources='{ingress}' \
  --wait --timeout 3m

log "Ingress with an address and an alias"
apply_ingress() { # target
  kubectl apply -f - >/dev/null <<EOF
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: web
  annotations:
    external-dns.kubernetes.io/target: "$1"
    external-dns.kubernetes.io/ttl: "120"
spec:
  rules:
    - host: web.$ZONE
      http: {paths: [{path: /, pathType: Prefix, backend: {service: {name: web, port: {number: 80}}}}]}
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: alias
  annotations:
    external-dns.kubernetes.io/target: "web.$ZONE"
spec:
  rules:
    - host: www.$ZONE
      http: {paths: [{path: /, pathType: Prefix, backend: {service: {name: web, port: {number: 80}}}}]}
EOF
}
apply_ingress 192.0.2.10
wait_dns "web.$ZONE" A 192.0.2.10
wait_dns "www.$ZONE" CNAME "web.$ZONE."
# external-dns writes the ownership TXT records after the records they describe.
for _ in $(seq 1 24); do
  owner_txt="$(dig +short @"$NAMESERVER" "a-web.$ZONE" TXT)"
  [[ "$owner_txt" == *"external-dns/owner=e2e"* ]] && break
  sleep 5
done
if [[ "$owner_txt" != *"external-dns/owner=e2e"* ]]; then
  echo "   no ownership TXT record: $owner_txt" >&2
  kubectl -n external-dns logs deploy/external-dns -c webhook --tail=40 >&2
  exit 1
fi
echo "   ownership TXT: $owner_txt"

log "Changed target"
apply_ingress 192.0.2.20
wait_dns "web.$ZONE" A 192.0.2.20

log "A record not owned by external-dns survives"
api PUT "/v1/dns/zones/$ZONE_ID/records" '{"record":{"name":"manual","type":"A","ttl":300,"rdata":["198.51.100.1"]}}' >/dev/null
sleep 25
wait_dns "manual.$ZONE" A 198.51.100.1

log "Deleting the Ingresses removes their records"
kubectl delete ingress web alias >/dev/null
wait_dns "web.$ZONE" A ""
wait_dns "www.$ZONE" CNAME ""
wait_dns "manual.$ZONE" A 198.51.100.1

log "Webhook log"
kubectl -n external-dns logs deploy/external-dns -c webhook --tail=200 | grep -E 'wrote|deleted|error' | head -20
