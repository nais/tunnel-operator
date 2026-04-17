# Spike: UDP VIP binding behind GKE passthrough L4 load balancer

## Problem statement

Question:

> When a UDP forwarder pod behind a GCP L4 pass-through load balancer on GKE (with `externalTrafficPolicy: Local`) sends a response back to a client, does the response automatically leave with the load balancer VIP as source, or does it leave with the pod/node IP?

This matters because the forwarder is part of a NAT-sensitive UDP path. If the client sends to the LB VIP but receives the reply from a different source IP, the client or its NAT can drop the packet.

## Decision

**YES — the forwarder needs VIP binding (or an equivalent kernel/network rule) for UDP responses.**

Put differently: **NO, GKE/GCP does not automatically rewrite UDP responses so they appear from the LB VIP on the return path.**

`const VIPBindingNeeded = true`

This is the most accurate documented answer for a GKE Service backed by a **GCP regional external passthrough Network Load Balancer** (`cloud.google.com/l4-rbs: "enabled"`) with `externalTrafficPolicy: Local`.

## Why

1. **GCP's UDP passthrough LB documentation explicitly describes the DSR problem for UDP.**
   GCP documents that UDP is stateless, so the kernel does not inherently know that a reply should use the load balancer VIP as its source. Without explicit handling, the source address used for the response is the interface/socket address, not the VIP.

2. **GKE with `externalTrafficPolicy: Local` preserves client source IP on ingress; it does not promise automatic VIP sourcing on egress.**
   The relevant behavior is source-IP preservation for requests and local-node forwarding semantics. That helps the pod see the real client IP. It does not mean UDP replies are source-rewritten to the LB VIP.

3. **GCP/GKE document Direct Server Return for responses.**
   Response traffic bypasses the load balancer datapath. That means there is no LB component on the return path that can rewrite a pod's UDP reply back to the VIP for the application.

4. **Kubernetes source-IP docs are about preserving the client IP on requests.**
   They do not say that UDP reply packets from a backend pod automatically gain the Service/LB VIP as their source.

5. **Recent Kubernetes UDP conntrack issues reinforce that UDP needs explicit care.**
   Existing issues and fixes around UDP conntrack and blackholing show that relying on TCP-like connection semantics for UDP is unsafe.

## Research findings

### Source 1: GCP official UDP passthrough LB guidance

Source:
- https://cloud.google.com/load-balancing/docs/network/udp-with-network-load-balancing

Relevant finding:
- GCP documents that for UDP with passthrough network load balancing, the kernel does not automatically know to send the response back with the VIP as source because UDP is stateless.
- GCP's documented fixes are all explicit mechanisms: iptables DNAT, nftables mangling, application bind to the VIP, or ancillary data (`IP_PKTINFO`) handling.

Interpretation:
- If automatic conntrack/VIP rewriting already solved this for UDP responses, GCP would not need to document these explicit remedies.

### Source 2: GKE LoadBalancer Service behavior

Source:
- https://cloud.google.com/kubernetes-engine/docs/concepts/service-load-balancer

Relevant finding:
- With `externalTrafficPolicy: Local`, GKE preserves client source IP on ingress.
- GKE documents node-side DNAT to the Pod and documents that `Local` avoids SNAT when traffic is delivered to a local Pod.
- GKE LoadBalancer Services are implemented with passthrough Network Load Balancers, whose responses use Direct Server Return behavior.

Interpretation:
- DSR means the return path bypasses the LB, so there is no automatic return-path rewrite to the VIP.
- No SNAT on Local means the source remains what the backend kernel/application emits.

### Source 3: Backend service-based external passthrough LB architecture

Source:
- https://cloud.google.com/load-balancing/docs/network/networklb-backend-service

Relevant finding:
- The forwarding rule IP is the destination of the incoming packet.
- The load balancer preserves source IP addresses of incoming packets.
- Responses from backend VMs go directly to the client, not back through the load balancer.

Interpretation:
- The VIP is on the ingress packet.
- The return path is direct from backend to client, so any source-IP choice for UDP replies must come from backend socket/kernel behavior, not from the load balancer.

## Answer to the core question

> Does the forwarder need to bind outbound sockets to the LB VIP IP?

**YES.**

For this architecture, the documented packet flow is:
- incoming packet destination: LB VIP
- GKE node performs DNAT to the Pod IP/target port
- with `externalTrafficPolicy: Local`, client source IP is preserved to the Pod
- pod's UDP response is **not** automatically rewritten to the LB VIP on the way back out
- therefore the forwarder must use VIP binding or an equivalent mechanism so replies present the LB VIP as source

Without that, the client can observe the reply as coming from the pod/node IP instead of the VIP, which breaks NAT-sensitive traversal semantics.

## Actionable guidance for Task 11

Task 11 should be implemented as if `VIPBindingNeeded = true`.

Acceptable approaches:

1. **Application-level VIP binding**
   - Bind the UDP socket to the LB VIP when possible.
   - Or use `recvmsg`/`sendmsg` with `IP_PKTINFO`/equivalent per-packet source selection so replies use the destination IP that the client originally targeted.
   - On Linux, binding to a non-local VIP might require `net.ipv4.ip_nonlocal_bind=1`, which is a deployment constraint to validate on GKE.

2. **Kernel/network-level rewrite**
   - Use iptables/nftables rules that preserve request delivery to the pod while forcing reply source IP to the LB VIP.

Recommended Task 11 stance:
- do **not** rely on kube-proxy/conntrack automatically rewriting the UDP response to the VIP
- design the operator/forwarder API so the forwarder can learn the VIP and use it explicitly
- validate with packet capture from an external client

## Suggested validation on GKE

1. Deploy a forwarder pod behind the external passthrough LB.
2. Send UDP from an external host to the LB VIP.
3. Capture packets on the node/pod and on the external host.
4. Confirm whether the response source is:
   - desired: LB VIP
   - failure mode: pod IP or node IP
5. Repeat after enabling explicit VIP source selection.

## Local spike program

See `cmd/forwarder/spike_vip_binding.go`.

That program is **not** a full GKE dataplane test. It exists to document the decision and to demonstrate the more general rule that UDP source selection follows the local socket/address unless the application explicitly chooses otherwise.
