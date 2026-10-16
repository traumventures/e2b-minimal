# e2b-minimal

E2B sandboxes in **one container with one API key**. It runs the E2B
orchestrator (Firecracker microVMs, built from E2B's public source with one
small patch) and puts a small API in front of it. The API speaks the subset of
the E2B REST API the official SDKs use, so `e2b` for JavaScript and Python
work unchanged.

No Redis, ClickHouse, dashboard, teams or users. Optional pieces:

| Piece | Default | Optional |
| --- | --- | --- |
| API state (templates, builds, sandboxes) | SQLite file in `E2B_DATA_DIR` | Postgres (`DATABASE_URL`) |
| Templates and snapshots | `E2B_DATA_DIR/storage` | any S3-compatible bucket (`S3_BUCKET`) |
| Sandbox disks, snapshot cache | `E2B_DATA_DIR` on the host (own disk or mount if you like) | |

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
- cgroup v2

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
| `PUBLIC_URL` | from the request | How clients reach the API; used for local template upload URLs |
| `SANDBOX_DOMAIN` | unset | Domain returned for `getHost()` URLs |
| `DATABASE_URL` | unset | Postgres for the API state; unset uses SQLite in `E2B_DATA_DIR/minimal/state.db` (an old `state.json` is imported once) |
| `S3_BUCKET` | unset | Bucket for templates and snapshots; unset keeps them in `E2B_DATA_DIR/storage` |
| `S3_BUILD_CACHE_BUCKET` | unset | Bucket for the template build cache (layers, uploaded files) |
| `S3_ENDPOINT`, `S3_REGION`, `S3_FORCE_PATH_STYLE` | AWS, unset, `true` | For S3-compatible stores (MinIO, SeaweedFS, R2, Hetzner, …) |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION` | unset | S3 credentials |
| `E2B_DATA_DIR` | `/var/lib/e2b` | Host directory for everything else |
| `MEMORY_MODE` | `regular` | `regular` or `hugepages`, see below |
| `SANDBOX_ALL_MEMORY_MAX` | unset (no limit) | MiB for all sandboxes together |
| `SANDBOX_MEMORY_MIN` | `256` | MiB guaranteed to every running sandbox (`regular` only) |
| `SANDBOX_ALL_CPU_MAX` | unset (no limit) | Cores for all sandboxes together |
| `DEFAULT_CPU_COUNT`, `DEFAULT_MEMORY_MB` | all cores, `4096` | Size of `base` and of builds that do not ask: the most one sandbox can use |
| `COLD_BOOT` | `false` | Boot new sandboxes instead of restoring the template's memory snapshot |
| `HUGEPAGES` | `2048` | 2 MiB pages reserved on start (`hugepages` only) |
| `MAX_SANDBOX_HOURS` | `24` | Upper bound for timeouts |
| `BUILD_DISK_MB`, `DEFAULT_FREE_DISK_MB` | `10240`, `1024` | Build working space and free space left in a template |
| `BASE_IMAGE`, `BOOTSTRAP_BASE` | `e2bdev/base`, `true` | The `base` template built on first start |
| `PF_MIN_FREE_GIB` | `20` | Disk check in preflight |
| `SKIP_HOST_SETUP` | `0` | `1` skips preflight, host setup and downloads |

Only one API may use a Postgres database or a `state.db`; a second one refuses to start.

## Memory and CPU

A sandbox's size (vCPUs, RAM) comes from its template. Templates that do not
ask for a size get all host cores and `DEFAULT_MEMORY_MB`, so that is the most
one sandbox can use, not what it takes.

**`MEMORY_MODE=regular` (default).** Sandboxes take host memory as they touch
it, and memory the guest frees goes back to the host within seconds (free
page reporting). An idle sandbox takes about 30 MiB. Limits and guarantees:

- `SANDBOX_ALL_MEMORY_MAX` caps all sandboxes together (`memory.max` on the
  `e2b` cgroup), so the host never runs short.
- Every running sandbox is guaranteed `SANDBOX_MEMORY_MIN` (`memory.min`).
  A create or resume that could not be guaranteed it, because the guarantees
  would exceed the limit or that much is not free right now, is refused with
  `503`; the SDKs retry.
- Above its guarantee a sandbox uses what is free. When all sandboxes
  together get within 5 % of the limit, the API kills the one using the most
  memory. Without that, the kernel OOM killer would kill sandboxes that did
  nothing: the guest memory of a killed Firecracker is only freed when the
  orchestrator ends the sandbox.

Fresh memory is slow in a sandbox restored from the template's memory
snapshot: it comes in at about 50–70 MB/s the first time, and again after the
host took it back. `COLD_BOOT=true` boots new sandboxes from the template's
disk instead (about 1.5 s to start instead of 0.2 s); memory then comes at
full speed (about 700 MB/s on first touch). Resuming a paused sandbox always
restores its memory.

**`MEMORY_MODE=hugepages`.** E2B's default: VM memory comes from
`HUGEPAGES` 2 MiB pages reserved at start. The pool is a hard limit; a sandbox
that cannot get pages fails to start. The pool needs free, unfragmented memory
on the host (best right after boot). Templates built in one mode only run in
that mode.

**CPU.** `SANDBOX_ALL_CPU_MAX` caps all sandboxes together (`cpu.max`); within
that, each sandbox uses up to its vCPUs.

## Security

- The API serves plain HTTP. Keep it on a private network or put TLS in front.
- All API routes require the key. Sandbox traffic does not: envd checks each
  sandbox's own access token, other ports are open unless the sandbox was
  created with `network.allowPublicTraffic: false`.
- The container is privileged and runs the orchestrator in the host's
  namespaces. Treat the host as dedicated to it.

## Data and teardown

Under `E2B_DATA_DIR` (default `/var/lib/e2b`):

- `storage/`: templates, snapshots, build cache, unless they are on S3
- `orchestrator/`: local cache of templates and snapshots in use
- `fc-vm/`: running sandboxes
- `minimal/state.db`: templates, builds, sandboxes (SQLite), unless in Postgres

Fixed host paths: `/var/lib/e2b/bin` (orchestrator) and `/fc-*` (envd,
kernel, Firecracker, BusyBox).

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

Verified on a real install (Debian 13, kernel 6.12, `e2b` 2.56.0 for JS and
Python): base template build, `Template.build` with a `COPY` upload,
commands, files, PTY, a sandbox port via `getHost()`, pause and resume with
memory kept, timeout kill and auto-pause, and a container restart (paused
sandboxes resume, lost running ones are dropped).

Also verified there: the SDK suites with state in Postgres and templates,
snapshots and build cache on S3 (SeaweedFS), a paused sandbox surviving a
restart on that setup, a second API refused on the same database;
`MEMORY_MODE=regular` with a 4 GiB template (idle sandbox ~30 MiB, 1.9 GiB
touched and freed back to ~0.5 GiB within 10 s), admission (a sixth sandbox
refused at `SANDBOX_ALL_MEMORY_MAX=6144`, `SANDBOX_MEMORY_MIN=1024`), an
overload by three sandboxes taking 3 GiB each (two of them killed by the API,
the other three alive, no kernel OOM kill), and `SANDBOX_ALL_CPU_MAX=4` holding a
12-vCPU sandbox at 4.0 cores.

`.github/workflows/e2e.yml` runs both: the fake suite, and the real stack on
the runner's KVM.

## Upgrading E2B

See [UPSTREAM.md](UPSTREAM.md).
