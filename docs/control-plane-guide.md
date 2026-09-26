# Control plane: how it works and how to use it

This guide walks through the control plane from the inside out:

1. How a request becomes a command
2. How catalog entries map to commands
3. Adding and allowing a new command (`uptime`)
4. Testing it by hand on your machine (no GitHub involved)
5. Building and running the container
6. Calling it from a GitHub Actions workflow
7. Troubleshooting

---

## 1. How a request becomes a command

The GitHub runner never runs terraform, packer, ansible or anything else. It can only ask
the control plane to run a **named action** from the catalog (`catalog.yml`). The server
decides whether the caller may run it, and builds the exact command itself.

```
GitHub runner                              control plane host
-------------                              ------------------
cpctl run host.uptime
  |  1. ask GitHub for an OIDC token
  |  2. POST /v1/actions/host.uptime/jobs  ->  3. verify token (signature, issuer,
  |     Authorization: Bearer <token>             audience, repository_owner)
  |                                            4. match the action's `allow` rules
  |                                            5. validate params against the schema
  |                                            6. (repo actions) fetch the ref, pin the commit
  |  <- 202 {"id": "..."}                      7. queue the job, return its id
  |                                            8. worker: take lock, build argv, run it
  |  9. GET /v1/jobs/{id}/logs?offset=N  <-       (no shell, clean environment,
  |     (poll until the job is done)              secrets scrubbed from the log)
  | 10. exit with the job's exit code
```

Key points:

- **Identity comes from GitHub, not from a secret.** Every workflow run can get a short-lived
  OIDC token signed by GitHub. It says which repository, branch, workflow and environment the
  job is running in. The server checks the signature against GitHub's public keys.
- **The catalog is the whole attack surface.** A caller can only pick an action name and fill
  in declared parameters. Everything else (binary, flags, directory, environment) is fixed on
  the server.
- **Jobs are asynchronous.** Submitting returns immediately with a job id. `cpctl` then
  streams the log by polling. Long terraform applies do not depend on one HTTP request
  staying open.
- **State** lives in `<data-dir>`: `controlplane.db` (SQLite: jobs and the audit log),
  `jobs/<id>/log` (job output), `jobs/<id>/tfplan` (saved plans), `repos/` (git mirrors).

## 2. How catalog entries map to commands

Each action has a `tool` and an `op`. The server turns them into argv lists. There is no
shell anywhere, so `;`, `|`, `$(...)` in a value are just characters.

| tool / op | what runs (in `<repo>/<dir>`) |
|---|---|
| `terraform` / `plan` | `terraform init -input=false -no-color`<br>`terraform plan -input=false -no-color -out=<job>/tfplan -var-file=<job>/vars.tfvars.json`<br>`terraform show -json <job>/tfplan` (saved as the `plan.json` artifact) |
| `terraform` / `apply` (via `from_plan`) | `terraform init ...`<br>`terraform apply -input=false -no-color <plan job>/tfplan` |
| `terraform` / `validate` | `terraform init -backend=false ...`, `terraform validate` |
| `packer` / `build`, `validate` | `packer init .`<br>`packer build -color=false -timestamp-ui -var-file=<job>/vars.pkrvars.json .` |
| `ansible` / `playbook`, `check` | `ansible-playbook -i <inventory> --limit=<x> --extra-vars=@<job>/extra-vars.json <playbook>` (`check` adds `--check --diff`) |
| `command` / `run` | exactly the `command:` list, e.g. `/usr/bin/uptime --pretty` |

Where caller values end up:

- `params` declares what a caller may send, each with a type: `enum`, `string` (must have a
  `pattern`, anchored automatically), `int` (optional `min`/`max`) or `bool`. Unknown params
  are rejected.
- `vars` (terraform, packer, ansible) are templates like `"{{ .region }}"`. They are written
  to a JSON var file, never put on the command line.
- `args` (ansible only) fill `--limit`, `--tags` or `--skip-tags`.
- `command` elements (command tool only) may be templates. Each element becomes exactly one
  argv entry, so a value with spaces stays one argument. The first element must be a literal
  absolute path, so a parameter can never choose which program runs.

