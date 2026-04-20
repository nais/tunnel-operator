# Deployment Plan: UDP Forwarder Architecture

## Overview

This plan covers deploying the STUN → UDP forwarder rewrite across all components. The new architecture is:

```
Client (CLI) → LB VIP:port → Forwarder pod → WireGuard → Gateway pod → Target service
```

**Repos involved:**

| Repo | Branch | Commits ahead | What changed |
|---|---|---|---|
| tunnel-operator | `main` | 15 ahead of `origin/main` | CRD, operator, forwarder, gateway, Helm charts, metrics |
| api | `ptp_wireguard` | 16 ahead of `origin/ptp_wireguard` | Tunnel domain, forwarder endpoint, metrics |
| cli | `ptp_wireguard` | 8 ahead of `origin/ptp_wireguard` | Forwarder endpoint flow, no STUN |

**Breaking changes:**
- CRD fields changed (STUN fields removed, forwarder fields added)
- API returns `forwarderEndpoint` instead of `stunEndpoint`
- CLI expects `forwarderEndpoint` — old CLI versions won't work
- Gateway no longer performs STUN — uses WireGuard roaming
- Any existing Tunnel CRs will need to be recreated (new spec shape)

---

## Pre-deployment Checklist

- [ ] All 3 repos pass tests locally (verified ✅)
- [ ] Push tunnel-operator to `origin/main`
- [ ] Push api `ptp_wireguard` branch → create PR → merge
- [ ] Push cli `ptp_wireguard` branch → create PR → merge
- [ ] CI builds 3 container images:
  - `tunnel-operator` (from `/Dockerfile`)
  - `tunnel-operator-forwarder` (from `/cmd/forwarder/Dockerfile`)
  - `tunnel-operator-gateway` (from `/cmd/gateway/Dockerfile`)
- [ ] Verify images are pushed to `europe-north1-docker.pkg.dev/nais-io/nais/images/`
- [ ] Import `dashboard.json` into Grafana (or add to dashboard provisioning)

---

## Phase 1: CRD Update (all tenant clusters)

**What:** Deploy the updated Tunnel CRD to all tenant clusters via the `tunnel-operator-tenant` chart.

