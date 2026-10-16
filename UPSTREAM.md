# What comes from E2B

Everything here is from [e2b-dev/runtime](https://github.com/e2b-dev/runtime)
(Apache-2.0, see `LICENSE`), commit `e456ecfcf4bc878616fba7c89bc0ba946acf6afd`.

| Here | Upstream | Changed |
| --- | --- | --- |
| `upstream/scripts/*.sh` | `embed/compose/scripts/` | no |
| `proto/*.proto` | `packages/orchestrator/` | no |
| `internal/pb/orchestrator` | `packages/shared/pkg/grpc/orchestrator` | no |
| `internal/pb/templatemanager` | `packages/shared/pkg/grpc/template-manager` | package name and import path |
| artifact versions in `Dockerfile` | `embed/compose/.env` | no |

The orchestrator release (`v0.16.202610051633-d1cd3db3890`) is built from a
commit that is not public. The protos are from the public commit above;
protobuf fields only get added, so a newer proto against this release is safe.

## Moving to a new release

1. Take the new versions from `embed/compose/.env` into the `Dockerfile`.
2. Copy the scripts and the generated Go files from the same commit, rewrite
   the template-manager import path as above, and update the commit here.
3. Diff `packages/api/internal/orchestrator/create_instance.go` and
   `packages/api/internal/template-manager/create_template.go` for new
   required fields of `SandboxConfig` and `TemplateConfig`.
4. Run `test/fake-e2e.sh`, then `node test/sdk/lifecycle.mjs --exec` against a
   real install.
