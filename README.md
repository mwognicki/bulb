# bulb

A small Kubernetes `type=LoadBalancer` controller for clusters that don't have a cloud load balancer.

If you run kubeadm on a handful of VPS nodes — each with its own pinned public IP, no floating IPs, no shared L2, no BGP — then `kubectl apply` of a `type=LoadBalancer` Service just sits in `<pending>` forever. bulb is a Klipper-style fix for that case: it puts a tiny L4 proxy on every node's hostPort, opens the matching port in the host firewall, and computes dry-run DNS record targets for operators to publish manually.

> **Status: pre-alpha.** The current tagged baseline is `v0.0.6`.
> Phases 1–4 are complete. The project ships a controller, proxy,
> firewall-agent, DNSRecord dry-run, node-ip-labeler, and a Helm chart.
> It is installable today. Phase 5 (DNS provider publishing) is optional
> and deferred.

## How it works

```
                       ┌──────────────────────────────────────────┐
                       │  bulb controller (in-cluster)            │
                       │   watches Services with type=LoadBalancer│
                       └──────────────────────────────────────────┘
                            │ creates       │ emits         │ emits
                            ▼               ▼               ▼
                    ┌──────────────┐  ┌──────────────┐  ┌──────────────┐
                    │ proxy DS     │  │ LBPort CR    │  │ DNSRecord CR │
                    │ (per Service)│  │              │  │              │
                    └──────────────┘  └──────────────┘  └──────────────┘
                            │               │                  │
                            │               ▼                  ▼
                            │       ┌──────────────┐
                            │       │firewall-agent│
                            │       │  (per node)  │
                            │       │ programs     │
                            │       │ firewalld,   │
                            │       │ iptables, or │
                            │       │ nftables     │
                            │       └──────────────┘
                            ▼
            client ── node:port ── proxy pod ── ClusterIP or Endpoint ── your pods
```

The dataplane (the proxy pods) is independent of the control plane. If the controller crashes, existing Services keep serving traffic — only changes stop being reconciled.

## What it gives you

- A real, working public endpoint for `Service.spec.type: LoadBalancer` on every node, within ~10 seconds of `kubectl apply`.
- Coordinated host firewall rules, so a deleted Service also closes the port.
- DNS dry-run output via `DNSRecord` CRs, so operators can inspect desired A/AAAA records while provider integrations remain intentionally deferred.
- Plays nicely with cert-manager doing **HTTP-01 / TLS-ALPN-01** ACME challenges. Ports 80 and 443 stay yours; bulb won't touch them unless you explicitly hand them over.

## What it deliberately doesn't do

- **No virtual-IP failover.** That's impossible without floating IPs / BGP / shared L2. Future automated failover is DNS-based; today DNS output is dry-run only.
- **No DNS provider publishing yet.** `DNSRecord` objects are the contract boundary for now; provider integrations are intentionally deferred.
- **No L7.** No TLS termination, host routing, or path rewriting. Use ingress-nginx for HTTP; bulb just makes its `:80` and `:443` reachable.
- **No multi-cluster, multi-tenant, or general-purpose ambitions.** Small cluster, single operator, ≈ 2k LoC of Go.

## Requirements

