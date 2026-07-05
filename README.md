# homelab-cluster-agent

Small always-on HTTP service that runs the homelab's cluster `startup.sh` /
`shutdown.sh` scripts on request, so they can be triggered remotely (e.g.
from Home Assistant) from a host that stays up when the k3s cluster itself
is powered down.

## Endpoints

All endpoints except `/healthz` require the shared-secret header
`X-Agent-Token: <token>`.

| Method | Path              | Description                              |
| ------ | ----------------- | ---------------------------------------- |
| GET    | `/healthz`        | Liveness check, no auth                  |
| GET    | `/status`         | JSON: current/last job state             |
| POST   | `/hooks/startup`  | Runs `STARTUP_SCRIPT` in the background  |
| POST   | `/hooks/shutdown` | Runs `SHUTDOWN_SCRIPT` in the background |

Only one job may run at a time; a second request while one is in flight
gets `409 Conflict`. Job requests return `202 Accepted` immediately — poll
`/status` for progress and completion. `output_tail` (the last ~64KB of
combined stdout/stderr) updates live as the script runs, not just after it
exits, so a stuck job is visible mid-flight instead of showing an empty
string until it finishes.

The agent itself knows nothing about Wake-on-LAN, Proxmox, or the NAS — it
just execs whatever `STARTUP_SCRIPT`/`SHUTDOWN_SCRIPT` point at and reports
back exit code and output. All of the cluster-specific logic lives in
`scripts/`, described below.

## `scripts/` — the startup pipeline

`STARTUP_SCRIPT` (`scripts/startup.sh`) runs three phases in order:

1. **`wake-hosts.sh`** — sends a Wake-on-LAN magic packet to every host
   listed in `scripts/targets.conf` (currently `pve1`, `pve2`, and the NAS),
   then watches all of them in parallel until each accepts a TCP connection
   on its configured port, or fails loudly if one doesn't come up in time.
   Add or remove a host by editing `targets.conf` — nothing else needs to
   change, including the Go agent.
2. **`wake-control-plane.sh`** — waits for the k3s API server to respond,
   then for the `k3s-cp-1` node to report `Ready`. Only reachable once
   `pve1` (which hosts the `k3s-cp-1` VM) is actually up.
3. **`wake-workers.sh`** — waits for `k3s-worker-1` and `k3s-worker-2` to
   report `Ready`, uncordons both, resumes the ArgoCD auto-sync for
   `cloudnativepg` that `shutdown.sh` paused, and prints final cluster
   status.

Each phase is a standalone script and can be run by hand for
troubleshooting (e.g. `./scripts/wake-hosts.sh` on its own to just wake
everything without touching Kubernetes).

`scripts/lib.sh` holds the shared helpers (`send_magic_packet`,
`wait_for_tcp`, `wait_for_node_ready`) used across the phase scripts.

`scripts/shutdown.sh` is unchanged from `homelab-infra/scripts/shutdown.sh`
— it only drains and powers off the k3s VMs, it doesn't power off `pve1`,
`pve2`, or the NAS. If you want WOL to actually save power (rather than
just being a no-op because the hosts were never off), the physical hosts
need to be shut down too — not currently done here.

### `targets.conf`

```
# name    mac                  broadcast_addr        check_host    check_port
pve1      <fill in>            10.0.10.255:9         10.0.10.30    8006
pve2      <fill in>            10.0.10.255:9         10.0.10.40    8006
nas       <fill in>            10.0.10.255:9         10.0.10.150   443
```

Fill in the real MAC address for each host (`ip link` / `arp -a` /
router DHCP lease list). `broadcast_addr` is the `host:port` the magic
packet is sent to — the LAN broadcast address on port 9 works if the agent
is on the same L2 segment as the targets; otherwise use that subnet's
directed broadcast and make sure your router/switch permits it.
`check_host:check_port` is what `wait_for_tcp` polls to decide the host is
up — Proxmox's web UI port (8006) and the NAS's HTTPS port (443) work well
since both come up early in boot and don't require auth to TCP-connect.

### Dependencies

The scripts assume `bash`, `kubectl` (with a working kubeconfig), `python3`
(`shutdown.sh`'s JSON parsing), `nc`, and `wakeonlan` (`apt install
wakeonlan` — used by `lib.sh` to send the magic packet, rather than
reimplementing that inline) are available to the user the service runs as.

They also need the `sshcp`/`sshw1`/`sshw2` aliases `shutdown.sh` uses,
defined in `~/.bashrc` for the user the service runs as. `startup.sh` and
`shutdown.sh` both `source ~/.bashrc` with `shopt -s expand_aliases` first
— bash disables alias expansion in non-interactive scripts by default, and
the default Debian/Ubuntu `~/.bashrc` template also returns immediately for
non-interactive shells (`case $- in *i*) ;; *) return;; esac`), which would
silently skip the aliases too; make sure that guard isn't present (or the
aliases are defined above it / in a file sourced unconditionally).

## Build

```bash
go build -o homelab-cluster-agent .
```

## Test

```bash
go test ./...
```

## Deploy (systemd)

```bash
go build -o homelab-cluster-agent .
sudo install -m 755 homelab-cluster-agent /usr/local/bin/homelab-cluster-agent
sudo install -d /opt/homelab/scripts
sudo install -m 755 scripts/*.sh /opt/homelab/scripts/
sudo install -m 644 scripts/targets.conf /opt/homelab/scripts/
sudo $EDITOR /opt/homelab/scripts/targets.conf   # fill in real MAC addresses
sudo install -m 600 deploy/homelab-cluster-agent.env.example /etc/homelab-cluster-agent.env
sudo $EDITOR /etc/homelab-cluster-agent.env   # set AGENT_TOKEN, script paths
sudo install -m 644 deploy/homelab-cluster-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now homelab-cluster-agent
```
