# e2b-minimal

E2B sandboxes in **one container with one API key**. It runs the released E2B
orchestrator (Firecracker microVMs, the same binary E2B Embed uses) and puts a
small API in front of it. The API speaks the subset of the E2B REST API the
official SDKs use, so `e2b` for JavaScript and Python work unchanged.

No Postgres, Redis, ClickHouse, S3, dashboard, teams or users. State is one
JSON file; templates and snapshots live on the host's disk.

```
 SDK ──HTTP──▶ e2b-minimal API (:3000) ──gRPC──▶ orchestrator (:5008)
                     │                               │
                     └── sandbox traffic ──▶ orchestrator proxy (:5007) ──▶ envd in the VM
```

## Requirements

The orchestrator needs the host, like E2B Embed does:

- Linux on x86_64 or aarch64 with `/dev/kvm`
- Docker with a privileged container in the host's network, PID and cgroup
  namespaces
- at least 20 GiB free on `/` (templates, snapshots, build cache)
- 2 MiB hugepages for VM memory: every running sandbox and every build VM
  takes its memory from them. `HUGEPAGES=2048` reserves 4 GiB; the host must
  be able to give them, which needs free, unfragmented memory (best set right
  after boot)

The container checks all of this on start (`preflight.sh`) and says what to
fix.

## Run

```bash
cp .env.example .env
echo "E2B_API_KEY=e2b_$(openssl rand -hex 20)" >> .env
docker compose up -d --build
docker compose logs -f
```

The first start loads `nbd` and `tun`, reserves hugepages, downloads the
pinned orchestrator, envd, kernel, Firecracker and BusyBox (checksums
verified) and builds the `base` template from `e2bdev/base`. Sandboxes can be
created once the log says `template build ready`.

## Use

Point the SDK at the API for both the control plane and sandbox traffic:

```bash
export E2B_API_URL=http://e2b-host:3000
export E2B_SANDBOX_URL=http://e2b-host:3000
export E2B_API_KEY=<the key from .env>
```

```js
import { Sandbox, Template } from 'e2b'

const sbx = await Sandbox.create()            // the "base" template
const r = await sbx.commands.run('uname -a')
await sbx.files.write('/tmp/a.txt', 'hi')
await sbx.pause()
const again = await Sandbox.connect(sbx.sandboxId)
await again.kill()

await Template.build(Template().fromTemplate('base').runCmd('pip install pandas'), 'py-data')
```

```python
from e2b import Sandbox

with Sandbox.create() as sbx:
    print(sbx.commands.run("echo hi").stdout)
```

`sandbox.getHost(port)` returns `<port>-<sandboxID>.<SANDBOX_DOMAIN>`. Those
names need a wildcard DNS record for `SANDBOX_DOMAIN` pointing at the API;
requests to them are routed to the port in the sandbox.

## What is supported

| Area | Supported | Not supported |
| --- | --- | --- |
| Sandboxes | create, connect, getInfo, list (state and metadata filter), setTimeout, kill, metadata, env vars, `allowInternetAccess`, `network.allowOut/denyOut/allowPublicTraffic` | fork, snapshots API, volumes, secrets, MCP, IAM, network update, metrics (empty), logs |
| Lifecycle | timeout kills, `autoPause` pauses on timeout (with or without memory), pause and resume via `connect` | auto-resume on traffic |
| Inside the sandbox | everything envd does: commands, PTY, files, watchers, git helpers, ports | |
| Templates | `Template.build` (from image, from template, private registries, `COPY` uploads, start and ready commands), list, delete, alias lookup | tags (a `name:tag` builds `name`), public templates |

Unsupported routes answer `501`.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `E2B_API_KEY` | required | The one key, sent by the SDKs as `X-API-Key` |
| `LISTEN_ADDR` | `:3000` | API listen address (host network) |
| `PUBLIC_URL` | from the request | How clients reach the API; used for template upload URLs |
| `SANDBOX_DOMAIN` | unset | Domain returned for `getHost()` URLs |
| `HUGEPAGES` | `2048` | 2 MiB pages reserved on start |
| `MAX_SANDBOX_HOURS` | `24` | Upper bound for timeouts |
| `DEFAULT_CPU_COUNT`, `DEFAULT_MEMORY_MB` | `2`, `512` | Size of `base` and of builds that do not ask |
| `BUILD_DISK_MB`, `DEFAULT_FREE_DISK_MB` | `10240`, `1024` | Build working space and free space left in a template |
| `BASE_IMAGE`, `BOOTSTRAP_BASE` | `e2bdev/base`, `true` | The `base` template built on first start |
| `PF_MIN_FREE_GIB` | `20` | Disk check in preflight |
| `SKIP_HOST_SETUP` | `0` | `1` skips preflight, host setup and downloads |

## Security

- The API serves plain HTTP. Keep it on a private network or put TLS in front.
- All API routes require the key. Sandbox traffic does not: envd checks each
  sandbox's own access token, other ports are open unless the sandbox was
  created with `network.allowPublicTraffic: false`.
- The container is privileged and runs the orchestrator in the host's
  namespaces. Treat the host as dedicated to it.

## Data and teardown

- `/var/lib/e2b/storage`: templates, snapshots, build cache (orchestrator)
- `/var/lib/e2b/minimal/state.json`: templates, builds, sandboxes (API)
- `/var/lib/e2b/bin`, `/fc-*`: downloaded binaries, kernel

`docker compose down` stops every sandbox. Paused sandboxes and templates
stay. To undo the host changes (nbd, hugepages, sysctl and modprobe files):

```bash
docker compose down
docker run --rm --privileged --pid host --network host -v /:/host \
  --entrypoint sh e2b-minimal:local -c \
  'nsenter -t 1 -m -u -i -n -p -- bash -s < /opt/e2b/scripts/host-teardown.sh'
```

## Tests

`test/fake-e2e.sh` runs the official JS and Python SDKs through build, create,
timeout, list, pause, resume and kill against a fake orchestrator, with the
race detector on. It checks the REST contract without KVM. `test/sdk/lifecycle.mjs --exec`
adds commands and files and needs a real install.

## Upgrading E2B

See [UPSTREAM.md](UPSTREAM.md).
