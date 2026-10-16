// Exercises every SDK feature COMPATIBILITY.md lists, against a real install.
// Each check reports on its own, so one failure does not hide the rest.
//   E2B_API_URL=... E2B_SANDBOX_URL=... E2B_API_KEY=... node compat.mjs [area...]
// Areas: sandbox commands files pty git templates presets
import { Sandbox, Template } from 'e2b'
import assert from 'node:assert/strict'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const API = process.env.E2B_API_URL
const KEY = process.env.E2B_API_KEY
const only = process.argv.slice(2)
const results = []
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

async function check(area, name, fn) {
  if (only.length && !only.includes(area)) return
  const t = Date.now()
  try {
    await fn()
    results.push({ area, name, ok: true })
    console.log(`  ok    ${area.padEnd(9)} ${name} (${Date.now() - t} ms)`)
  } catch (e) {
    results.push({ area, name, ok: false, err: String(e?.message ?? e).split('\n')[0].slice(0, 200) })
    console.log(`  FAIL  ${area.padEnd(9)} ${name}: ${results.at(-1).err}`)
  }
}

const run = (sbx, cmd, opts) => sbx.commands.run(cmd, { timeoutMs: 120_000, ...opts })

// ---------------------------------------------------------------- sandbox
const sbx = await Sandbox.create({ timeoutMs: 600_000, envs: { COMPAT_ENV: 'from-create' }, metadata: { suite: 'compat' } })

