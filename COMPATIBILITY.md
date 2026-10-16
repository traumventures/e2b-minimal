# SDK compatibility

Every public method of the official JavaScript SDK (`e2b` 2.56.0) and what it
does against e2b-minimal. The Python SDK has the same surface with
snake_case names and behaves the same.

| | |
| --- | --- |
| ✅ | works, tested on a real install |
| ☑️ | works, not separately tested (same code path as a tested method, or envd-only) |
| ⚠️ | partly works, see note |
| ❌ | not supported; the API answers `501` unless noted |

Everything that runs **inside** a sandbox (commands, files, PTY, git) talks
directly to `envd` in the VM, which is E2B's own unmodified binary. Those
methods behave exactly as on E2B Cloud.

## Sandbox lifecycle

| Method | Status | Notes |
| --- | --- | --- |
| `Sandbox.create(template?, opts)` | ✅ | default template `base` |
| `opts.timeoutMs` | ✅ | capped at `MAX_SANDBOX_HOURS` |
| `opts.metadata` | ✅ | |
| `opts.envs` | ☑️ | passed to the VM |
| `opts.allowInternetAccess` | ☑️ | |
| `opts.network.allowOut` / `denyOut` | ☑️ | `denyOut` takes IPs and CIDRs only; domains in `allowOut` |
| `opts.network.allowPublicTraffic: false` | ☑️ | ports need the `trafficAccessToken` header |
| `opts.lifecycle.onTimeout: 'kill'` | ✅ | |
| `opts.lifecycle.onTimeout: 'pause'` | ✅ | with or without memory (`mode`) |
| `opts.lifecycle.autoResume` | ❌ | ignored: a paused sandbox does not wake on traffic, call `connect` |
| `opts.secure` | ☑️ | always on: every sandbox gets an envd access token |
| `opts.mcp` | ❌ | ignored |
| `opts.volumeMounts` | ❌ | ignored |
| `opts.iam` | ❌ | ignored |
| `Sandbox.connect(id, opts)` | ✅ | resumes a paused sandbox, extends a running one |
| `connect` with `onResume: 'reboot'` | ⚠️ | resumes with memory restored instead of rebooting |
| `sandbox.getInfo()` / `Sandbox.getInfo(id)` | ✅ | state, metadata, size, start and end |
| `Sandbox.getFullInfo(id)` | ☑️ | same data as `getInfo` |
| `sandbox.isRunning()` | ☑️ | envd health check |
| `sandbox.setTimeout(ms)` / `Sandbox.setTimeout` | ✅ | |
| `sandbox.kill()` / `Sandbox.kill(id)` | ✅ | also removes a paused sandbox's snapshots |
| `sandbox.pause()` / `Sandbox.pause(id)` | ✅ | memory and disk |
| `pause({ keepMemory: false })` / `mode` | ☑️ | disk only, resume reboots |
| `sandbox.betaPause()` / `Sandbox.betaPause` | ☑️ | same endpoint as `pause` |
| `Sandbox.list({ query: { metadata, state } })` | ✅ | metadata filter tested; everything in one page |
| `sandbox.getHost(port)` | ✅ | needs wildcard DNS for `SANDBOX_DOMAIN` |
| `sandbox.uploadUrl()` / `downloadUrl()` | ☑️ | signed envd URLs, served through the API address |
| `sandbox.getMetrics()` / `Sandbox.getMetrics(id)` | ⚠️ | returns an empty list |
| `sandbox.updateNetwork()` / `Sandbox.updateNetwork` | ❌ | set network rules at create time |
| `sandbox.fork()` / `Sandbox.fork` | ❌ | |
| `sandbox.createSnapshot()` / `listSnapshots` / `deleteSnapshot` | ❌ | use `pause` |
| `sandbox.getMcpUrl()` / `getMcpToken()` | ❌ | |

## Commands (`sandbox.commands`)

