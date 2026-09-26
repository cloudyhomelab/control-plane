# controlplane

A REST API that GitHub Actions runners call to run terraform, packer and ansible. Runners
never execute tools themselves: they can only invoke named actions from a YAML catalog,
with typed parameters, authenticated by GitHub OIDC tokens.

## How it works

- **Catalog** (`examples/catalog.yml`): each action names a tool, an operation, a repo and
  directory, typed `params`, which refs it may run, and `allow` rules over OIDC claims.
  Tools are `terraform`, `packer`, `ansible`, and `command` (a fixed argv such as
  `/usr/bin/uptime`, optionally without a repo).
- **Auth**: the runner sends its GitHub OIDC token. The server checks signature, issuer,
  audience and `repository_owner`, then matches the action's `allow` rules.
- **Code**: the server clones the repo with its own deploy key and runs the exact commit
  the ref resolved to at submit time. Callers only choose among `allowed_refs`.
- **Jobs**: `POST` returns a job id immediately; the runner polls status and log. Jobs with
  the same `lock` run one at a time. Cancel sends SIGINT to the tool's process group.
- **Plan, then apply**: terraform apply actions take `plan_job_id` and apply that saved plan
  file, at the plan's commit and params. A plan can be applied once, within `plan_max_age`.

Safety rules: no shell anywhere (argv only), unknown params rejected, `string` params must
have an anchored pattern, var values go through JSON var files, the child environment is
built from scratch, and values of `secrets` env vars are scrubbed from logs.

## API

| Method | Path | |
|---|---|---|
| GET | `/v1/actions` | actions the caller may invoke, with param schemas |
| POST | `/v1/actions/{name}/jobs` | `{"ref", "params", "plan_job_id"}` → 202 job |
| GET | `/v1/jobs/{id}` | job status |
| GET | `/v1/jobs/{id}/logs?offset=N` | raw log bytes; headers `X-Next-Offset`, `X-Job-Status` |
| GET | `/v1/jobs/{id}/artifacts/plan.json` | `terraform show -json` of a plan job |
| POST | `/v1/jobs/{id}/cancel` | cancel a queued or running job |

Callers see only jobs from their own repository, unless a `server.admins` rule matches.
Send `Idempotency-Key` to make retries safe (cpctl does this per workflow run attempt).

## Using it from a workflow

See `examples/workflows/network.yml`. The workflow needs `permissions: id-token: write`;
the `action/` composite builds `cpctl`, submits the job, streams the log and fails the step
if the job fails. Cancelling the workflow cancels the job.

## Running

```sh
go run ./cmd/controlplane -config examples/catalog.yml -check      # validate catalog
go run ./cmd/controlplane -config catalog.yml -data-dir ./data     # serve on 127.0.0.1:8080
```

Put it behind a TLS reverse proxy. `deploy/` has a systemd unit and a Containerfile.
Cloud credentials go in the server environment (`/etc/controlplane/env`) and reach a tool
only through the action's `env_profile`.

For local testing, `-insecure-dev-auth` (loopback only) accepts an unsigned base64url JSON
object of claims as the bearer token:

```sh
go run ./cmd/controlplane -config examples/dev-catalog.yml -data-dir ./data -insecure-dev-auth &
export CONTROLPLANE_URL=http://127.0.0.1:8080
export CONTROLPLANE_TOKEN=$(go run ./cmd/cpctl dev-token repository=cloudyhome/infra ref=refs/heads/main)
go run ./cmd/cpctl run host.uptime
```

## Tests

`go test ./...`. Tool runs use the test binary as a fake terraform, so no tools are needed.
