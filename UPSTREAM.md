# What comes from E2B

Everything here is from [e2b-dev/runtime](https://github.com/e2b-dev/runtime)
(Apache-2.0, see `LICENSE`), commit `e456ecfcf4bc878616fba7c89bc0ba946acf6afd`.

| Here | Upstream | Changed |
| --- | --- | --- |
| orchestrator binary | built in the `Dockerfile` from `packages/orchestrator` | `orchestrator/memory.patch` |
| `upstream/scripts/*.sh` | `embed/compose/scripts/` | no |
| `proto/*.proto` | `packages/orchestrator/` | no |
| `internal/pb/orchestrator` | `packages/shared/pkg/grpc/orchestrator` | no |
| `internal/pb/templatemanager` | `packages/shared/pkg/grpc/template-manager` | package name and import path |
| envd, kernel, Firecracker, BusyBox versions in `Dockerfile` | `embed/compose/.env` | no, released binaries |

## The orchestrator patch

`orchestrator/memory.patch` adds two switches that E2B otherwise reads from
LaunchDarkly, which this deployment does not run:

- `TEMPLATE_HUGE_PAGES=false` builds templates on 4 KiB pages instead of
  2 MiB hugepages, so sandboxes take host memory on demand and it is part of
  the memory cgroup.
- `TEMPLATE_FREE_PAGE_REPORTING=true` gives template VMs a balloon with free
  page reporting, so memory the guest frees goes back to the host. A cold
  boot (`filesystem_boot`) gets the same balloon as the template.

The entrypoint sets both from `MEMORY_MODE`.

`fetch-artifacts.sh` still downloads the released orchestrator (its checksum
is pinned); the entrypoint replaces it with the one built here.

## Moving to a new commit

1. Set `RUNTIME_COMMIT` in the `Dockerfile` and take the artifact versions
   from that commit's `embed/compose/.env`.
2. Check that `orchestrator/memory.patch` still applies (`git apply --check`).
3. Copy the scripts, protos and generated Go files from the same commit,
   rewrite the template-manager import path as above, and update the commit
   here.
4. Diff `packages/api/internal/orchestrator/create_instance.go` and
   `packages/api/internal/template-manager/create_template.go` for new
   required fields of `SandboxConfig` and `TemplateConfig`.
5. Run `test/fake-e2e.sh`, then `node test/sdk/lifecycle.mjs --exec` against a
   real install.
