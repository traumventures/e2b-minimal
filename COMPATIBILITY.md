# SDK compatibility

Every public method of the official JavaScript SDK (`e2b` 2.56.0) and what it
does against e2b-minimal. The Python SDK has the same surface with
snake_case names and behaves the same.

| | |
| --- | --- |
| ✅ | works, tested on a real install by [`test/sdk/compat.mjs`](test/sdk/compat.mjs) or `lifecycle.mjs` |
| ☑️ | should work, not tested |
| ⚠️ | partly works, see note |
| ❌ | not supported; the API answers `501` unless noted |

Run the whole suite against your install:

```bash
cd test/sdk && npm install
E2B_API_URL=... E2B_SANDBOX_URL=... E2B_API_KEY=... node compat.mjs        # all areas
node compat.mjs sandbox files              # or some of: sandbox commands files pty git templates presets
```

Last run: 52 of 52 checks passed (Debian 13, kernel 6.12, `e2b` 2.56.0), plus
`lifecycle.py` for the Python SDK.

Everything that runs **inside** a sandbox (commands, files, PTY, git) talks
directly to `envd` in the VM, which is E2B's own unmodified binary. Those
methods behave exactly as on E2B Cloud.

## Sandbox lifecycle

| Method | Status | Notes |
| --- | --- | --- |
| `Sandbox.create(template?, opts)` | ✅ | default template `base` |
| `opts.timeoutMs` | ✅ | capped at `MAX_SANDBOX_HOURS` |
| `opts.metadata` | ✅ | |
| `opts.envs` | ✅ | |
| `opts.allowInternetAccess` | ✅ | `false` blocks all outbound traffic |
| `opts.network.allowOut` / `denyOut` | ✅ | as on E2B: `denyOut` takes IPs and CIDRs; domains in `allowOut` need `denyOut: ['0.0.0.0/0']` |
| `opts.network.allowPublicTraffic: false` | ✅ | ports answer `403` without the `e2b-traffic-access-token` header |
| `opts.lifecycle.onTimeout: 'kill'` | ✅ | |
| `opts.lifecycle.onTimeout: 'pause'` | ✅ | with or without memory (`mode`) |
| `opts.lifecycle.autoResume` | ❌ | ignored: a paused sandbox does not wake on traffic, call `connect` |
| `opts.secure` | ✅ | always on: envd refuses requests without the sandbox's token |
| `opts.mcp` | ❌ | ignored |
| `opts.volumeMounts` | ❌ | ignored |
| `opts.iam` | ❌ | ignored |
| `Sandbox.connect(id, opts)` | ✅ | resumes a paused sandbox, extends a running one |
| `connect` with `onResume: 'reboot'` | ⚠️ | resumes with memory restored instead of rebooting |
| `sandbox.getInfo()` / `Sandbox.getInfo(id)` | ✅ | state, metadata, size, start and end |
| `Sandbox.getFullInfo(id)` | ✅ | same data as `getInfo` |
| `sandbox.isRunning()` | ✅ | |
| `sandbox.setTimeout(ms)` / `Sandbox.setTimeout` | ✅ | |
| `sandbox.kill()` / `Sandbox.kill(id)` | ✅ | also removes a paused sandbox's snapshots |
| `sandbox.pause()` / `Sandbox.pause(id)` | ✅ | memory and disk |
| `pause({ keepMemory: false })` / `mode` | ✅ | disk only: files stay, processes do not |
| `sandbox.betaPause()` / `Sandbox.betaPause` | ✅ | |
| `Sandbox.list({ query: { metadata, state } })` | ✅ | everything in one page |
| `sandbox.getHost(port)` | ✅ | needs wildcard DNS for `SANDBOX_DOMAIN` |
| `sandbox.uploadUrl()` / `downloadUrl()` | ⚠️ | work with host routing (`SANDBOX_DOMAIN` + wildcard DNS, `E2B_DOMAIN` in the client). With `E2B_SANDBOX_URL` the SDK builds URLs without the sandbox id, so they cannot be routed |
| `sandbox.getMetrics()` / `Sandbox.getMetrics(id)` | ⚠️ | returns an empty list |
| `sandbox.updateNetwork()` / `Sandbox.updateNetwork` | ❌ | set network rules at create time |
| `sandbox.fork()` / `Sandbox.fork` | ❌ | |
| `sandbox.createSnapshot()` / `listSnapshots` / `deleteSnapshot` | ❌ | use `pause` |
| `sandbox.getMcpUrl()` / `getMcpToken()` | ❌ | |

## Commands (`sandbox.commands`)

