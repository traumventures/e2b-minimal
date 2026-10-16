"""Control-plane checks with the official Python SDK.

Usage: E2B_API_URL=... E2B_SANDBOX_URL=... E2B_API_KEY=... python test/sdk/lifecycle.py [--exec]
"""

import sys

from e2b import Sandbox

run_exec = "--exec" in sys.argv

print("- create")
sbx = Sandbox.create("base", timeout=120, metadata={"suite": "py"})
info = sbx.get_info()
assert info.state.value == "running", info.state

print("- set_timeout")
sbx.set_timeout(200)

print("- list")
ids = [s.sandbox_id for s in Sandbox.list().next_items()]
assert sbx.sandbox_id in ids

if run_exec:
    print("- commands.run")
    assert sbx.commands.run("echo hi").stdout.strip() == "hi"

print("- pause / connect")
sbx.pause()
assert Sandbox.get_info(sbx.sandbox_id).state.value == "paused"
sbx = Sandbox.connect(sbx.sandbox_id)
assert sbx.get_info().state.value == "running"

print("- kill")
sbx.kill()
print("lifecycle.py: ok")