await check('sandbox', 'opts.envs', async () => {
  assert.equal((await run(sbx, 'echo $COMPAT_ENV')).stdout.trim(), 'from-create')
})
await check('sandbox', 'secure (envd access token required)', async () => {
  const info = await Sandbox.getFullInfo(sbx.sandboxId)
  assert.ok(info.envdAccessToken || sbx.envdAccessToken, 'no envd access token')
  const res = await fetch(`${process.env.E2B_SANDBOX_URL}/files?path=/etc/hostname`, {
    headers: { 'E2b-Sandbox-Id': sbx.sandboxId, 'E2b-Sandbox-Port': '49983' },
  })
  assert.equal(res.status, 401, `unauthenticated envd read gave ${res.status}`)
})
await check('sandbox', 'Sandbox.getFullInfo', async () => {
  const info = await Sandbox.getFullInfo(sbx.sandboxId)
  assert.equal(info.sandboxId, sbx.sandboxId)
  assert.equal(info.metadata.suite, 'compat')
})
await check('sandbox', 'isRunning', async () => {
  assert.equal(await sbx.isRunning(), true)
})
await check('sandbox', 'list({ state })', async () => {
  const running = await Sandbox.list({ query: { state: ['running'], metadata: { suite: 'compat' } } }).nextItems()
  assert.ok(running.some((s) => s.sandboxId === sbx.sandboxId))
  const paused = await Sandbox.list({ query: { state: ['paused'], metadata: { suite: 'compat' } } }).nextItems()
  assert.ok(!paused.some((s) => s.sandboxId === sbx.sandboxId))
})
// Signed URLs carry no sandbox id when E2B_SANDBOX_URL is set; they work
// through host routing (<port>-<id>.<SANDBOX_DOMAIN>), simulated here with a
// Host header against the API address.
const viaHost = (signed) => {
  const u = new URL(signed)
  return [`${API}${u.pathname}${u.search}`, { Host: `49983-${sbx.sandboxId}.e2b.local` }]
}
await check('sandbox', 'uploadUrl / downloadUrl (host routing)', async () => {
  const [up, h1] = viaHost(await sbx.uploadUrl('/tmp/url-upload.txt'))
  const form = new FormData()
  form.append('file', new Blob(['via signed url']), 'url-upload.txt')
  const r = await fetch(up, { method: 'POST', body: form, headers: h1 })
  assert.ok(r.ok, `upload ${r.status}`)
  const [down, h2] = viaHost(await sbx.downloadUrl('/tmp/url-upload.txt'))
  assert.equal(await (await fetch(down, { headers: h2 })).text(), 'via signed url')
  const [bad, h3] = viaHost((await sbx.downloadUrl('/tmp/url-upload.txt')).replace(/signature=[^&]+/, 'signature=v1_bad'))
  assert.equal((await fetch(bad, { headers: h3 })).status, 401, 'bad signature accepted')
})
await check('sandbox', 'network: domain in allowOut without denyOut 0.0.0.0/0 is refused', async () => {
  await assert.rejects(Sandbox.create({ timeoutMs: 60_000, network: { allowOut: ['example.com'] } }), /0\.0\.0\.0\/0|ALL_TRAFFIC/)
})
await check('sandbox', 'allowInternetAccess: true', async () => {
  const r = await run(sbx, 'curl -s -o /dev/null -w "%{http_code}" --max-time 10 https://example.com')
  assert.match(r.stdout, /^[23]\d\d$/)
})
await check('sandbox', 'allowInternetAccess: false', async () => {
  const s = await Sandbox.create({ timeoutMs: 120_000, allowInternetAccess: false })
  try {
    const r = await run(s, 'curl -s -o /dev/null -w "%{http_code}" --max-time 5 https://example.com || echo blocked')
    assert.match(r.stdout, /blocked|000/)
  } finally { await s.kill() }
})
await check('sandbox', 'network.denyOut (CIDR)', async () => {
  const s = await Sandbox.create({ timeoutMs: 120_000, network: { denyOut: ['1.1.1.1/32'] } })
  try {
    const blocked = await run(s, 'curl -s -o /dev/null -w "%{http_code}" --max-time 5 https://1.1.1.1 || echo blocked')
    assert.match(blocked.stdout, /blocked|000/)
    const other = await run(s, 'curl -s -o /dev/null -w "%{http_code}" --max-time 10 https://8.8.8.8 || echo blocked')
    assert.doesNotMatch(other.stdout, /blocked|000/, 'other hosts should stay reachable')
  } finally { await s.kill() }
})
await check('sandbox', 'network.allowOut (domain) with denyOut 0.0.0.0/0', async () => {
  const s = await Sandbox.create({ timeoutMs: 120_000, network: { allowOut: ['example.com'], denyOut: ['0.0.0.0/0'] } })
  try {
    const ok = await run(s, 'curl -s -o /dev/null -w "%{http_code}" --max-time 10 https://example.com || echo blocked')
    assert.match(ok.stdout, /^[23]\d\d$/, `allowed domain: ${ok.stdout}`)
    const no = await run(s, 'curl -s -o /dev/null -w "%{http_code}" --max-time 5 https://github.com || echo blocked')
    assert.match(no.stdout, /blocked|000/, `other domain: ${no.stdout}`)
  } finally { await s.kill() }
})
await check('sandbox', 'network.allowPublicTraffic: false', async () => {
  const s = await Sandbox.create({ timeoutMs: 120_000, network: { allowPublicTraffic: false } })
  try {
    await run(s, 'python3 -m http.server 8000 >/dev/null 2>&1', { background: true })
    await sleep(1500)
    const host = s.getHost(8000)
    const without = await fetch(`${API}/`, { headers: { Host: host } })
    assert.equal(without.status, 403, `without token: ${without.status}`)
    const info = await Sandbox.getInfo(s.sandboxId)
    const token = s.trafficAccessToken ?? info.trafficAccessToken
    assert.ok(token, 'no traffic access token on the sandbox')
    const withTok = await fetch(`${API}/`, { headers: { Host: host, 'e2b-traffic-access-token': token } })
    assert.equal(withTok.status, 200, `with token: ${withTok.status}`)
  } finally { await s.kill() }
})
await check('sandbox', 'pause({ keepMemory: false }) then connect reboots', async () => {
  const s = await Sandbox.create({ timeoutMs: 300_000 })
  await s.files.write('/home/user/disk.txt', 'on disk')
  await run(s, 'sleep 3600 >/dev/null 2>&1', { background: true })
  await s.pause({ keepMemory: false })
  assert.equal((await Sandbox.getInfo(s.sandboxId)).state, 'paused')
  const r = await Sandbox.connect(s.sandboxId, { timeoutMs: 120_000 })
  try {
    assert.equal(await r.files.read('/home/user/disk.txt'), 'on disk')
    const ps = await run(r, 'pgrep -x sleep || echo none')
    assert.match(ps.stdout, /none/, 'process survived a disk-only pause')
  } finally { await r.kill() }
})
await check('sandbox', 'betaPause', async () => {
  const s = await Sandbox.create({ timeoutMs: 120_000 })
  await s.betaPause()
  assert.equal((await Sandbox.getInfo(s.sandboxId)).state, 'paused')
  await Sandbox.kill(s.sandboxId)
})
await check('sandbox', 'Sandbox.kill(id) on a paused sandbox', async () => {
  const s = await Sandbox.create({ timeoutMs: 120_000 })
  await s.pause()
  assert.equal(await Sandbox.kill(s.sandboxId), true)
  await assert.rejects(Sandbox.getInfo(s.sandboxId))
})

