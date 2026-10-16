// Control-plane checks with the official JS SDK: template build (with a file
// upload), sandbox create, info, timeout, list, pause, resume, kill.
// Usage: E2B_API_URL=http://host:3000 E2B_SANDBOX_URL=http://host:3000 \
//        E2B_API_KEY=... node test/sdk/lifecycle.mjs [--exec]
// --exec also runs a command and a file round trip (needs a real orchestrator).
import { Sandbox, Template } from 'e2b'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import assert from 'node:assert/strict'

const exec = process.argv.includes('--exec')
const step = (m) => console.log(`- ${m}`)

const dir = mkdtempSync(join(tmpdir(), 'e2b-min-'))
writeFileSync(join(dir, 'hello.txt'), 'hello from the build context\n')

step('Template.build with a COPY step')
const tpl = Template({ fileContextPath: dir })
  .fromTemplate('base')
  .copy('hello.txt', '/home/user/hello.txt')
  .runCmd('echo built')
const info = await Template.build(tpl, 'sdk-test', {
  cpuCount: 1,
  memoryMB: 256,
  onBuildLogs: (e) => console.log(`  build: ${e.message}`),
})
assert.ok(info.templateId)

step('Sandbox.create')
const sbx = await Sandbox.create('sdk-test', { timeoutMs: 120_000, metadata: { suite: 'lifecycle' } })
assert.match(sbx.sandboxId, /^[a-z0-9]{20}$/)

step('getInfo')
const got = await sbx.getInfo()
assert.equal(got.state, 'running')
assert.equal(got.metadata.suite, 'lifecycle')

step('setTimeout')
await sbx.setTimeout(300_000)
const after = await sbx.getInfo()
assert.ok(after.endAt.getTime() - Date.now() > 250_000)

step('Sandbox.list with a metadata filter')
const listed = await Sandbox.list({ query: { metadata: { suite: 'lifecycle' } } }).nextItems()
assert.ok(listed.some((s) => s.sandboxId === sbx.sandboxId))

if (exec) {
  step('commands.run and files')
  const r = await sbx.commands.run('cat /home/user/hello.txt && uname -a')
  assert.match(r.stdout, /hello from the build context/)
  await sbx.files.write('/tmp/x.txt', 'round trip')
  assert.equal(await sbx.files.read('/tmp/x.txt'), 'round trip')
}

step('pause')
await sbx.pause()
assert.equal((await Sandbox.getInfo(sbx.sandboxId)).state, 'paused')

step('connect resumes')
const resumed = await Sandbox.connect(sbx.sandboxId, { timeoutMs: 120_000 })
assert.equal((await resumed.getInfo()).state, 'running')
if (exec) {
  assert.equal(await resumed.files.read('/tmp/x.txt'), 'round trip')
}

step('kill')
await resumed.kill()
await assert.rejects(Sandbox.getInfo(sbx.sandboxId))

console.log('lifecycle: ok')
