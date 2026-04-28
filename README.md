# bulb

A small Kubernetes `type=LoadBalancer` controller for clusters that don't have a cloud load balancer.

If you run kubeadm on a handful of VPS nodes — each with its own pinned public IP, no floating IPs, no shared L2, no BGP — then `kubectl apply` of a `type=LoadBalancer` Service just sits in `<pending>` forever. bulb is a Klipper-style fix for that case: it puts a tiny L4 proxy on every node's hostPort, opens the matching port in firewalld, and (optionally) publishes a DNS record set pointing at the live nodes.

> **Status: pre-alpha.** The repo currently contains the design and a skeleton. Phase 1 (the MVP) is being built. Don't deploy this yet.

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
                            │       ┌──────────────┐   ┌──────────────┐
                            │       │firewall-agent│   │  dns-agent   │
                            │       │  (per node)  │   │ (1 replica)  │
                            │       │ programs     │   │ pushes to    │
                            │       │ firewalld    │   │ Cloudflare   │
                            │       │ via D-Bus    │   │              │
                            │       └──────────────┘   └──────────────┘
                            ▼
            client ── node:port ── proxy pod ── ClusterIP:targetPort ── your pods
```

The dataplane (the proxy pods) is independent of the control plane. If the controller crashes, existing Services keep serving traffic — only changes stop being reconciled.

## What it gives you

- A real, working public endpoint for `Service.spec.type: LoadBalancer` on every node, within ~10 seconds of `kubectl apply`.
- Coordinated firewalld rules, so a deleted Service also closes the port.
- Optional DNS publishing (Cloudflare to start) with active TCP health checks — a dead node drops out of the record set within ~30 seconds, so DNS-based clients fail over without you doing anything.
- Plays nicely with cert-manager doing **HTTP-01 / TLS-ALPN-01** ACME challenges. Ports 80 and 443 stay yours; bulb won't touch them unless you explicitly hand them over.

## What it deliberately doesn't do

- **No virtual-IP failover.** That's impossible without floating IPs / BGP / shared L2. Failover is DNS-based and takes ≈ TTL + a few seconds.
- **No L7.** No TLS termination, host routing, or path rewriting. Use ingress-nginx for HTTP; bulb just makes its `:80` and `:443` reachable.
- **No multi-cluster, multi-tenant, or general-purpose ambitions.** Small cluster, single operator, ≈ 2k LoC of Go.

## Requirements

- Kubernetes **1.36** (kubeadm).
- A RHEL-family host OS with **firewalld + nftables + systemd**.
- One static public IPv4 (and optionally IPv6) per node.
- A CNI that gives you working ClusterIP semantics (Cilium is what's tested).
- kube-proxy in iptables mode.

## Annotations

Per-Service knobs (all optional):

| Annotation | Purpose |
|---|---|
| `bulb.toturi.tech/external-traffic-policy` | `Local` or `Cluster` (default `Cluster`) |
| `bulb.toturi.tech/nodes` | Node selector restricting which nodes serve the Service |
| `bulb.toturi.tech/dns-name` | FQDN to publish via `dns-agent` |
| `bulb.toturi.tech/proxy-protocol` | `v1` or `v2` — wrap upstream connections in PROXY protocol |
| `bulb.toturi.tech/keep-on-uninstall` | `"true"` — leave the proxy DaemonSet running if bulb is uninstalled |
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

## Roadmap

bulb is shipped in phases. Each phase is a real, deployable subset; the next phase doesn't start until the previous one is in production.

1. **Klipper-clone (MVP)** — controller + per-Service TCP proxy DaemonSets. Ports must already be open in firewalld.
2. **Firewall agent** — `LBPort` CRD + per-node agent programming firewalld via D-Bus.
3. **Health & DNS** — `DNSRecord` CRD + Cloudflare provider + per-target TCP health checks.
4. **Polish** — UDP, PROXY protocol, IPv6, `externalTrafficPolicy: Local`, automatic per-node IP discovery, multi-arch images.

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
bulb firewall-agent   # firewalld reconciler (per-node DaemonSet)
bulb dns-agent        # DNS publisher (Deployment)
```

## License

MIT — see [LICENSE](LICENSE).