// --------------------------------------------------------------- commands
await check('commands', 'run with cwd, user and envs', async () => {
  const r = await run(sbx, 'pwd; whoami; echo $X', { cwd: '/tmp', user: 'root', envs: { X: 'y' } })
  assert.deepEqual(r.stdout.trim().split('\n'), ['/tmp', 'root', 'y'])
})
await check('commands', 'run with onStdout/onStderr streaming', async () => {
  const out = [], err = []
  await run(sbx, 'echo a; echo b >&2; sleep 0.2; echo c', { onStdout: (d) => out.push(d), onStderr: (d) => err.push(d) })
  assert.equal(out.join(''), 'a\nc\n')
  assert.equal(err.join(''), 'b\n')
})
await check('commands', 'non-zero exit raises CommandExitError', async () => {
  await assert.rejects(run(sbx, 'exit 3'), (e) => e.exitCode === 3)
})
await check('commands', 'list / connect / kill', async () => {
  const h = await run(sbx, 'sleep 300', { background: true })
  const list = await sbx.commands.list()
  assert.ok(list.some((p) => p.pid === h.pid), 'not in list')
  const again = await sbx.commands.connect(h.pid)
  assert.equal(again.pid, h.pid)
  assert.equal(await sbx.commands.kill(h.pid), true)
  await assert.rejects(again.wait())
})
await check('commands', 'sendStdin / closeStdin', async () => {
  const h = await run(sbx, 'cat', { background: true, stdin: true })
  await sbx.commands.sendStdin(h.pid, 'hello stdin\n')
  if (sbx.commands.supportsStdinClose) {
    await sbx.commands.closeStdin(h.pid)
    const res = await h.wait()
    assert.equal(res.stdout, 'hello stdin\n')
  } else {
    await sleep(500)
    assert.equal(h.stdout, 'hello stdin\n')
    await h.kill()
  }
})
await check('commands', 'CommandHandle wait / sendStdin / closeStdin / kill / disconnect', async () => {
  const h = await run(sbx, 'read l; echo got:$l', { background: true, stdin: true })
  await h.sendStdin('x\n')
  const res = await h.wait()
  assert.equal(res.stdout.trim(), 'got:x')
  assert.equal(h.exitCode, 0)
  const k = await run(sbx, 'sleep 300', { background: true })
  assert.equal(await k.kill(), true)
  const d = await run(sbx, 'sleep 2; echo late', { background: true })
  await d.disconnect()
  const back = await sbx.commands.connect(d.pid)
  assert.equal((await back.wait()).stdout.trim(), 'late')
})

