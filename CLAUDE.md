# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project status

Pre-alpha Go project. The repository has working controller, proxy,
firewall-agent, DNSRecord dry-run, and node-ip-labeler code through most
of Phase 4. The latest tagged baseline is `v0.0.5`, which includes
node annotation based public IP discovery, IPv6, UDP forwarding, PROXY
protocol, and `externalTrafficPolicy: Local` endpoint routing.

Before adding Helm or optional DNS publishing, tighten the operational
contract described below: conflict handling, status conditions/events,
proxy health checks, metrics, and documentation must match the code.

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

Four loosely-coupled in-cluster components. **Dataplane is independent of control plane** — if the controller dies, existing Services keep serving traffic.

```
Controller (Deployment, 1 replica + leader election)
  - watches Services with type=LoadBalancer
  - reconciles per-Service DaemonSets
  - writes Service.status.loadBalancer.ingress
  - emits LBPort CRs (firewall) and DNSRecord CRs (DNS dry-run)
        │
        ├── Per-Service DS: proxy pod per node, hostPort=svc port, forwards to ClusterIP:port
        │   or ready Endpoint IPs when externalTrafficPolicy=Local
        ├── firewall-agent (DS): on every node, programs firewalld/iptables/nftables, reconciles LBPort CRs
        └── node-ip-labeler (DS): annotates Nodes with public IPv4/IPv6
```

Single binary, multiple subcommands: `bulb controller`, `bulb proxy`,
`bulb firewall-agent`, `bulb node-ip-labeler`. `bulb dns-agent` is a
stub and remains deferred to Phase 5. Simpler image story.

All coordination is through the Kubernetes API. **No component listens on the tailnet or public NIC for control traffic.**

## Repo layout (target)

```
cmd/bulb/main.go              # subcommand router
internal/controller/          # Service reconciler
internal/proxy/               # TCP/UDP L4 forwarder
internal/firewall/            # per-node firewall-agent
internal/labeler/             # node public IP labeler
deploy/manifests/             # CRDs, RBAC, controller Deployment
deploy/helm/bulb/             # Helm chart (later)
docs/design.md                # living design doc
```

Namespace for all bulb workloads: `bulb-system`. Annotation/label prefix: `bulb.toturi.tech/`. LoadBalancerClass: `bulb`. Per-Service DaemonSet name: `bulb-<svc-namespace>-<svc-name>`.

## Phased delivery

Don't build phase N+1 until N is in production.

- **Phase 1 — Klipper-clone (MVP). Done.** Controller + per-Service proxy DaemonSets. TCP forwarding works and `loadBalancer.ingress` is now populated from Node annotations rather than the original static ConfigMap design.
- **Phase 2 — Firewall agent. Mostly done.** `LBPort` CRD + `firewall-agent` DaemonSet. The current agent supports firewalld, iptables, and nftables backends, dry-run mode, policy filtering, status updates, status-writer tests, stale applied-node cleanup, firewall events, and firewall-agent metrics. Remaining work is mostly conflict/condition polish.
- **Phase 3 — DNS dry-run. Done, provider publishing deferred.** Controller computes and surfaces the desired DNS configuration per Service using `DNSRecord` CRs — but **no dns-agent, no health checks, no provider integration yet**. Rationale: target clusters are small, static VPS nodes; IP churn is rare. The operator can use the output to configure DNS manually until automated publishing is justified.
- **Phase 4 — Polish. Mostly done.** UDP, PROXY protocol, IPv6, `externalTrafficPolicy: Local`, automatic per-node IP discovery, and multi-arch-capable Docker builds are present. Release automation and contract tightening remain.
  - Per-node IP discovery is done: `node-ip-labeler` DaemonSet discovers public IPs from the default-route interface and annotates Nodes with `bulb.toturi.tech/public-ipv4` and `bulb.toturi.tech/public-ipv6`. The controller reads node annotations instead of the static ConfigMap. The static `node-ips` ConfigMap is deprecated.