| Method | Status | Notes |
| --- | --- | --- |
| `run(cmd, opts)` | ✅ | `cwd`, `envs`, `user`, `timeoutMs`, `onStdout`/`onStderr`, non-zero exit raises `CommandExitError` |
| `run(cmd, { background: true })` | ✅ | returns a `CommandHandle` |
| `list()` | ✅ | |
| `connect(pid)` | ✅ | |
| `kill(pid)` | ✅ | |
| `sendStdin(pid, data)` / `closeStdin` / `supportsStdinClose` | ✅ | |
| `CommandHandle.wait()` / `kill()` / `disconnect()` / `sendStdin()` / `closeStdin()` | ✅ | |
| `CommandHandle.stdout` / `stderr` / `exitCode` / `error` | ✅ | |

## Files (`sandbox.files`)

| Method | Status | Notes |
| --- | --- | --- |
| `write(path, data)` | ✅ | |
| `read(path, { format })` | ✅ | `text`, `bytes`, `blob`, `stream` |
| `writeFiles([...])` | ✅ | |
| `list(path, { depth })` | ✅ | |
| `exists(path)` | ✅ | |
| `getInfo(path)` | ✅ | |
| `makeDir(path)` | ✅ | |
| `rename(from, to)` | ✅ | |
| `remove(path)` | ✅ | |
| `watchDir(path, onEvent, { recursive })` | ✅ | streams through the API proxy |
| large files | ✅ | 50 MB written and read back |

## Terminal (`sandbox.pty`)

| Method | Status | Notes |
| --- | --- | --- |
| `create({ cols, rows, onData })` | ✅ | |
| `kill(pid)` | ✅ | |
| `sendInput(pid, data)` | ✅ | |
| `resize(pid, size)` | ✅ | |
| `connect(pid)` | ✅ | after `disconnect()` |

## Git (`sandbox.git`)

All git helpers run `git` inside the VM through `commands`, so they work as
far as the template has `git` (the `base` template does).

| Method | Status |
| --- | --- |
| `clone`, `init`, `status`, `branches`, `createBranch`, `checkoutBranch`, `deleteBranch` | ✅ |
| `add`, `commit`, `reset`, `restore`, `pull`, `push` | ✅ |
| `remoteAdd`, `remoteGet`, `getConfig`, `setConfig`, `configureUser`, `dangerouslyAuthenticate` | ✅ |

## Templates (`Template`)

| Method | Status | Notes |
| --- | --- | --- |
| `Template.build(tpl, name, opts)` | ✅ | `cpuCount` and `memoryMB` optional, server defaults apply |
| `Template.buildInBackground` | ✅ | |
| `Template.getBuildStatus` | ✅ | with build logs |
| `Template.exists(name)` / `aliasExists` | ✅ | |
| `Template.toDockerfile` / `toJSON` | ✅ | local, no server call |
| `Template.getTags` / `assignTags` / `removeTags` | ❌ | tags are not kept; `name:tag` builds `name` |
| `fromTemplate(name)` | ✅ | |
| `fromImage(image, credentials?)` / `fromBaseImage()` | ✅ | `e2bdev/base` builds the `base` template |
| `fromUbuntuImage`, `fromDebianImage`, `fromPythonImage`, `fromNodeImage`, `fromBunImage`, `fromAlpineImage`, `fromArchImage`, `fromFedoraImage` | ✅ | all eight built and started |
| `fromDockerfile(content)` | ✅ | `ENV` applies during the build only, as on E2B |
| `fromImage(image, { username, password })` | ✅ | private registry; wrong credentials fail the build |
| `fromAWSRegistry` / `fromGCPRegistry` | ☑️ | passed to the builder, not tested |
| `runCmd`, `setEnvs`, `setUser`, `setWorkdir` | ✅ | |
| `aptInstall`, `pipInstall`, `npmInstall`, `bunInstall` | ✅ | |
| `copy` / `copyItems` | ✅ | files are uploaded through the API; owned by root unless `user` is given |
| `makeDir`, `makeSymlink`, `remove`, `rename`, `gitClone` | ✅ | |
| `setStartCmd`, `setReadyCmd` | ✅ | the start command runs in every new sandbox |
| `skipCache` | ✅ | |
| `addMcpServer` | ❌ | needs E2B's `mcp-gateway` template |
| `betaDevContainerPrebuild` / `betaSetDevContainerStart` | ⚠️ | needs a `devcontainer` template you build yourself |
| Delete a template (REST `DELETE /templates/{id}`) | ✅ | refused while sandboxes use it |
| List templates (REST `GET /templates`) | ✅ | |

## Other SDK classes

| Class | Status |
| --- | --- |
| `Volume` | ❌ |
| `Secret` | ❌ |

## Beyond the SDK

| Feature | Status | Notes |
| --- | --- | --- |
| One API key (`X-API-Key` or `Authorization: Bearer`) | ✅ | |
| Teams, users, per-key permissions | ❌ | by design |
| Dashboard | ❌ | by design |
| Sandbox and build logs API | ⚠️ | build logs yes, sandbox logs no |
| Pause/resume survives an API restart | ✅ | |
| Running sandboxes survive an API restart | ⚠️ | a container restart stops them; they are dropped |
| Several hosts | ❌ | one orchestrator per API |