**Why first:** The CRD must accept the new fields (`forwarderPort`, `forwarderEndpoint`) before the operator tries to write them. Old STUN fields are removed, so any existing Tunnel CRs will lose those fields (acceptable — they're being replaced).

**Steps:**
1. Deploy `tunnel-operator-tenant` chart to all tenant clusters
   - This updates `config/crd/bases/nais.io_tunnels.yaml`
   - RBAC in tenant clusters is unchanged (no Services permission needed — gateways are pods)
2. Verify CRD is applied: `kubectl get crd tunnels.nais.io -o yaml | grep forwarderPort`

**Rollback:** Re-deploy old CRD version. New fields are additive, so the old operator simply ignores them.

---

## Phase 2: Operator + Forwarder (management cluster)

**What:** Deploy the new operator and forwarder in the management cluster via the `tunnel-operator` chart.

**Why second:** The operator must be running before clients can create tunnels. The forwarder must be running for traffic to flow.

**Steps:**
1. Set Helm values — verify:
   ```yaml
   forwarder:
     enabled: true
     grpcAddr: "tunnel-operator:9090"  # operator's gRPC service
   grpc:
     port: 9090
   gatewayImage: <new gateway image tag>
   ```
2. Deploy `tunnel-operator` chart
3. Wait for rollout:
   ```bash
   kubectl rollout status deployment/tunnel-operator
   kubectl rollout status deployment/tunnel-operator-forwarder
   ```
4. Verify forwarder LB gets a VIP:
   ```bash
   kubectl get svc tunnel-operator-forwarder -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
   ```
5. Verify operator discovers the VIP (check operator logs for VIP log line)
6. Verify gRPC connectivity:
   ```bash
   kubectl logs deployment/tunnel-operator-forwarder | grep "connected to"
   ```
7. Verify metrics endpoints:
   ```bash
   # Operator metrics (port 8080)
   kubectl port-forward deployment/tunnel-operator 8080 &
   curl -s localhost:8080/metrics | grep tunnel_operator_

   # Forwarder metrics (port 8080)
   kubectl port-forward deployment/tunnel-operator-forwarder 8081:8080 &
   curl -s localhost:8081/metrics | grep tunnel_forwarder_
   ```

**What gets created:**
- Operator Deployment (1 replica) with gRPC server on `:9090`
- Forwarder Deployment (2 replicas, HPA 2-10)
- Forwarder Service (type LoadBalancer, GCP L4 pass-through)
- Forwarder HPA
- Forwarder NetworkPolicy (egress: DNS + gateway:51820 + operator:9090 only)
- ClusterRole/ClusterRoleBinding for operator (includes Services read for VIP discovery)

**Rollback:** `helm rollback` to previous chart version. Delete forwarder resources if they don't exist in old chart.

---

## Phase 3: API Deployment

**What:** Deploy the updated API that returns `forwarderEndpoint` in tunnel responses.

**Why third:** The API needs the operator + forwarder already running so that newly created tunnels get a `forwarderEndpoint` assigned.

**Steps:**
1. Merge `ptp_wireguard` branch into main
2. Deploy API (standard deployment process)
3. Verify tunnel creation returns forwarder endpoint:
   ```graphql
   mutation {
     createTunnel(input: {
       teamSlug: "myteam"
       environmentName: "dev"
       targetHost: "some-service.namespace.svc.cluster.local"
       targetPort: 5432
       clientPublicKey: "<test-key>"
     }) {
       tunnel {
         name
         forwarderEndpoint
         phase
       }
     }
   }
   ```
4. Verify metrics:
   ```bash
   curl -s <api-metrics-endpoint>/metrics | grep tunnel_api_operations_total
   ```

**Rollback:** Redeploy previous API version. Old API expects STUN fields which no longer exist in the CRD — would need CRD rollback too (Phase 1 rollback).

---

## Phase 4: CLI Release

**What:** Release the updated CLI that uses `forwarderEndpoint` instead of STUN.

**Why last:** Clients must only be updated after the full backend (operator + forwarder + API) is ready. Old CLI versions will stop working once the API stops returning STUN endpoints.

**Steps:**
1. Merge `ptp_wireguard` branch into main
2. Tag a release
3. Verify end-to-end tunnel flow:
   ```bash
   nais tunnel connect --team myteam --env dev --target-host some-service --target-port 5432
   ```
4. Verify traffic flows through: Client → Forwarder (UDP) → WireGuard → Gateway → Target

**Rollback:** Users downgrade CLI. But old CLI won't work with new API — requires full stack rollback.

---

## Phase 5: Validation

**End-to-end smoke test:**
1. Create a tunnel via CLI
2. Verify gateway pod starts in tenant cluster
3. Verify forwarder assigns a unique port (check operator logs)
4. Verify WireGuard handshake completes
5. Send TCP traffic through the tunnel
6. Verify traffic appears in Grafana dashboard:
   - Forwarder: packets/bytes flowing, active session
   - Gateway: TCP connection, bytes transferred
   - Operator: reconciliation success, tunnel in Ready phase
   - API: create operation counted

**Gateway metrics verification:**
```bash
# Find gateway pod in tenant cluster
kubectl get pods -l app=tunnel-gateway -n <team-namespace> --context <tenant-cluster>
kubectl port-forward <gateway-pod> 9091 --context <tenant-cluster>
curl -s localhost:9091/metrics | grep tunnel_gateway_
```

---

## Rollback Strategy

| Failure at | Action | Impact |
|---|---|---|
| Phase 1 (CRD) | Re-apply old CRD | None — operator hasn't changed yet |
| Phase 2 (Operator) | `helm rollback` operator chart | No tunnel creation until fixed |
| Phase 3 (API) | Redeploy old API + rollback Phase 1-2 | Full stack rollback needed |
| Phase 4 (CLI) | Users downgrade CLI + rollback Phase 1-3 | Full stack rollback needed |

**Key insight:** Phases 1-2 can be rolled back independently. Once Phase 3 (API) deploys, rollback requires reverting all phases because the API, CRD, and operator are tightly coupled on the new schema.

---

## Post-deployment

- [ ] Delete any leftover STUN-related resources (if they existed as separate deployments)
- [ ] Monitor Grafana dashboard for traffic flow
- [ ] Verify HPA scales forwarder under load
- [ ] Confirm NetworkPolicy blocks unexpected egress (test with `kubectl exec` into forwarder pod)
- [ ] **Still TODO:** Remove `TeamSlug` and `Environment` fields from CRD (separate change, lower priority)

---

## Open Items

1. **VIP binding not yet validated on GKE** — The spike (`docs/spike-vip-binding.md`) concluded `VIPBindingNeeded = true` based on documentation research, not live testing. After deployment, capture packets to confirm UDP reply source IP matches the LB VIP. If it doesn't, implement `IP_PKTINFO`-based source selection in the forwarder.

2. **TeamSlug/Environment CRD cleanup** — These fields are redundant (namespace = team, operator knows environment). Removal is a separate change to avoid coupling it with this deployment.
