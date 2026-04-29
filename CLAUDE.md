# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project status

Greenfield Go project. Only LICENSE, README, and this file exist — no code yet. Phase 1 is the next thing to build (see "Phased delivery" below). When scaffolding, follow the layout in "Repo layout" rather than inventing one.

## What bulb is

A Klipper-style `type=LoadBalancer` controller for a kubeadm cluster that has **no cloud LB, no shared L2, no BGP, and pinned per-node public IPs** (one static IPv4/IPv6 per node, not reassignable). bulb makes a `type=LoadBalancer` Service publicly reachable on every node's public IP, on the requested port, within seconds.

Hard constraint: total Go LoC budget ≈ 2k. Resist scaffolding everything at once — each phase teaches you what the next phase actually needs.

## Environment invariants (do not try to change these)

- A small set of commodity VPS nodes (no cloud-LB, no floating IPs, no shared L2 between nodes). Node count is not load-bearing — don't optimize for or assume any specific number.
- OS: a current RHEL-family distro — assume "RHEL-like with firewalld + nftables + systemd". Don't hardcode a specific distro or version.
- **Kubernetes: kubeadm v1.36 — pin to this.** APIs and feature gates may be 1.36-specific; bumping is an deliberate project decision, not an automatic upgrade.
- One static IPv4 (and IPv6) pinned per node; not reassignable.
- Inter-node: Tailscale tailnet (100.64.0.0/10). Public NICs reachable but not used for control plane.
- CNI: Cilium (VXLAN over tailnet). PodCIDR `10.244.0.0/16`, ServiceCIDR `10.96.0.0/16`.
- Host firewall: firewalld (nftables backend). Public zone on default-route NIC; trusted zone on `tailscale0` and pod/service CIDRs.
- Open public ports today: 22 (optional), 80, 443.
- kube-proxy in iptables mode.

## Architecture

Three loosely-coupled in-cluster components. **Dataplane is independent of control plane** — if the controller dies, existing Services keep serving traffic.

```
Controller (Deployment, 1 replica + leader election)
  - watches Services with type=LoadBalancer
  - reconciles per-Service DaemonSets
  - writes Service.status.loadBalancer.ingress
  - emits LBPort CRs (firewall) and DNSRecord CRs (DNS dry-run)
        │
        ├── Per-Service DS: proxy pod per node, hostPort=svc port, forwards to ClusterIP:port
        └── firewall-agent (DS): on every node, programs firewalld via D-Bus, reconciles LBPort CRs
```

Single binary, multiple subcommands: `bulb controller`, `bulb proxy`, `bulb firewall-agent`. `bulb dns-agent` deferred to Phase 5. Simpler image story.

All coordination is through the Kubernetes API. **No component listens on the tailnet or public NIC for control traffic.**

## Repo layout (target)

```
cmd/bulb/main.go              # subcommand router
internal/controller/          # Service reconciler
internal/proxy/               # TCP forwarder (Phase 1 only)
deploy/manifests/             # CRDs, RBAC, controller Deployment
deploy/helm/bulb/             # Helm chart (later)
docs/design.md                # living design doc
```

Namespace for all bulb workloads: `bulb-system`. Annotation/label prefix: `bulb.toturi.tech/`. LoadBalancerClass: `bulb`. Per-Service DaemonSet name: `bulb-<svc-namespace>-<svc-name>`.

## Phased delivery

Don't build phase N+1 until N is in production.

- **Phase 1 — Klipper-clone (MVP).** Controller + per-Service proxy DaemonSets. TCP only, no PROXY protocol. `loadBalancer.ingress` from a static node→public-IP ConfigMap. No firewall coordination, no DNS. Acceptance: `kubectl apply -f svc-type-lb.yaml` → working public endpoint within 10s on a port the operator pre-opened.
- **Phase 2 — Firewall agent.** `LBPort` CRD + `firewall-agent` DaemonSet. Allowlist/denylist of managed ports. Acceptance: Service create → port in `firewall-cmd --zone=public --list-ports` on every node within 5s; Service delete → port disappears.
  - **Phase 2 closure items** (accepted scope extensions beyond the minimal acceptance target):
    1. Unit tests for `LBPort.status.appliedNodes` writers in the firewall-agent (`updateLBPortStatuses`, `ensureNodeInStatus`, `ensureNodeNotInStatus`): add/remove/idempotency/conflict-retry against a fake client.
    2. Stale node cleanup: the controller already has `lbports/status` RBAC — on each reconcile, diff `appliedNodes` against current cluster nodes and prune entries belonging to nodes that no longer exist or are unschedulable. Prevents orphaned entries when a node is removed while its agent is down.
    3. `FirewallPortOpened` / `FirewallPortClosed` events on the owning Service: the controller should watch LBPort status changes and emit a Kubernetes Event when `appliedNodes` converges to match `spec.nodes` (all agents applied) or when it stalls (partial apply after a timeout). This completes the observability requirement at CLAUDE.md:115.
