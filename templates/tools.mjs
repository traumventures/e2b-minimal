// Template "tools": uv + Python, Bun, Typst and Tesseract (German and English)
// on top of the base template.
//
//   cd templates && npm install
//   E2B_API_URL=... E2B_API_KEY=... node tools.mjs
//
// Then: Sandbox.create('tools')
import { Template } from 'e2b'

const BUN_VERSION = '1.4.3'
const UV_VERSION = '0.13.1'
const TYPST_VERSION = '0.15.1'
const PYTHON_VERSION = '3.13'

export const tools = Template()
  .fromTemplate('base')
  .aptInstall(
    ['curl', 'ca-certificates', 'unzip', 'xz-utils', 'tesseract-ocr', 'tesseract-ocr-deu', 'tesseract-ocr-eng'],
    { noInstallRecommends: true },
  )
  // uv into /usr/local/bin, and a uv-managed Python as the default `python`.
  .runCmd(
    [
      `curl -LsSf https://astral.sh/uv/${UV_VERSION}/install.sh | env UV_INSTALL_DIR=/usr/local/bin UV_NO_MODIFY_PATH=1 sh`,
      `env UV_PYTHON_INSTALL_DIR=/opt/uv/python UV_PYTHON_BIN_DIR=/usr/local/bin uv python install ${PYTHON_VERSION} --default --preview-features python-install-default`,
      'chmod -R a+rX /opt/uv',
      // The base image ships Python 3.11 in /usr/local/bin; point python and python3 at 3.13.
      'ln -sf python3.13 /usr/local/bin/python3 && ln -sf python3 /usr/local/bin/python',
    ],
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
  .setEnvs({ UV_PYTHON_INSTALL_DIR: '/opt/uv/python', UV_PYTHON: PYTHON_VERSION, UV_LINK_MODE: 'copy' })
  .runCmd('uv --version && python --version && bun --version && typst --version && tesseract --version | head -1')

if (import.meta.url === `file://${process.argv[1]}`) {
  const info = await Template.build(tools, 'tools', {
    cpuCount: 2,
    memoryMB: 1024,
    onBuildLogs: (e) => console.log(String(e)),
  })
  console.log(`built tools: ${info.templateId}`)
}
