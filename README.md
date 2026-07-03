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
`/status` for completion, exit code, and the last ~64KB of combined
stdout/stderr.

## Configuration (environment variables)

| Variable          | Default                            | Required |
| ----------------- | ---------------------------------- | -------- |
| `AGENT_TOKEN`     | —                                  | yes      |
| `LISTEN_ADDR`     | `:9090`                            | no       |
| `STARTUP_SCRIPT`  | `/opt/homelab/scripts/startup.sh`  | no       |
| `SHUTDOWN_SCRIPT` | `/opt/homelab/scripts/shutdown.sh` | no       |

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