- Kubernetes **1.36** (kubeadm).
- Linux nodes with at least one supported host firewall backend available: **firewalld**, **iptables**, or **nftables**.
- One static public IPv4 (and optionally IPv6) per node.
- A CNI that gives you working ClusterIP semantics (Cilium is what's tested).
- kube-proxy in iptables mode.

## Annotations

Per-Service knobs (all optional):

| Annotation | Purpose |
|---|---|
| `bulb.toturi.tech/external-traffic-policy` | `Local` or `Cluster` (default `Cluster`) |
| `bulb.toturi.tech/nodes` | Node selector restricting which nodes serve the Service |
| `bulb.toturi.tech/dns-name` | FQDN for dry-run `DNSRecord` output |
| `bulb.toturi.tech/proxy-protocol` | `v1` or `v2` — wrap upstream connections in PROXY protocol |
| `bulb.toturi.tech/keep-on-uninstall` | `"true"` leaves the proxy DaemonSet running when the Service is deleted or moved away from bulb ownership; conflict/invalid cleanup still removes stale proxies |
| `bulb.toturi.tech/allow-privileged-port` | `"true"` — required to claim a port `< 1024` |

Example:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-app
  annotations:
    bulb.toturi.tech/dns-name: my-app.example.com
spec:
  type: LoadBalancer
  loadBalancerClass: bulb
  selector:
    app: my-app
  ports:
    - port: 8443
      targetPort: 8443
      protocol: TCP
```

## Installing with Helm

```sh
helm install bulb oci://ghcr.io/mwognicki/charts/bulb \
  --version 0.0.6 \
  --namespace bulb-system \
  --create-namespace \
  --set firewall.backend=firewalld   # or iptables / nftables
```

Key values:

| Value | Default | Purpose |
|---|---|---|
| `firewall.backend` | `firewalld` | Host firewall backend: `firewalld`, `iptables`, or `nftables` |
| `firewall.deniedPorts` | `"22,80,443"` | Ports the agent must never open |
| `firewall.dryRun` | `"false"` | Set `"true"` to audit without mutating the firewall |
| `image.tag` | *(chart appVersion)* | Pin a specific image tag |
| `namespace` | `bulb-system` | Namespace for all bulb workloads |
| `namespaceCreate` | `true` | Set `false` if the namespace is managed externally |
| `nodeIpLabeler.interval` | `60s` | How often the labeler re-reads the default-route interface |

See `deploy/helm/bulb/values.yaml` for the full set of knobs.

## Raw Manifests

The raw manifests in `deploy/manifests/` remain available if you prefer
applying them directly (e.g. for GitOps tooling that manages CRDs
separately). They include one example workload at
[deploy/manifests/examples/echo-service.yaml](deploy/manifests/examples/echo-service.yaml).

Before applying them:

- Make sure the `ghcr.io/mwognicki/bulb:v0.0.5` image exists and is pullable from your cluster.
- Pick the firewall backend that matches your nodes: `firewalld`, `iptables`, or `nftables`.
- Public IPs are discovered automatically by the `node-ip-labeler` DaemonSet and stored as Node annotations (`bulb.toturi.tech/public-ipv4`, `bulb.toturi.tech/public-ipv6`). No manual ConfigMap is needed.

Apply the core manifests in this order:

```sh
kubectl apply -f deploy/manifests/00-namespace.yaml
kubectl apply -f deploy/manifests/05-lbport-crd.yaml
kubectl apply -f deploy/manifests/06-dnsrecord-crd.yaml
kubectl apply -f deploy/manifests/10-rbac.yaml
kubectl apply -f deploy/manifests/28-node-ip-labeler.yaml
kubectl apply -f deploy/manifests/20-controller.yaml
kubectl apply -f deploy/manifests/15-firewall-agent-config.yaml
kubectl apply -f deploy/manifests/25-firewall-agent.yaml
kubectl -n bulb-system rollout status deploy/bulb-controller
kubectl apply -f deploy/manifests/examples/echo-service.yaml
```

Expected reconciled objects:

- The controller creates a per-Service DaemonSet in `bulb-system`.
- The controller also creates one cluster-scoped `LBPort` object per exposed Service port.
- That DaemonSet schedules one proxy pod per eligible node.
- Proxy pods expose HTTP probes on `:8081`: `/healthz` after listeners bind, and `/readyz` after TCP upstreams are reachable. UDP-only Services become ready once listeners bind because there is no generic safe UDP upstream probe.
- The node-ip-labeler annotates Nodes with `bulb.toturi.tech/public-ipv4` and, when available, `bulb.toturi.tech/public-ipv6`.
- The `echo` Service gets `status.loadBalancer.ingress` values from those Node annotations.

Useful inspection commands:

```sh
kubectl -n bulb-system get pods
kubectl -n bulb-system get ds
kubectl -n bulb-system logs deploy/bulb-controller
kubectl get lbport
kubectl -n echo get svc echo -o yaml
```

The firewall-agent desired-state logic is backend-agnostic, and the implemented mutating backends today are:

- `firewalld` via D-Bus
- `iptables` via dedicated `BULB-INPUT` chains in both `iptables` and `ip6tables`
- `nftables` via a dedicated `inet bulb` table

The shipped manifest still defaults to `firewalld`.

Owned rule space by backend:

- `firewalld`: ports listed in the configured zone plus a node-local bulb state file that records which ports bulb added
- `iptables`: a dedicated `BULB-INPUT` chain, jumped from `INPUT`
- `nftables`: a dedicated `inet bulb_firewall_agent` table

To run that agent too:

```sh
kubectl apply -f deploy/manifests/15-firewall-agent-config.yaml
kubectl apply -f deploy/manifests/25-firewall-agent.yaml
kubectl -n bulb-system get ds bulb-firewall-agent
kubectl -n bulb-system logs ds/bulb-firewall-agent
```

Backend-specific rollout variants:

`firewalld`:
```sh
kubectl apply -f deploy/manifests/15-firewall-agent-config.yaml
kubectl apply -f deploy/manifests/25-firewall-agent.yaml
kubectl -n bulb-system logs ds/bulb-firewall-agent
firewall-cmd --zone=public --list-ports
```

`iptables`:
```sh
kubectl apply -f deploy/manifests/16-firewall-agent-config-iptables.yaml
kubectl apply -f deploy/manifests/26-firewall-agent-iptables.yaml
kubectl -n bulb-system logs ds/bulb-firewall-agent
iptables -S BULB-INPUT
ip6tables -S BULB-INPUT
```

`nftables`:
```sh
kubectl apply -f deploy/manifests/17-firewall-agent-config-nftables.yaml
kubectl apply -f deploy/manifests/27-firewall-agent-nftables.yaml
kubectl -n bulb-system logs ds/bulb-firewall-agent
nft list table inet bulb_firewall_agent
```

The firewall-agent ConfigMap controls the backend and policy knobs that are no longer hardcoded in the DaemonSet:

- `backend`: `firewalld`, `iptables`, or `nftables`
- `zone`: zone-style backends such as `firewalld`
- `stateFile`: node-local tracking file path
- `deniedPorts`: comma-separated denylist, for example `22,80,443`
- `dryRun`: when `true`, validate and compute normally but skip firewall mutation

The agent also exports Prometheus metrics on `:9100/metrics`, including reconcile totals, desired port counts, and policy-filtered port counts.

Dry-run mode:

- set `dryRun: "true"` in the selected firewall-agent ConfigMap
- the agent still validates backend prerequisites and stays subject to readiness checks
- reconcile logs show the filtered port set that would be applied
- metrics still reflect desired and filtered ports, and a dedicated dry-run apply counter increments instead of mutating host firewall state

Backend validation behavior:

- the agent validates the configured backend during startup and fails fast if required host capabilities are missing
- readiness also re-checks backend availability, so a running pod goes unready if the backend becomes unusable

Practical validation expectations by backend:

- `firewalld`: the system D-Bus socket must be reachable and the configured zone must be queryable
- `iptables`: both `iptables` and `ip6tables` must be present and their `INPUT` chains must be inspectable
- `nftables`: the `nft` binary must be present and `nft list tables` must succeed

## What's next

Phase 5 — DNS provider publishing — is intentionally deferred. The `DNSRecord`
CRs the controller emits today are the contract boundary: an operator can
inspect them and configure DNS manually. Automated provider integration
(`dns-agent`) will be Phase 5 when it becomes a priority.

## Release Automation

Multi-arch image publishing is automated in
`.github/workflows/release.yml`:

- On tag push matching `v*`, GitHub Actions builds and pushes a
  multi-arch image (`linux/amd64`, `linux/arm64`) to
  `ghcr.io/mwognicki/bulb` with semver tags.
- The same workflow also publishes release binaries and, when a Helm
  chart exists, packages and publishes the chart.

## Roadmap

bulb has been built as incremental deployable slices. Current completeness:

| Phase | Status | Notes |
|---|---|---|
| 1. Klipper-clone MVP | Done | Controller creates per-Service proxy DaemonSets; TCP forwarding works; LoadBalancer ingress is populated from Node annotations rather than the original static ConfigMap design. |
| 2. Firewall agent | Done | `LBPort` CRD and firewall-agent with firewalld, iptables, and nftables backends; dry-run mode, validation, status, events, cleanup, metrics, and controller-side conflict detection (`PortConflict=True` condition). |
| 3. DNS dry-run | Done | Controller emits `DNSRecord` CRs that describe desired records. Provider publishing is intentionally deferred. |
| 4. Polish + packaging | Done | UDP, PROXY protocol, IPv6, `externalTrafficPolicy: Local`, automatic per-node IP discovery, Service conditions/events, proxy health probes, `keep-on-uninstall`, controller/proxy metrics, multi-arch Docker builds, and Helm chart (`deploy/helm/bulb/`). |
| 5. DNS publishing | Optional / Deferred | `dns-agent` + provider integrations. `DNSRecord` CRs are already emitted for manual use; automated publishing is Phase 5 when it becomes a priority. |

## Building

```sh
make build      # → bin/bulb
make test       # race-tested unit tests
make vet        # go vet ./...
make lint       # golangci-lint (must be installed)
```

The whole project is a single Go binary with subcommands:

```sh
bulb controller       # the reconciler (Deployment)
bulb proxy            # L4 forwarder (per-Service DaemonSet)
bulb firewall-agent   # per-node firewall reconciler with pluggable backends
bulb node-ip-labeler  # per-node public IP discovery and Node annotation
bulb dns-agent        # deferred/stub DNS publisher
```

## License

MIT — see [LICENSE](LICENSE).
