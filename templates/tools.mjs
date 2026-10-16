// Template "tools": uv (using the base image's Python), Bun and Typst on top
// of the base template.
//
//   cd templates && npm install
//   E2B_API_URL=... E2B_API_KEY=... node tools.mjs
//
// Then: Sandbox.create('tools')
import { Template } from 'e2b'

const BUN_VERSION = '1.4.3'
const UV_VERSION = '0.13.1'
const TYPST_VERSION = '0.15.1'

export const tools = Template()
  .fromTemplate('base')
  .aptInstall(['curl', 'ca-certificates', 'unzip', 'xz-utils'], { noInstallRecommends: true })
  // uv into /usr/local/bin. It uses the base image's Python and never downloads one.
  .runCmd(
    `curl -LsSf https://astral.sh/uv/${UV_VERSION}/install.sh | env UV_INSTALL_DIR=/usr/local/bin UV_NO_MODIFY_PATH=1 sh`,
    { user: 'root' },
  )
  // Bun, pinned.
  .runCmd(
    [
      `curl -fsSL https://bun.sh/install | env BUN_INSTALL=/usr/local bash -s "bun-v${BUN_VERSION}"`,
      'ln -sf /usr/local/bin/bun /usr/local/bin/bunx',
    ],
    { user: 'root' },
  )
  // Typst, static musl build from the GitHub release.
  .runCmd(
    [
      `curl -fsSL -o /tmp/typst.tar.xz https://github.com/typst/typst/releases/download/v${TYPST_VERSION}/typst-x86_64-unknown-linux-musl.tar.xz`,
      'tar -xJf /tmp/typst.tar.xz -C /tmp',
      'install -m 0755 /tmp/typst-x86_64-unknown-linux-musl/typst /usr/local/bin/typst',
      'rm -rf /tmp/typst.tar.xz /tmp/typst-x86_64-unknown-linux-musl',
    ],
    { user: 'root' },
  )
  .setEnvs({ UV_PYTHON_PREFERENCE: 'only-system', UV_PYTHON_DOWNLOADS: 'never', UV_LINK_MODE: 'copy' })
  .runCmd('uv --version && python --version && bun --version && typst --version')

if (import.meta.url === `file://${process.argv[1]}`) {
  // No cpuCount/memoryMB: the server's defaults apply (all cores and
  // DEFAULT_MEMORY_MB); with MEMORY_MODE=regular a sandbox only takes the
  // memory it uses.
  const info = await Template.build(tools, 'tools', {
    onBuildLogs: (e) => console.log(String(e)),
  })
  console.log(`built tools: ${info.templateId}`)
}