- **Phase 3 — DNS dry-run.** Controller computes and surfaces the desired DNS configuration per Service (which node IPs should be in the A record, based on `bulb.toturi.tech/dns-name` annotation). Output via structured log, Service status/conditions, and/or a dry-run `DNSRecord` CR — but **no dns-agent, no health checks, no provider integration yet**. Rationale: target clusters are small, static VPS nodes; IP churn is rare. The operator can use the output to configure DNS manually until automated publishing is justified.
- **Phase 4 — Polish.** UDP, PROXY protocol, IPv6, `externalTrafficPolicy: Local`, multi-arch images.
  - Per-node IP discovery is done: `node-ip-labeler` DaemonSet discovers public IPs from the default-route interface and annotates Nodes with `bulb.toturi.tech/public-ipv4` and `bulb.toturi.tech/public-ipv6`. The controller reads node annotations instead of the static ConfigMap. The static `node-ips` ConfigMap is deprecated.
- **Phase 5 — DNS publishing (optional).** `dns-agent` Deployment + Cloudflare provider. Active TCP health checks per node:port. Failed nodes withdrawn from DNS. Acceptance: kill one node's proxy pod → its IP removed from DNS A record set within 30s.
- **Phase 6 (far future, optional).** Replace userspace TCP splice with SO_REUSEPORT + eBPF sockmap or nftables DNAT + conntrack. More DNS providers. kubectl plugin.

## Functional requirements

