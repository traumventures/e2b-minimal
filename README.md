<div align="center">

# e2b-minimal

**Self-hosted [E2B](https://e2b.dev) sandboxes in one container. No accounts, no API keys.**

Firecracker microVMs, driven by the official `e2b` SDKs for JavaScript and Python, unchanged.
No Redis, no ClickHouse, no dashboard, no teams.

[![e2e](https://github.com/traumventures/e2b-minimal/actions/workflows/e2e.yml/badge.svg)](https://github.com/traumventures/e2b-minimal/actions/workflows/e2e.yml)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

</div>

---

```js
import { Sandbox } from 'e2b'

const sbx = await Sandbox.create()
const { stdout } = await sbx.commands.run('uname -a')   // runs in a Firecracker VM on your host
```

## Why

E2B's own self-hosting options run the full platform: Postgres, Redis,
ClickHouse, a log pipeline, an API with teams and keys, a dashboard, a client
proxy, and a dozen setup jobs. If you just want sandboxes for your own
agents and services, that is a lot to operate.

e2b-minimal keeps the part that does the work, E2B's **orchestrator**, and
replaces everything in front of it with a small Go API (~2,400 lines) that
speaks the part of the E2B REST API the SDKs use.

| | E2B Embed (official compose) | e2b-minimal |
| --- | --- | --- |
| Containers | ~20 | **1** |
| Required services | Postgres, Redis, ClickHouse, Vector | **none** |
| Auth | teams, users, API keys | **none** (optional single key) |
| API state | Postgres | **SQLite file**, or Postgres |
| Templates and snapshots | local disk | local disk, or **any S3** |
| VM memory | fixed hugepage pool | **on demand**, global cap, per-sandbox guarantee |
| SDKs | official | official, unchanged |

## Quick start

You need a Linux host with KVM (`/dev/kvm`), Docker, cgroup v2 and 20 GiB of
free disk. The container checks all of this on start and tells you what to fix.

```bash
git clone https://github.com/traumventures/e2b-minimal && cd e2b-minimal
cp .env.example .env
docker compose up -d --build        # first build compiles the orchestrator, a few minutes
docker compose logs -f              # wait for "template build ready"
```

On first start the container loads the `nbd` and `tun` kernel modules,
downloads envd, the guest kernel, Firecracker and BusyBox (checksums
verified) and builds the `base` template from `e2bdev/base`.

Then point the SDK at it. API and sandbox traffic share one address:

```bash
export E2B_API_URL=http://your-host:3000
export E2B_SANDBOX_URL=http://your-host:3000
export E2B_API_KEY=unused     # the SDKs refuse to start without one; the server ignores it
```

There is no authentication by default: anyone who can reach port 3000 can
start sandboxes. Keep it on a private network, or set `E2B_API_KEY` on the
server to require that key (see [Security](#security)).

## Use it

Everything below is the plain, official SDK.

```js
import { Sandbox } from 'e2b'

const sbx = await Sandbox.create({ timeoutMs: 10 * 60_000, metadata: { job: 'report' } })

await sbx.files.write('/home/user/data.csv', 'a,b\n1,2\n')
const r = await sbx.commands.run('python3 -c "import csv; print(list(csv.reader(open(\'data.csv\'))))"', { cwd: '/home/user' })

await sbx.pause()                                  // memory and disk saved
const same = await Sandbox.connect(sbx.sandboxId)  // resumed where it left off
await same.kill()
```

```python
from e2b import Sandbox

with Sandbox.create() as sbx:
    print(sbx.commands.run("echo hello from a microVM").stdout)
```

Commands, PTY terminals, files, watchers, git helpers and ports all work,
since they talk to `envd` inside the VM. `sbx.getHost(8000)` returns
`8000-<sandboxID>.<SANDBOX_DOMAIN>`; point a wildcard DNS record for
`SANDBOX_DOMAIN` at the host to reach sandbox ports.

### Network rules

Same rules as on E2B Cloud:

```js
await Sandbox.create({ allowInternetAccess: false })                          // no outbound traffic
await Sandbox.create({ network: { denyOut: ['1.1.1.1/32'] } })                // block addresses
await Sandbox.create({ network: { allowOut: ['pypi.org'], denyOut: ['0.0.0.0/0'] } })  // only these domains
await Sandbox.create({ network: { allowPublicTraffic: false } })              // ports need the traffic token
```

### Templates

Build your own images with the SDK's template builder:

```js
import { Template } from 'e2b'

const tpl = Template()
  .fromTemplate('base')
  .aptInstall(['ffmpeg'])
  .runCmd('pip install pandas')
  .copy('scripts/', '/home/user/scripts/')

await Template.build(tpl, 'data-tools')
// later: Sandbox.create('data-tools')
```

All template builder steps work, including `fromDockerfile`, the distro
presets (Ubuntu, Debian, Python, Node, Bun, Alpine, Arch, Fedora), private
registries with credentials, `setStartCmd`/`setReadyCmd` and
`buildInBackground`. As on E2B, `ENV`/`setEnvs` apply during the build only,
and `copy` writes files as root unless you pass `{ user }`.

[`templates/tools.mjs`](templates/tools.mjs) is a complete example with uv,
Bun and Typst.

## Memory and CPU

A sandbox's size comes from its template. Templates that do not ask for one
get **all host cores** and `DEFAULT_MEMORY_MB` (4 GiB). That is the most a
sandbox can use, not what it takes:

- **On demand.** A sandbox takes host memory as it touches it, and memory the
  guest frees goes back to the host within seconds. An idle 4 GiB sandbox
  uses about 30 MiB.
- **Global cap.** `SANDBOX_ALL_MEMORY_MAX` and `SANDBOX_ALL_CPU_MAX` limit all
  sandboxes together, so the host stays healthy.
- **Per-sandbox guarantee.** Every running sandbox is guaranteed
  `SANDBOX_MEMORY_MIN`. A new sandbox that could not get it is refused with
  `503` (the SDKs retry) instead of squeezing the others.
- **Overload.** If the sandboxes together still approach the cap, the API
  kills the one using the most memory. The others keep running.

```bash
SANDBOX_ALL_MEMORY_MAX=24576   # 24 GiB for all sandboxes
SANDBOX_ALL_CPU_MAX=8          # 8 cores for all sandboxes
SANDBOX_MEMORY_MIN=512         # each running sandbox is guaranteed 512 MiB
```

> **Fast start or fast memory.** By default a sandbox starts in ~0.2 s by
> restoring the template's memory snapshot, but memory it has never touched
> then arrives at only 50–70 MB/s. With `COLD_BOOT=true` new sandboxes boot
> instead (~1.5 s) and get memory at full speed (~700 MB/s). Pick cold boot
> for memory-heavy work.

`MEMORY_MODE=hugepages` switches to E2B's default: a fixed pool of 2 MiB
pages (`HUGEPAGES`) reserved at start. Templates built in one mode only run
in that mode.

## Storage

| What | Default | Optional |
| --- | --- | --- |
| API state (templates, builds, sandboxes) | SQLite, `E2B_DATA_DIR/minimal/state.db` | Postgres: `DATABASE_URL` |
| Templates, snapshots, build cache | `E2B_DATA_DIR/storage` | any S3-compatible bucket: `S3_BUCKET`, `S3_BUILD_CACHE_BUCKET` |
| Running sandboxes, local cache | `E2B_DATA_DIR` | put it on its own disk or mount |

```bash
DATABASE_URL=postgres://e2b:secret@db:5432/e2b
S3_BUCKET=e2b-templates
S3_BUILD_CACHE_BUCKET=e2b-build-cache
S3_ENDPOINT=https://s3.example.com     # MinIO, SeaweedFS, R2, Hetzner, …
AWS_ACCESS_KEY_ID=...
AWS_SECRET_ACCESS_KEY=...
AWS_REGION=us-east-1
```

One API per database or `state.db`: a second one refuses to start.

## How it works

```
                  ┌──────────────────── e2b-minimal container ─────────────────────┐
 e2b SDK ──HTTP──▶│ API :3000 ──gRPC──▶ orchestrator ──▶ Firecracker microVMs       │
  (X-API-Key)     │    │                     │              (one per sandbox)       │
                  │    └─ sandbox traffic ──▶ proxy :5007 ──▶ envd inside the VM     │
                  └──────────────────────────┬──────────────────────────────────────┘
                                             │
                      SQLite / Postgres      │      local disk / S3
```

- **API** (`internal/server`): sandbox lifecycle, timeouts, pause and resume,
  template builds, the memory guarantees and limits, and routing of sandbox
  traffic (by `E2b-Sandbox-Id` header or `<port>-<id>.<domain>` host).
- **Orchestrator**: E2B's, built from the public
  [e2b-dev/runtime](https://github.com/e2b-dev/runtime) source with a small
  patch for on-demand memory ([details](UPSTREAM.md)). It runs in the host's
  namespaces, like in E2B Embed, so the container is privileged.

## What is supported

Everything inside a sandbox works as on E2B Cloud: commands, PTY, files,
watchers, git helpers and ports go straight to E2B's own `envd` in the VM.
The control plane covers create, connect, list, timeouts, pause and resume
(also on timeout) and `Template.build`.

Not supported: fork, the snapshot API, volumes, secrets, MCP, template tags,
auto-resume on traffic, network updates after create and sandbox metrics.
Those routes answer `501` (metrics: an empty list).

One caveat: `uploadUrl()`/`downloadUrl()` only work with host routing
(`SANDBOX_DOMAIN` plus wildcard DNS, `E2B_DOMAIN` in the client). With
`E2B_SANDBOX_URL` the SDK builds those URLs without the sandbox id.

**Method-by-method checklist, with what was tested: [COMPATIBILITY.md](COMPATIBILITY.md).**

## Configuration

All settings are environment variables, see [`.env.example`](.env.example).

| Variable | Default | |
| --- | --- | --- |
| `E2B_API_KEY` | unset (no check) | Optional key every API request must carry |
| `LISTEN_ADDR` | `:3000` | API address on the host network |
| `PUBLIC_URL` | from request | How clients reach the API (for template upload URLs) |
| `SANDBOX_DOMAIN` | | Domain for `getHost()` URLs |
| `DATABASE_URL` | | Postgres instead of SQLite |
| `S3_BUCKET`, `S3_BUILD_CACHE_BUCKET` | | S3 instead of local disk |
| `S3_ENDPOINT`, `S3_REGION`, `S3_FORCE_PATH_STYLE` | AWS, –, `true` | S3-compatible stores |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION` | | S3 credentials |
| `E2B_DATA_DIR` | `/var/lib/e2b` | Host directory for everything else |
| `SANDBOX_ALL_MEMORY_MAX` | no limit | MiB for all sandboxes together |
| `SANDBOX_ALL_CPU_MAX` | no limit | Cores for all sandboxes together |
| `SANDBOX_MEMORY_MIN` | `256` | MiB guaranteed to each running sandbox |
| `DEFAULT_MEMORY_MB`, `DEFAULT_CPU_COUNT` | `4096`, all cores | Size of templates that do not ask |
| `COLD_BOOT` | `false` | Boot new sandboxes instead of restoring memory |
| `MEMORY_MODE`, `HUGEPAGES` | `regular`, `2048` | `hugepages` for a fixed page pool |
| `MAX_SANDBOX_HOURS` | `24` | Longest allowed timeout |
| `BUILD_DISK_MB`, `DEFAULT_FREE_DISK_MB` | `10240`, `1024` | Build space, free space left in a template |
| `BASE_IMAGE`, `BOOTSTRAP_BASE` | `e2bdev/base`, `true` | The `base` template built on first start |
| `PF_MIN_FREE_GIB` | `20` | Disk check on start |
| `SKIP_HOST_SETUP` | `0` | Skip host checks, setup and downloads |

## Security

- **No authentication by default.** Whoever reaches the API can run code in
  sandboxes. Keep it on a private network (loopback, VPN, Tailscale) or set
  `E2B_API_KEY` on the server; then every API route needs that key as
  `X-API-Key` or `Authorization: Bearer`, and clients set the same value.
- The API speaks plain HTTP. Put TLS in front if it leaves the host.
- Sandbox traffic never needs the API key: envd checks each
  sandbox's own access token, other ports are open unless a sandbox was
  created with `network: { allowPublicTraffic: false }`.
- The container is privileged and runs the orchestrator on the host. Give it
  a dedicated host or VM (with nested virtualization).

## Operations

`docker compose down` stops all sandboxes; paused sandboxes and templates
stay. On restart, paused sandboxes can be resumed and running ones that were
lost are dropped from the list.

To undo the host changes (kernel modules, sysctl and modprobe files):

```bash
docker compose down
docker run --rm --privileged --pid host --network host -v /:/host \
  --entrypoint sh e2b-minimal:local -c \
  'nsenter -t 1 -m -u -i -n -p -- bash -s < /opt/e2b/scripts/host-teardown.sh'
```

## Tests

- `go test -race ./...`: store tests (SQLite persistence, locking, import).
- `test/fake-e2e.sh`: the official JS and Python SDKs against a fake
  orchestrator, no KVM needed.
- `node test/sdk/lifecycle.mjs --exec` and `python test/sdk/lifecycle.py --exec`:
  the main lifecycle against a real install, per SDK.
- `node test/sdk/compat.mjs [area...]`: every feature in
  [COMPATIBILITY.md](COMPATIBILITY.md), one check each, against a real
  install. Areas: `sandbox commands files pty git templates presets`. Set
  `REGISTRY`, `REGISTRY_USER`, `REGISTRY_PASSWORD` to include the private
  registry checks.

CI runs the store tests and the fake suite on every push, and the lifecycle
and compat suites on a GitHub runner with KVM.

Last full run on a real host (Debian 13, kernel 6.12, `e2b` 2.56.0): 52 of 52
compat checks, both lifecycle suites, plus Postgres and S3 (SeaweedFS),
restarts, the memory guarantee and overload handling, and the CPU cap.

## Credits

Built on [E2B](https://github.com/e2b-dev/runtime) (Apache-2.0): the
orchestrator, host scripts and protocol definitions come from there, see
[UPSTREAM.md](UPSTREAM.md). This project is not affiliated with E2B.

Licensed under [Apache-2.0](LICENSE).