The tool's environment is built from scratch: `PATH`, `HOME=<data-dir>/home` and a few
`TF_*`/`ANSIBLE_*` settings. Anything else must be listed in the action's `env_profile`:

```yaml
env_profiles:
  aws-prod:
    pass_through: [AWS_REGION]                            # copied from the server environment
    secrets: [AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY]   # copied, and scrubbed from logs
    set: { TF_LOG: "" }                                   # fixed values
```

## 3. Adding and allowing a new command: `uptime`

Say you want workflows in `cloudyhome/infra` to be able to see how long the host has been up.

### Step 1: find the absolute path of the binary

On the machine (or in the image) where the control plane runs:

```sh
command -v uptime      # /usr/bin/uptime on Arch and Debian
```

The path must be absolute. The server refuses to start if `command[0]` is relative.

### Step 2: add the action to the catalog

```yaml
actions:
  host.uptime:
    description: Show how long the control plane host has been up
    tool: command
    command: [/usr/bin/uptime, --pretty]
    timeout: 1m
    allow:
      - repository: cloudyhome/infra
```

What each line does:

- `host.uptime` is the name the runner uses. Pick any `group.verb` style name.
- `tool: command` means run a fixed argv. `op` defaults to `run`.
- `command` is the exact argv. No shell.
- There is no `repo`, so nothing is checked out and the command runs in an empty job
  directory. Callers cannot pass a `ref` to this action.
- `timeout` makes the job get SIGINT, then SIGKILL after `server.cancel_grace` (60s by default).
- `allow` lists who may call it. At least one rule is required. With no rules nobody can
  call it, and an empty rule is rejected at startup.

### Step 3: decide who is allowed

Each rule is a set of token claims that must **all** match. If **any** rule matches, the
call is allowed. Values can be one string or a list, and `*` matches anything (including `/`).

```yaml
allow:
  # any workflow in cloudyhome/infra
  - repository: cloudyhome/infra

  # only from main, in a job that uses the `production` environment
  - repository: cloudyhome/infra
    ref: refs/heads/main
    environment: production

  # several repos, only on push or manual runs
  - repository: [cloudyhome/app, cloudyhome/web-*]
    event_name: [push, workflow_dispatch]

  # only when called from one specific reusable workflow
  - job_workflow_ref: "cloudyhome/infra/.github/workflows/ops.yml@refs/heads/main"
```

Claims you can match on: `repository`, `repository_owner`, `ref`, `ref_type`, `event_name`,
`environment`, `workflow_ref`, `job_workflow_ref`, `actor`, `sub`.

Tip: `environment` is the strongest gate. It is only in the token when the job declares
`environment: production`, and GitHub's environment protection rules (required reviewers,
branch restrictions) apply before the job gets a token.

Separately, `server.allowed_org: cloudyhome` rejects every token from outside the org before
any rule is checked.

### Step 4 (optional): add parameters

A variant that takes a validated argument:

```yaml
  host.disk:
    description: Show free space on one mount point
    tool: command
    command: [/usr/bin/df, -h, "{{ .mount }}"]
    params:
      mount: { type: enum, values: [/, /tmp], default: / }
    allow:
      - repository: cloudyhome/infra
        environment: production
```

The caller sends `mount=/tmp`. Anything outside the enum gets HTTP 422. Prefer `enum`. If
you must use `string`, give it a tight `pattern`, and don't let it start with `-` unless the
program handles that safely.

### Step 5: validate the catalog

```sh
controlplane -config catalog.yml -check
# catalog.yml: 5 actions OK
```

Every mistake (unknown field, relative path, template using an undeclared param, missing
`allow`) is reported with the action name. The systemd unit runs this check before every start.

### Step 6: make sure the binary exists where the server runs

In the container that means the image must have it. `uptime` comes from `procps`, which is
already in `deploy/Containerfile`. For other commands, add their package to the
`apt-get install` line and rebuild (see section 5).

### Step 7: reload

The catalog is read at startup. Restart the service (`systemctl restart controlplane`, or
restart the container). Jobs running at that moment get SIGINT and are marked cancelled.

## 4. Testing by hand on your machine (no GitHub)