### Service reconciliation
- Watch Services cluster-wide; act when `spec.type == LoadBalancer` and `spec.loadBalancerClass` is empty or `bulb`.
- Per Service, create/update DaemonSet `bulb-<ns>-<name>` in `bulb-system`. Single-container pod runs the proxy. `hostPort = spec.ports[*].port`. Args: `--upstream <ClusterIP>:<targetPort>` per port. `dnsPolicy: ClusterFirstWithHostNet`, `hostNetwork: false` (hostPort is enough; keep hostNetwork off to avoid ambient capability).
- Populate `Service.status.loadBalancer.ingress` from node annotation `bulb.toturi.tech/public-ipv4` (and v6 if present).
- Per-Service annotations:
  - `bulb.toturi.tech/external-traffic-policy: Local|Cluster` (default `Cluster`)
  - `bulb.toturi.tech/nodes: <selector>` (default: all schedulable)
  - `bulb.toturi.tech/dns-name: api.example.com` (opt into DNS)
  - `bulb.toturi.tech/proxy-protocol: v1|v2`
  - `bulb.toturi.tech/keep-on-uninstall: "true"` (don't GC DS when operator is removed)
  - `bulb.toturi.tech/allow-privileged-port: "true"` (required to open ports < 1024)
- On Service delete or type change, GC the DaemonSet, LBPort CRs, DNSRecord CRs.

### Proxy dataplane
- TCP: accept on `0.0.0.0:<hostPort>`, dial `<ClusterIP>:<targetPort>`, splice both directions, propagate close. Per-connection goroutine; no shared state.
- UDP (Phase 4): per-(client-addr, port) upstream socket, idle timeout default 30s.
- PROXY protocol v1/v2 emission upstream (annotation-driven).
- Liveness: TCP self-test on each listening port. Readiness: ClusterIP reachable.
- SIGTERM: stop accepting, drain up to `terminationGracePeriodSeconds` (default 30s), exit.

### Firewall coordination
`LBPort` CRD (`spec: {port, protocol, nodes: [...]|"*"}`, `status: {appliedNodes}`). Controller emits one per Service port × node-set.

`firewall-agent` watches LBPort, computes `(port, protocol)` set this node should expose in firewalld's **public zone**, reconciles to match. **Uses firewalld D-Bus API directly** (`org.fedoraproject.FirewallD1`), not `firewall-cmd`. Idempotent: full sync on restart, no rule duplication or loss.

Hard rules: only modifies the `public` zone. Default denylist `{22, 80, 443}` (configurable via ConfigMap). Refuses ports < 1024 unless Service has `bulb.toturi.tech/allow-privileged-port: "true"`. Tracks applied rules in a node-local file so it never removes rules it didn't add.

### DNS dry-run (Phase 3, opt-in)
Controller computes the desired DNS A record for a Service when the `bulb.toturi.tech/dns-name` annotation is present. Output channels: structured log line, Service status/conditions, and/or a dry-run `DNSRecord` CR (`spec: {fqdn, type, ttl, targets}`, no provider field). **No agent consumes this yet** — it is informational only, for the operator to act on manually.

### DNS publishing (Phase 5, deferred)
`dns-agent` Deployment + Cloudflare provider (pluggable). Active TCP health checks per node:port. Failed nodes withdrawn from DNS. Provider credentials in Secret in `bulb-system`; never logged. Not in scope until Phase 5.

### TLS / ACME coordination (HTTP-01)
bulb must **cooperate cleanly with cert-manager (or another ACME client) doing HTTP-01 / TLS-ALPN-01 webserver challenges** on ports 80/443. DNS-01 is explicitly *not* the assumed path — operators want to issue certs for arbitrary hostnames pointing at node IPs, including hosts whose DNS isn't managed by `dns-agent`.

Concrete implications:
- 80 and 443 stay in the firewall-agent's default denylist — they are operator-managed and already open. bulb never closes them.
- When a Service requests port 80 or 443 via bulb, the controller treats it as a port-conflict / opt-in case (the operator must explicitly hand those ports to bulb; otherwise ingress-nginx + cert-manager keeps them).
- The proxy must not strip or rewrite the ACME challenge path (`/.well-known/acme-challenge/...`) — it's an L4 splice, so this is automatic, but don't introduce L7 logic that would break it.
- When `dns-agent` (Phase 5) withdraws a node from a record set due to health failure, certificate renewal for that hostname must still succeed via the remaining nodes — the live target set must always be non-empty when *any* node is healthy, never momentarily empty during reconcile.
- DNS records published by `dns-agent` (Phase 5) should respect a short TTL (default 60s) so freshly-issued certs propagate to clients quickly; ACME validation tolerates this.

### Observability
- Prometheus metrics on `:9100/metrics` from every component (reconcile counts/errors/latency; per-Service active conns / bytes / dial errors; firewalld ops applied/failed / rule count; DNS API calls/errors/last sync).
- Structured JSON logs via `slog`. One event per significant action; no INFO spam. dns-agent (Phase 5) uses a custom slog handler that redacts known secret keys.
- Events on the Service object for major transitions (`LoadBalancerReconciled`, `FirewallPortOpened`, `DNSTargetWithdrawn` (Phase 5), …).

## Non-functional targets

- Service-create → traffic-serving: ≤ 10s.
- Failover (dead node → DNS rotated out, Phase 5): ≤ 30s after fail threshold; client-perceived ≤ TTL+10s.
- Proxy throughput: ≥ 1 Gbps per node for plain TCP splice (single core OK).
- Proxy memory per active conn: ≤ 32 KiB steady state.
- Controller cold-start to first reconcile: ≤ 5s.
- Agent restart impact: zero dataplane disruption.
- Crash safety: all state derives from API server + node-local firewalld; no etcd state outside our CRs.

## Security

- Least-privilege RBAC. Controller: services, endpointslices, nodes, own CRDs, DaemonSets in `bulb-system` only. Proxy: no API access, no hostNetwork, drop all caps. firewall-agent: hostNetwork + privileged required (firewalld D-Bus); reads CRs only. dns-agent (Phase 5): no host access; reads CRs + Secret; egress to provider API only.
- No `panic` outside of `init`. Errors wrapped with context.
- Image: distroless (`gcr.io/distroless/static`). amd64 minimum, arm64 nice-to-have.

## CRDs

All `v1alpha1` until 1.0, cluster-scoped, status subresource enabled, kubectl printer columns set.

| Kind | Owner | Purpose |
|---|---|---|
| `LBPort` | controller | Tells firewall-agents which ports to open on which nodes |
| `DNSRecord` | controller | Dry-run DNS output (Phase 3); consumed by dns-agent in Phase 5 |
| `LBProvider` (future) | operator | Provider config (e.g. Cloudflare zone ID + API token Secret ref). v1 may use a ConfigMap. |

## Resolved decisions

- Language: **Go** (controller-runtime ecosystem). Aligns with Klipper-LB.
- Single binary, subcommand-dispatched.
- Leader election: **yes**, even with 1 replica (so rolling updates don't double-reconcile). Use client-go leases.
- Operator uninstall: GC chain via OwnerReferences by default; `bulb.toturi.tech/keep-on-uninstall=true` opts out.
- Two Services on same hostPort: controller refuses second, sets `PortConflict=True` condition.
- LBPort conflict: `LBPort.spec.owner` (controller name); refuse on conflict. No multi-controller support in v1.
- Node public IP discovery: ~~ConfigMap (Phase 1)~~ → node annotations written by `node-ip-labeler` DaemonSet (Phase 4, done).

## Out of scope

True virtual-IP failover (impossible without floating IPs / BGP / shared L2). Sub-second failover. Replacing Cilium's dataplane (we sit above it; pod-to-pod is Cilium's). L7 (TLS, host routing, path rewriting — use ingress-nginx). Multi-cluster, multi-tenant. Auth/mTLS/WAF. Admission webhook (we tolerate misconfig; surface it via Service `status` conditions).

## Known risks (and what we already decided to do)

- **firewall-agent locks operator out of SSH.** Hardcoded 22 in denylist; agent never removes rules it didn't add (tracked in a node-local file).
- **Cloudflare token leak via logs.** Token only used in dns-agent (Phase 5); redacting slog handler; integration tests assert no token in stderr.
- **Node IP changes (provider migration / VM rebuild).** `node-ip-labeler` re-runs on boot; controller reconciles status. Document in runbook.
- **Cilium upgrade breaks ClusterIP semantics.** We rely only on stable kube-proxy / Service CIDR semantics; no Cilium APIs.