// ------------------------------------------------------------------ files
await check('files', 'writeFiles', async () => {
  await sbx.files.writeFiles([
    { path: '/tmp/wf/a.txt', data: 'A' },
    { path: '/tmp/wf/b.bin', data: new Uint8Array([1, 2, 3]).buffer },
  ])
  assert.equal(await sbx.files.read('/tmp/wf/a.txt'), 'A')
  assert.deepEqual([...(await sbx.files.read('/tmp/wf/b.bin', { format: 'bytes' }))], [1, 2, 3])
})
await check('files', 'read formats blob / stream', async () => {
  const blob = await sbx.files.read('/tmp/wf/a.txt', { format: 'blob' })
  assert.equal(await blob.text(), 'A')
  const stream = await sbx.files.read('/tmp/wf/a.txt', { format: 'stream' })
  assert.equal(await new Response(stream).text(), 'A')
})
await check('files', 'makeDir / exists / getInfo', async () => {
  assert.equal(await sbx.files.makeDir('/tmp/md/x/y'), true)
  assert.equal(await sbx.files.exists('/tmp/md/x/y'), true)
  assert.equal(await sbx.files.exists('/tmp/md/nope'), false)
  const info = await sbx.files.getInfo('/tmp/wf/a.txt')
  assert.equal(info.size, 1)
  assert.equal(info.type, 'file')
})
await check('files', 'list with depth', async () => {
  const flat = await sbx.files.list('/tmp/md')
  assert.deepEqual(flat.map((e) => e.name), ['x'])
  const deep = await sbx.files.list('/tmp/md', { depth: 2 })
  assert.ok(deep.some((e) => e.path === '/tmp/md/x/y'))
})
await check('files', 'rename / remove', async () => {
  await sbx.files.rename('/tmp/wf/a.txt', '/tmp/wf/c.txt')
  assert.equal(await sbx.files.exists('/tmp/wf/a.txt'), false)
  await sbx.files.remove('/tmp/wf/c.txt')
  assert.equal(await sbx.files.exists('/tmp/wf/c.txt'), false)
})
await check('files', 'watchDir (recursive)', async () => {
  await sbx.files.makeDir('/tmp/watch/sub')
  const events = []
  const w = await sbx.files.watchDir('/tmp/watch', (e) => events.push(e), { recursive: true })
  await sbx.files.write('/tmp/watch/sub/new.txt', 'x')
  await sleep(1500)
  await w.stop()
  assert.ok(events.some((e) => e.name.includes('new.txt')), `events: ${JSON.stringify(events)}`)
})
await check('files', 'large file round trip (50 MB)', async () => {
  const big = new Uint8Array(50 * 1024 * 1024).map((_, i) => i % 251)
  await sbx.files.write('/tmp/big.bin', big.buffer)
  const back = await sbx.files.read('/tmp/big.bin', { format: 'bytes' })
  assert.equal(back.length, big.length)
  assert.equal(back[12345678], big[12345678])
})

// -------------------------------------------------------------------- pty
await check('pty', 'create / sendInput / resize / connect / kill', async () => {
  let out = ''
  const p = await sbx.pty.create({ cols: 80, rows: 24, onData: (d) => { out += new TextDecoder().decode(d) } })
  await sbx.pty.sendInput(p.pid, new TextEncoder().encode('echo pty-$((40+2))\n'))
  await sleep(800)
  assert.match(out, /pty-42/)
  await sbx.pty.resize(p.pid, { cols: 132, rows: 40 })
  await sbx.pty.sendInput(p.pid, new TextEncoder().encode('stty size\n'))
  await sleep(800)
  assert.match(out, /40 132/)
  await p.disconnect()
  let out2 = ''
  const again = await sbx.pty.connect(p.pid, { onData: (d) => { out2 += new TextDecoder().decode(d) } })
  await sbx.pty.sendInput(p.pid, new TextEncoder().encode('echo reconnected\n'))
  await sleep(800)
  assert.match(out2, /reconnected/)
  assert.equal(await sbx.pty.kill(again.pid), true)
})