For local testing the server has `-insecure-dev-auth`. It accepts an **unsigned** token that
is just base64url JSON claims, so you can pretend to be any repository or environment. It
only starts on a loopback address.

### Step 1: build

```sh
cd /path/to/controlplane
go build -o bin/controlplane ./cmd/controlplane
go build -o bin/cpctl ./cmd/cpctl
```

### Step 2: start the server with the dev catalog

`examples/dev-catalog.yml` has `host.uptime` and `host.disk` from section 3.

```sh
./bin/controlplane -config examples/dev-catalog.yml -data-dir ./data \
  -listen 127.0.0.1:8080 -insecure-dev-auth
```

Leave it running and use a second terminal.

### Step 3: make a token

```sh
export CONTROLPLANE_URL=http://127.0.0.1:8080
export CONTROLPLANE_TOKEN=$(./bin/cpctl dev-token repository=cloudyhome/infra ref=refs/heads/main)
```

`dev-token` takes any `claim=value` pairs and fills in `repository_owner` from `repository`.

### Step 4: run it with cpctl

```sh
./bin/cpctl actions                 # what this identity is allowed to call
./bin/cpctl run host.uptime         # submit, stream the log, exit with the job's code
echo $?                             # 0
```

Expected output:

```
job d6afe8f282e99935d4919e51: host.uptime
[controlplane] action host.uptime
[controlplane] $ /usr/bin/uptime --pretty
up 3 days, 4 hours, 12 minutes
[controlplane] job succeeded
job d6afe8f282e99935d4919e51 succeeded
```

### Step 5: check that the rules actually block things

```sh
# host.disk needs environment=production: expect HTTP 403
./bin/cpctl run -p mount=/ host.disk

# with the environment claim it works
CONTROLPLANE_TOKEN=$(./bin/cpctl dev-token repository=cloudyhome/infra environment=production) \
  ./bin/cpctl run -p mount=/tmp host.disk

# value outside the enum: expect HTTP 422
CONTROLPLANE_TOKEN=$(./bin/cpctl dev-token repository=cloudyhome/infra environment=production) \
  ./bin/cpctl run -p mount=/etc host.disk

# another org: expect HTTP 401
CONTROLPLANE_TOKEN=$(./bin/cpctl dev-token repository=evil/x) ./bin/cpctl actions
```

### Step 6: the same thing with plain curl

This is exactly what `cpctl` does under the hood:

```sh
TOKEN=$(./bin/cpctl dev-token repository=cloudyhome/infra)
H="Authorization: Bearer $TOKEN"

curl -s http://127.0.0.1:8080/healthz
curl -s -H "$H" http://127.0.0.1:8080/v1/actions

# submit; returns {"id": "...", "status": "queued", ...}
curl -s -X POST -H "$H" -H 'Content-Type: application/json' \
  -d '{}' http://127.0.0.1:8080/v1/actions/host.uptime/jobs

# with params:  -d '{"params": {"mount": "/tmp"}}'
# repo actions: -d '{"ref": "refs/heads/main", "params": {...}}'
# apply:        -d '{"plan_job_id": "<id of a succeeded plan job>"}'

JOB=<id from above>
curl -s -H "$H" http://127.0.0.1:8080/v1/jobs/$JOB
curl -s -i -H "$H" "http://127.0.0.1:8080/v1/jobs/$JOB/logs?offset=0"
#   X-Next-Offset: pass this as ?offset= on the next call
#   X-Job-Status:  once this is done and the body is empty, you have the whole log
curl -s -X POST -H "$H" http://127.0.0.1:8080/v1/jobs/$JOB/cancel
```

### Step 7: look at what was recorded

```sh
sqlite3 data/controlplane.db 'select id, action, status, exit_code from jobs order by created_at desc limit 5'
sqlite3 data/controlplane.db 'select ts, repository, action, decision, reason from audit order by id desc limit 10'
cat data/jobs/<id>/log
```

### Testing a repo-based action locally

For terraform, packer, ansible, or a `command` with a `repo`, point a repo at a local git
directory. The server fetches it like a remote:

```yaml
repos:
  infra:
    url: /home/you/src/infra          # local path works; ssh URL in production
actions:
  web.check:
    tool: ansible
    op: check
    repo: infra
    dir: ansible
    playbook: site.yml
    inventory: inventories/dev
    allowed_refs: ["refs/heads/*"]
    allow: [{ repository: cloudyhome/app }]
```

Commit your changes first. The server runs committed code at the resolved commit, not your
working tree.

## 5. Building and running the container

### Step 1: build the image

From the repo root:

```sh
podman build -f deploy/Containerfile -t controlplane:latest .
# or: docker build -f deploy/Containerfile -t controlplane:latest .
```

The image has the `controlplane` binary, terraform and packer (versions pinned by the
`TERRAFORM_VERSION`/`PACKER_VERSION` build args), ansible-core, git, ssh and procps.
To change versions:

```sh
podman build -f deploy/Containerfile --build-arg TERRAFORM_VERSION=1.13.3 -t controlplane:latest .
```

Make sure the `tools:` paths in your catalog match the image: `/usr/local/bin/terraform`,
`/usr/local/bin/packer`, `/usr/bin/ansible-playbook`.

### Step 2: prepare the host directories

```sh
sudo mkdir -p /etc/controlplane/keys /var/lib/controlplane
sudo cp catalog.yml /etc/controlplane/catalog.yml
```

Deploy key, for repo-based actions. Create a read-only deploy key for each repo on GitHub
(repo Settings > Deploy keys, leave "Allow write access" off):

```sh
sudo ssh-keygen -t ed25519 -N '' -C controlplane -f /etc/controlplane/keys/infra
sudo cat /etc/controlplane/keys/infra.pub      # paste into GitHub as a deploy key
ssh-keyscan github.com | sudo tee /etc/controlplane/known_hosts
```

Credentials for tools go in an env file that only root can read:

```sh
sudo install -m 600 /dev/null /etc/controlplane/env
sudoedit /etc/controlplane/env
#   AWS_ACCESS_KEY_ID=...
#   AWS_SECRET_ACCESS_KEY=...
```

A variable only reaches a tool if an `env_profile` used by that action lists it.

The image runs as a non-root `controlplane` user, so the data directory and keys must be
readable by that UID. With rootless podman use `--userns=keep-id` or `chown` them to match.

### Step 3: validate the catalog with the image

```sh
podman run --rm -v /etc/controlplane:/etc/controlplane:ro,Z \
  --entrypoint /usr/local/bin/controlplane controlplane:latest \
  -config /etc/controlplane/catalog.yml -check
```

### Step 4: run it

```sh
podman run -d --name controlplane --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -v /etc/controlplane:/etc/controlplane:ro,Z \
  -v /var/lib/controlplane:/var/lib/controlplane:Z \
  --env-file /etc/controlplane/env \
  --stop-timeout 90 \
  controlplane:latest
```

- The port is published on 127.0.0.1 only. Put a TLS reverse proxy in front (next step).
- `--stop-timeout 90` gives terraform time to release state locks when the container stops.
- Commands run inside the container, so `uptime` reports the host kernel's uptime but `df`
  shows the container's filesystems. Mount what a command needs to see.

Without a container, `deploy/controlplane.service` is a hardened systemd unit for the same
binary: install it to `/etc/systemd/system/`, create a `controlplane` user, then
`systemctl enable --now controlplane`.

### Step 5: expose it over HTTPS

GitHub-hosted runners call the server from the internet, so it needs a public HTTPS name.
Example with Caddy (automatic TLS):

```
controlplane.cloudyhome.example {
    reverse_proxy 127.0.0.1:8080
}
```

If you'd rather not expose it, use self-hosted runners on the same network and point
`CONTROLPLANE_URL` at the internal address. Only `/healthz` is unauthenticated.

### Step 6: set the audience

In `catalog.yml`:

```yaml
server:
  oidc_audience: https://controlplane.cloudyhome.example   # any string; must match the workflow
  allowed_org: cloudyhome
```

Workflows must ask GitHub for a token with this exact audience (the `audience` input below).
This stops a token minted for another service from being replayed here.

## 6. Calling it from a GitHub Actions workflow

### How the composite action works

`action/action.yml` in this repo:

1. Sets up Go (from this repo's `go.mod`).
2. Builds `cpctl` from the action's own checkout.
3. Runs `cpctl run <action>` with your inputs. `cpctl`:
   - asks the runner for an OIDC token (`ACTIONS_ID_TOKEN_REQUEST_URL`, needs
     `permissions: id-token: write`) with your `audience`,
   - submits the job with an `Idempotency-Key` of run id, attempt, job and action, so a
     retried step does not start a second job,
   - streams the log into the step output,
   - writes `job_id` and `status` to the step outputs,
   - exits with the job's exit code, so a failed job fails the step,
   - on workflow cancel (SIGINT), cancels the job on the server.

Inputs: `server`, `audience`, `action`, `params` (one `key=value` per line), `ref`,
`plan-job`. Outputs: `job_id`, `status`.

For a line-by-line walkthrough of the `uses:` step, see `github-action-uses.md` in this folder.

### Step 1: let other repos use the action

If `cloudyhome/controlplane` is private: in its GitHub settings, go to Actions > General >
Access and choose "Accessible from repositories in the 'cloudyhome' organization".

### Step 2: add a workflow to the calling repo

`cloudyhome/infra/.github/workflows/uptime.yml`:

```yaml
name: uptime
on:
  workflow_dispatch:

permissions:
  id-token: write     # required: lets the job request an OIDC token
  contents: read

jobs:
  uptime:
    runs-on: ubuntu-latest
    steps:
      - uses: cloudyhome/controlplane/action@main
        with:
          server: https://controlplane.cloudyhome.example
          audience: https://controlplane.cloudyhome.example
          action: host.uptime
```

With params and an environment gate (for `host.disk`):

```yaml
jobs:
  disk:
    runs-on: ubuntu-latest
    environment: production        # puts environment=production in the token
    steps:
      - uses: cloudyhome/controlplane/action@main
        with:
          server: https://controlplane.cloudyhome.example
          audience: https://controlplane.cloudyhome.example
          action: host.disk
          params: |
            mount=/tmp
```

### Step 3: run it

Actions tab > uptime > Run workflow. The step log shows the command output. If the caller
isn't allowed, the step fails with `HTTP 403 forbidden: caller may not invoke host.uptime`.

### Terraform plan then apply

`examples/workflows/network.yml` shows the full pattern: a `plan` job on every push and PR,
and an `apply` job on main that runs in the `production` environment and passes the plan's
`job_id` as `plan-job`. The server then:

- applies the saved plan file, at the plan's commit and params (the apply action takes no params),
- refuses if the plan failed, is older than `server.plan_max_age` (24h), or was already applied,
- refuses if the apply token's `ref` differs from the plan's ref (`require_ref_match`, on by
  default for apply), so a PR workflow can't apply main.

Before applying, reviewers can read the plan at `GET /v1/jobs/<plan id>/artifacts/plan.json`.

## 7. Troubleshooting

| Symptom | Cause |
|---|---|
| `no OIDC token available` | Workflow is missing `permissions: id-token: write`. |
| `401 invalid token` | Audience mismatch between workflow and `server.oidc_audience`, repo outside `allowed_org`, or an expired token. The server log says which. |
| `403 forbidden` | No `allow` rule matched. Compare with `select * from audit order by id desc limit 5`, which records repository and subject. A common miss: `environment` is only in the token if the job declares `environment:`. |
| `403 ref_mismatch` | Apply requested from a token whose `ref` differs from the plan's ref. |
| `422 invalid_params` | Value outside the enum or pattern, missing required param, or unknown param. |
| `422 ref_not_allowed` / `ref_unresolved` | Ref not in `allowed_refs`, or the server can't fetch it (deploy key, known_hosts, branch name). |
| `409 plan_*` | Plan failed, expired, already applied, or belongs to another action. |
| Job `failed`, exit code N | The tool itself failed. Read the job log. |
| Job `timed_out` | Raise the action's `timeout`. |
| Job `failed: interrupted by server restart` | The server restarted while the job was queued or running. Jobs are never re-run automatically. |
| Server won't start | Run `controlplane -config catalog.yml -check`. It names the action and the problem. |