- **Phase 5 — DNS publishing (optional, deferred).** Provider integrations and active DNS target health checks are intentionally out of the current contract-tightening scope.
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
  - `bulb.toturi.tech/keep-on-uninstall: "true"` (planned; currently documented but not implemented)
  - `bulb.toturi.tech/allow-privileged-port: "true"` (required to open ports < 1024)
- On Service delete or type change, GC the DaemonSet, LBPort CRs, DNSRecord CRs.

### Contract-tightening backlog before Helm
These items should be handled before introducing `deploy/helm/bulb/`,
because Helm should package a stable operator contract rather than
freeze the current rough edges:

1. **Refresh docs continuously.** Keep this file and README aligned with
   shipped behavior. Remove stale Phase 1/static ConfigMap instructions
   whenever they reappear.
2. **Service and LBPort conflict handling.** Enforce same-hostPort
   conflicts between Services and set `PortConflict=True` on the owning
   Service. Detect existing `LBPort.spec.owner` conflicts instead of
   blindly overwriting objects with colliding names.
3. **Status conditions and events.** Add first-class Service conditions
   for conflict, invalid annotations, Local policy with no ready
   endpoints, and successful reconciliation. Emit `LoadBalancerReconciled`
   in addition to existing firewall events.
4. **Proxy health contract.** Replace the current basic TCP socket
   readiness probe with an explicit proxy health/readiness surface:
   liveness should prove listeners are alive, and readiness should
   reflect upstream reachability where possible. Define acceptable
   behavior for UDP-only Services.
5. **Metrics.** Add custom controller/proxy metrics beyond the default
   controller-runtime metrics: reconcile counts/errors/latency,
   per-Service active connections, bytes, and upstream dial errors.
6. **Annotation truthfulness.** Either implement
   `bulb.toturi.tech/keep-on-uninstall` or remove it from the public
   contract. Today it is documented but not wired into behavior.
7. **Endpoints API decision.** The Local policy implementation currently
   uses core `Endpoints`; either move to EndpointSlices or update the
   security/RBAC documentation to bless `Endpoints` for this small-cluster
   design.
8. **Release path.** Document and automate multi-arch image publishing
   before Helm references versioned images as an install path.

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
`dns-agent` Deployment + provider integration. Active TCP health checks per node:port. Failed nodes withdrawn from DNS. Provider credentials in Secret in `bulb-system`; never logged. Not in scope until Phase 5, and ignored for the current contract-tightening pass.

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
- Structured JSON logs via `slog`. One event per significant action; no INFO spam. dns-agent (Phase 5) should use a custom slog handler that redacts known secret keys.
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

- Least-privilege RBAC. Controller: services, endpoints, nodes, own CRDs, DaemonSets in `bulb-system` only. EndpointSlices are still a possible future refinement, but the current Local policy implementation reads core `Endpoints`. Proxy: no API access, no hostNetwork, drop all caps. firewall-agent: hostNetwork + privileged required (firewalld D-Bus); reads CRs only. dns-agent (Phase 5): no host access; reads CRs + Secret; egress to provider API only.
- No `panic` outside of `init`. Errors wrapped with context.
- Image: distroless (`gcr.io/distroless/static`). amd64 minimum, arm64 nice-to-have.

## CRDs

All `v1alpha1` until 1.0, cluster-scoped, status subresource enabled, kubectl printer columns set.

| Kind | Owner | Purpose |
|---|---|---|
| `LBPort` | controller | Tells firewall-agents which ports to open on which nodes |
| `DNSRecord` | controller | Dry-run DNS output (Phase 3); may be consumed by dns-agent in Phase 5 |
| `LBProvider` (future) | operator | Provider config for optional Phase 5 DNS publishing |

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
- **DNS provider token leak via logs.** Token only used in dns-agent (Phase 5); redacting slog handler; integration tests assert no token in stderr.
- **Node IP changes (provider migration / VM rebuild).** `node-ip-labeler` re-runs on boot; controller reconciles status. Document in runbook.
- **Cilium upgrade breaks ClusterIP semantics.** We rely only on stable kube-proxy / Service CIDR semantics; no Cilium APIs.