// -------------------------------------------------------------------- git
await check('git', 'init / configureUser / add / commit / status', async () => {
  await run(sbx, 'mkdir -p /home/user/repo')
  await sbx.git.init('/home/user/repo', { initialBranch: 'main' })
  await sbx.git.configureUser('Compat Test', 'compat@example.com', { scope: 'local', path: '/home/user/repo' })
  await sbx.files.write('/home/user/repo/a.txt', 'one')
  let st = await sbx.git.status('/home/user/repo')
  assert.ok(st.fileStatus.some((f) => f.name === 'a.txt'), `status: ${JSON.stringify(st)}`)
  await sbx.git.add('/home/user/repo', { all: true })
  await sbx.git.commit('/home/user/repo', 'first')
  st = await sbx.git.status('/home/user/repo')
  assert.equal(st.fileStatus.length, 0)
})
await check('git', 'branches / createBranch / checkoutBranch / deleteBranch', async () => {
  await sbx.git.createBranch('/home/user/repo', 'feature')
  await sbx.git.checkoutBranch('/home/user/repo', 'main')
  const b = await sbx.git.branches('/home/user/repo')
  assert.ok(b.branches.includes('feature') && b.currentBranch === 'main', JSON.stringify(b))
  await sbx.git.deleteBranch('/home/user/repo', 'feature', { force: true })
  assert.ok(!(await sbx.git.branches('/home/user/repo')).branches.includes('feature'))
})
await check('git', 'reset / restore', async () => {
  await sbx.files.write('/home/user/repo/a.txt', 'changed')
  await sbx.git.restore('/home/user/repo', { paths: ['a.txt'] })
  assert.equal(await sbx.files.read('/home/user/repo/a.txt'), 'one')
  await sbx.files.write('/home/user/repo/b.txt', 'b')
  await sbx.git.add('/home/user/repo', { files: ['b.txt'] })
  await sbx.git.reset('/home/user/repo', { paths: ['b.txt'] })
  const st = await sbx.git.status('/home/user/repo')
  assert.ok(st.fileStatus.some((f) => f.name === 'b.txt' && !f.staged), JSON.stringify(st.fileStatus))
})
await check('git', 'getConfig / setConfig', async () => {
  await sbx.git.setConfig('compat.key', 'value', { scope: 'local', path: '/home/user/repo' })
  assert.equal(await sbx.git.getConfig('compat.key', { scope: 'local', path: '/home/user/repo' }), 'value')
})
await check('git', 'remoteAdd / remoteGet / push / pull', async () => {
  await run(sbx, 'git init -q --bare -b main /home/user/remote.git')
  await sbx.git.remoteAdd('/home/user/repo', 'origin', '/home/user/remote.git')
  assert.equal(await sbx.git.remoteGet('/home/user/repo', 'origin'), '/home/user/remote.git')
  await sbx.git.push('/home/user/repo', { remote: 'origin', branch: 'main', setUpstream: true })
  await run(sbx, 'git clone -q /home/user/remote.git /home/user/other && cd /home/user/other && git -c user.name=x -c user.email=x@x commit -q --allow-empty -m second && git push -q')
  await sbx.git.pull('/home/user/repo', { remote: 'origin', branch: 'main' })
  const log = await run(sbx, 'git -C /home/user/repo log --oneline | wc -l')
  assert.equal(log.stdout.trim(), '2')
})
await check('git', 'clone (https)', async () => {
  await sbx.git.clone('https://github.com/octocat/Hello-World.git', { path: '/home/user/hello', depth: 1 })
  assert.equal(await sbx.files.exists('/home/user/hello/README'), true)
})
await check('git', 'dangerouslyAuthenticate', async () => {
  await sbx.git.dangerouslyAuthenticate({ username: 'user', password: 'token', host: 'example.com' })
  const r = await run(sbx, 'git config --global credential.helper')
  assert.ok(r.stdout.trim().length > 0)
})

// -------------------------------------------------------------- templates
await check('templates', 'toDockerfile / toJSON (local)', async () => {
  const t = Template().fromImage('ubuntu:24.04').runCmd('echo hi').setEnvs({ A: 'b' })
  assert.match(Template.toDockerfile(t), /FROM ubuntu:24\.04[\s\S]*RUN echo hi/)
  assert.ok(JSON.parse(await Template.toJSON(t)).steps.length >= 2)
})
await check('templates', 'exists / aliasExists', async () => {
  assert.equal(await Template.exists('base'), true)
  assert.equal(await Template.aliasExists('does-not-exist-xyz'), false)
})