| Method | Status | Notes |
| --- | --- | --- |
| `run(cmd, opts)` | ✅ | `cwd`, `envs`, `user`, `timeoutMs`, `onStdout`/`onStderr` |
| `run(cmd, { background: true })` | ✅ | returns a `CommandHandle` |
| `list()` | ☑️ | |
| `connect(pid)` | ☑️ | |
| `kill(pid)` | ☑️ | |
| `sendStdin(pid, data)` / `closeStdin` / `supportsStdinClose` | ☑️ | |
| `CommandHandle.wait()` / `kill()` / `disconnect()` / `sendStdin()` / `closeStdin()` | ☑️ | |
| `CommandHandle.stdout` / `stderr` / `exitCode` / `error` | ✅ | |

## Files (`sandbox.files`)

| Method | Status | Notes |
| --- | --- | --- |
| `write(path, data)` | ✅ | |
| `read(path, { format })` | ✅ | `text`, `bytes`, `blob`, `stream` |
| `writeFiles([...])` | ☑️ | |
| `list(path, { depth })` | ☑️ | |
| `exists(path)` | ☑️ | |
| `getInfo(path)` | ☑️ | |
| `makeDir(path)` | ☑️ | |
| `rename(from, to)` | ☑️ | |
| `remove(path)` | ☑️ | |
| `watchDir(path, onEvent, { recursive })` | ☑️ | streams through the API proxy |

## Terminal (`sandbox.pty`)

| Method | Status | Notes |
| --- | --- | --- |
| `create({ cols, rows, onData })` | ✅ | |
| `kill(pid)` | ✅ | |
| `sendInput(pid, data)` | ☑️ | |
| `resize(pid, size)` | ☑️ | |
| `connect(pid)` | ☑️ | |

## Git (`sandbox.git`)

All git helpers run `git` inside the VM through `commands`, so they work as
far as the template has `git` (the `base` template does).

| Method | Status |
| --- | --- |
| `clone`, `init`, `status`, `branches`, `createBranch`, `checkoutBranch`, `deleteBranch` | ☑️ |
| `add`, `commit`, `reset`, `restore`, `pull`, `push` | ☑️ |
| `remoteAdd`, `remoteGet`, `getConfig`, `setConfig`, `configureUser`, `dangerouslyAuthenticate` | ☑️ |

## Templates (`Template`)

| Method | Status | Notes |
| --- | --- | --- |
| `Template.build(tpl, name, opts)` | ✅ | `cpuCount` and `memoryMB` optional, server defaults apply |
| `Template.buildInBackground` | ☑️ | same endpoints as `build` |
| `Template.getBuildStatus` | ✅ | with build logs |
| `Template.exists(name)` / `aliasExists` | ☑️ | |
| `Template.toDockerfile` / `toJSON` | ☑️ | local, no server call |
| `Template.getTags` / `assignTags` / `removeTags` | ❌ | tags are not kept; `name:tag` builds `name` |
| `fromTemplate(name)` | ✅ | |
| `fromImage(image, credentials?)` / `fromBaseImage()` | ✅ | `e2bdev/base` builds the `base` template |
| `fromUbuntuImage`, `fromDebianImage`, `fromPythonImage`, `fromNodeImage`, `fromBunImage`, `fromAlpineImage`, `fromArchImage`, `fromFedoraImage` | ☑️ | `fromImage` with a preset |
| `fromDockerfile(content)` | ☑️ | parsed locally into steps |
| `fromAWSRegistry` / `fromGCPRegistry` / registry credentials | ☑️ | passed to the builder |
| `runCmd`, `setEnvs`, `setUser`, `setWorkdir` | ✅ | |
| `aptInstall`, `pipInstall`, `npmInstall`, `bunInstall` | ✅ / ☑️ | `aptInstall` tested |
| `copy` / `copyItems` | ✅ | files are uploaded through the API |
| `makeDir`, `makeSymlink`, `remove`, `rename`, `gitClone` | ☑️ | `runCmd` presets |
| `setStartCmd`, `setReadyCmd` | ☑️ | |
| `skipCache` | ☑️ | |
| `addMcpServer` | ❌ | needs E2B's `mcp-gateway` template |
| `betaDevContainerPrebuild` / `betaSetDevContainerStart` | ⚠️ | needs a `devcontainer` template you build yourself |
| Delete a template (REST `DELETE /templates/{id}`) | ☑️ | refused while sandboxes use it |
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
