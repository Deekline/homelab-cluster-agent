# homelab-cluster-agent

Small always-on HTTP service that runs the homelab's cluster `startup.sh` /
`shutdown.sh` scripts on request, so they can be triggered remotely (e.g.
from Home Assistant) from a host that stays up when the k3s cluster itself
is powered down.

If `WOL_MAC` is set, `POST /hooks/startup` first sends a Wake-on-LAN magic
packet to that address (e.g. the Proxmox host's MAC — the k3s nodes are VMs
on it with `on_boot = true`, so waking the hypervisor brings them all back)
before running `STARTUP_SCRIPT`. WOL is a no-op if `WOL_MAC` is unset.

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
`/status` for completion, exit code, and the last ~64KB of combined
stdout/stderr.

## Configuration (environment variables)

| Variable             | Default                            | Required |
| -------------------- | ----------------------------------- | -------- |
| `AGENT_TOKEN`        | —                                  | yes      |
| `LISTEN_ADDR`        | `:9090`                            | no       |
| `STARTUP_SCRIPT`     | `/opt/homelab/scripts/startup.sh`  | no       |
| `SHUTDOWN_SCRIPT`    | `/opt/homelab/scripts/shutdown.sh` | no       |
| `WOL_MAC`            | —                                  | no       |
| `WOL_BROADCAST_ADDR` | `255.255.255.255:9`                | no       |

`WOL_MAC` is the target's MAC address (e.g. `aa:bb:cc:dd:ee:ff`). Leave it
unset to disable WOL. `WOL_BROADCAST_ADDR` is the `host:port` the magic
packet is sent to — usually the LAN broadcast address on port 7 or 9; if the
agent isn't on the same L2 segment as the target, point it at that subnet's
directed broadcast address instead (e.g. `10.0.10.255:9`) and make sure your
router/switch permits it.

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
sudo install -m 600 deploy/homelab-cluster-agent.env.example /etc/homelab-cluster-agent.env
sudo $EDITOR /etc/homelab-cluster-agent.env   # set AGENT_TOKEN, script paths
sudo install -m 644 deploy/homelab-cluster-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now homelab-cluster-agent
```

The scripts it invokes must be present, executable, and have whatever they
need (kubeconfig, SSH keys, `zsh`) available to the user the service runs
as. That host wiring is a separate deployment step, not covered here.