const ctx = mkdtempSync(join(tmpdir(), 'e2b-compat-'))
writeFileSync(join(ctx, 'requirements.txt'), 'six==1.16.0\n')
writeFileSync(join(ctx, 'package.json'), JSON.stringify({ name: 'x', private: true, dependencies: { 'left-pad': '1.3.0' } }))
await check('templates', 'steps: pip/npm/bun install, dirs, symlink, rename, remove, gitClone, start/ready cmd, skipCache', async () => {
  const t = Template({ fileContextPath: ctx })
    .fromTemplate('base')
    .copy('requirements.txt', '/home/user/req/requirements.txt')
    .copy('package.json', '/home/user/app/package.json')
    .runCmd('chown -R user:user /home/user/app', { user: 'root' })
    .pipInstall(['-r', '/home/user/req/requirements.txt'])
    .npmInstall(['is-number@7.0.0'], { g: true })
    .runCmd('curl -fsSL https://bun.sh/install | BUN_INSTALL=/usr/local bash', { user: 'root' })
    .setWorkdir('/home/user/app')
    .bunInstall()
    .makeDir('/home/user/made/deep')
    .makeSymlink('/home/user/made', '/home/user/link')
    .runCmd('echo r > /home/user/made/before.txt && echo gone > /home/user/made/gone.txt')
    .rename('/home/user/made/before.txt', '/home/user/made/after.txt')
    .remove('/home/user/made/gone.txt')
    .gitClone('https://github.com/octocat/Hello-World.git', '/home/user/hw', { depth: 1 })
    .skipCache()
    .runCmd('date +%s%N > /home/user/built-at')
    .setStartCmd('sh -c "echo started > /tmp/started; sleep infinity"', 'test -f /tmp/started')
  await Template.build(t, 'compat-steps', { memoryMB: 1024 })
  const s = await Sandbox.create('compat-steps', { timeoutMs: 120_000 })
  try {
    const r = await run(s, [
      'python3 -c "import six; print(six.__version__)"',
      'ls /usr/local/lib/node_modules /usr/lib/node_modules 2>/dev/null | grep -c is-number',
      'test -d /home/user/app/node_modules/left-pad && echo bun-ok',
      'test -d /home/user/made/deep && readlink /home/user/link',
      'cat /home/user/made/after.txt; test -e /home/user/made/gone.txt || echo removed',
      'test -f /home/user/hw/README && echo cloned',
      'cat /tmp/started',
    ].join(' && '))
    assert.deepEqual(r.stdout.trim().split('\n'), ['1.16.0', '1', 'bun-ok', '/home/user/made', 'r', 'removed', 'cloned', 'started'])
  } finally { await s.kill() }
})
await check('templates', 'fromDockerfile', async () => {
  const t = Template().fromDockerfile('FROM e2bdev/base\nENV DF=from-env\nRUN echo from-dockerfile $DF > /home/user/df.txt\n')
  await Template.build(t, 'compat-dockerfile', { memoryMB: 1024 })
  const s = await Sandbox.create('compat-dockerfile', { timeoutMs: 60_000 })
  // ENV applies during the build only (SDK docs for setEnvs)
  try { assert.equal((await run(s, 'cat /home/user/df.txt')).stdout.trim(), 'from-dockerfile from-env') } finally { await s.kill() }
})
await check('templates', 'buildInBackground + getBuildStatus', async () => {
  const t = Template().fromTemplate('base').runCmd('echo bg > /home/user/bg.txt')
  const info = await Template.buildInBackground(t, 'compat-bg', { memoryMB: 1024 })
  let st
  for (let i = 0; i < 120; i++) {
    st = await Template.getBuildStatus(info)
    if (st.status === 'ready' || st.status === 'error') break
    await sleep(2000)
  }
  assert.equal(st.status, 'ready')
  const s = await Sandbox.create('compat-bg', { timeoutMs: 60_000 })
  try { assert.equal(await s.files.read('/home/user/bg.txt'), 'bg\n') } finally { await s.kill() }
})
// A private registry with basic auth holding e2bdev/base as :latest and
// :badcreds: REGISTRY=host:port REGISTRY_USER=... REGISTRY_PASSWORD=...
if (process.env.REGISTRY) {
  // Wrong credentials first, on a tag nothing has pulled yet: the builder
  // caches an image once pulled, so a later pull would not authenticate.
  await check('templates', 'fromImage from a private registry with wrong credentials fails', async () => {
    const t = Template().fromImage(`${process.env.REGISTRY}/compat/base:badcreds`, { username: process.env.REGISTRY_USER, password: 'wrong' }).runCmd('true')
    await assert.rejects(Template.build(t, 'compat-badcreds', { memoryMB: 1024 }), /unauthorized|401|auth|denied/i)
  })
  await check('templates', 'fromImage from a private registry with credentials', async () => {
    const t = Template().fromImage(`${process.env.REGISTRY}/compat/base:latest`, { username: process.env.REGISTRY_USER, password: process.env.REGISTRY_PASSWORD }).runCmd('echo private > /home/user/p.txt')
    await Template.build(t, 'compat-private', { memoryMB: 1024 })
    const s = await Sandbox.create('compat-private', { timeoutMs: 60_000 })
    try { assert.equal(await s.files.read('/home/user/p.txt'), 'private\n') } finally { await s.kill() }
  })
}
await check('templates', 'delete template (REST)', async () => {
  const list = await (await fetch(`${API}/templates`, { headers: { 'X-API-Key': KEY } })).json()
  for (const name of ['compat-steps', 'compat-dockerfile', 'compat-bg', 'compat-private', 'compat-badcreds']) {
    const t = list.find((x) => x.names.includes(name))
    if (!t) continue
    const r = await fetch(`${API}/templates/${t.templateID}`, { method: 'DELETE', headers: { 'X-API-Key': KEY } })
    assert.equal(r.status, 204, `${name}: ${r.status}`)
  }
  assert.equal(await Template.exists('compat-steps'), false)
})

// ---------------------------------------------------------------- presets
const presets = [
  ['fromUbuntuImage', (t) => t.fromUbuntuImage('24.04')],
  ['fromDebianImage', (t) => t.fromDebianImage('bookworm')],
  ['fromPythonImage', (t) => t.fromPythonImage('3.12')],
  ['fromNodeImage', (t) => t.fromNodeImage('22')],
  ['fromBunImage', (t) => t.fromBunImage('1.2')],
  ['fromAlpineImage', (t) => t.fromAlpineImage('3.20')],
  ['fromArchImage', (t) => t.fromArchImage()],
  ['fromFedoraImage', (t) => t.fromFedoraImage('41')],
]
for (const [name, make] of presets) {
  await check('presets', name, async () => {
    const alias = `compat-${name.toLowerCase()}`
    await Template.build(make(Template()), alias, { memoryMB: 1024 })
    const s = await Sandbox.create(alias, { timeoutMs: 60_000 })
    try {
      const r = await run(s, 'cat /etc/os-release | head -1; echo ok')
      assert.match(r.stdout, /ok/)
    } finally {
      await s.kill()
      const list = await (await fetch(`${API}/templates`, { headers: { 'X-API-Key': KEY } })).json()
      const t = list.find((x) => x.names.includes(alias))
      if (t) await fetch(`${API}/templates/${t.templateID}`, { method: 'DELETE', headers: { 'X-API-Key': KEY } })
    }
  })
}

await sbx.kill()
const failed = results.filter((r) => !r.ok)
console.log(`\n${results.length - failed.length}/${results.length} passed`)
for (const f of failed) console.log(`FAIL ${f.area} / ${f.name}: ${f.err}`)
process.exit(failed.length ? 1 : 0)
