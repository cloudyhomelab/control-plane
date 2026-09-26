# controlplane

A REST API that GitHub Actions runners call to run terraform, packer, ansible or any other
command. Runners never execute anything themselves: they can only invoke named actions from
a YAML catalog, with typed parameters, authenticated by GitHub OIDC tokens.

## How it works

- **Catalog** (`examples/catalog.yml`): each action lists `steps`, argv lists run in order
  (e.g. `[/usr/local/bin/terraform, plan, "-var=region={{ .region }}"]`). It also names an
  optional repo and directory, typed `params`, which refs it may run, and `allow` rules over
  OIDC claims. The catalog owner writes each command exactly; the server adds nothing.
- **Auth**: the runner sends its GitHub OIDC token. The server checks signature, issuer,
  audience and `repository_owner`, then matches the action's `allow` rules.
- **Code**: the server clones the repo with its own deploy key and runs the exact commit
  the ref resolved to at submit time. Callers only choose among `allowed_refs`.
- **Jobs**: `POST` returns a job id immediately; the runner polls status and log. Jobs with
  the same `lock` run one at a time. Cancel sends SIGINT to the tool's process group.
- **Chaining jobs**: an action with `input_from: <action>` takes an `input_job_id`. The server
  checks that job is a succeeded run of that action from the same repo, runs at its commit
  with its params, and exposes its directory as `{{ .input_dir }}`. That is how terraform
  apply reads the plan file a plan job wrote to `{{ .job_dir }}/tfplan`.
- **Approvers**: an action may list `approvers` (GitHub logins). Approval still happens in
  GitHub, through the environment gate on the calling job. The server reads that workflow
  run's approvals from the GitHub API and refuses the job unless one of the listed people
  approved the caller's environment. So the catalog decides who approves, whatever reviewers
  the calling repository configured.

Safety rules: no shell anywhere (argv only), each template fills exactly one argv element,
the first element of every step is a literal absolute path, unknown params are rejected,
`string` params must have an anchored pattern, the child environment is built from scratch,
and values of `secrets` env vars are scrubbed from logs.

## Writing your catalog

`examples/catalog.yml` is a realistic example, not something to deploy as is. Write your own
catalog (by default the server reads `/etc/controlplane/catalog.yml`) with your repos, actions
and allow rules, and check it with `controlplane -config catalog.yml -check`.

### Adding a repository

Actions that run code from git refer to an entry under `repos:` by its name:

```yaml
repos:
  myrepo:                        # local name, used as `repo: myrepo` in actions
    url: ...                     # where the server fetches the code from
    default_ref: refs/heads/main # optional; used when a caller sends no ref (default: refs/heads/main)

actions:
  some.action:
    repo: myrepo
    allowed_refs: ["refs/heads/main"]
    ...
```

The server fetches the repo itself, with its own access, and runs the exact commit it
resolved. The runner never sends code. Which kind of entry you need depends on whether the
repo is public.

#### Public repository

Use the HTTPS URL. That's all: no credentials, and TLS verifies that it is really github.com.

```yaml
repos:
  myrepo:
    url: https://github.com/OWNER/REPO.git
```

Check that the server host can reach it:

```sh
git ls-remote https://github.com/OWNER/REPO.git refs/heads/main
```

If the repo later becomes private, fetches fail with `422 ref_unresolved` (the server never
waits for a password prompt). Switch to a private entry then.

#### Private repository

Use the SSH URL with a **deploy key**: an SSH key that GitHub attaches to a single repo,
read-only. The server can then read that repo and nothing else.

1. Create a key pair on the control plane host, one per repo:

   ```sh
   sudo mkdir -p /etc/controlplane/keys
   sudo ssh-keygen -t ed25519 -N '' -C controlplane-REPO -f /etc/controlplane/keys/REPO
   ```

2. Add the public half to the repo on GitHub: **Settings > Deploy keys > Add deploy key**.
   Paste the contents of `/etc/controlplane/keys/REPO.pub` and leave **Allow write access** off.

3. Record GitHub's SSH host key, so the server refuses anything pretending to be github.com.
   This is shared by all private repos:

   ```sh
   ssh-keyscan github.com | sudo tee /etc/controlplane/known_hosts
   ```

   Compare the fingerprints with the ones GitHub publishes
   (docs.github.com, "GitHub's SSH key fingerprints") before trusting them.

4. Let only the service user read the private key. ssh refuses keys that others can read:

   ```sh
   sudo chown controlplane: /etc/controlplane/keys/REPO
   sudo chmod 600 /etc/controlplane/keys/REPO
   ```

5. Add the entry:

   ```yaml
   repos:
     myrepo:
       url: git@github.com:OWNER/REPO.git
       deploy_key_file: /etc/controlplane/keys/REPO
       known_hosts_file: /etc/controlplane/known_hosts
   ```

6. Check access the same way the server will fetch:

   ```sh
   sudo -u controlplane env GIT_SSH_COMMAND="ssh -i /etc/controlplane/keys/REPO \
     -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=yes \
     -o UserKnownHostsFile=/etc/controlplane/known_hosts" \
     git ls-remote git@github.com:OWNER/REPO.git refs/heads/main
   ```

   A line with a commit hash means it works. `Permission denied (publickey)` means the deploy
   key is not on that repo, or the file isn't readable by the service user.

GitHub does not let one deploy key be used on two repos, so each private repo gets its own
key file.

#### What protects you in either case

What runs is whatever is on a ref in the action's `allowed_refs`. For a repo whose merges
trigger actions, protect those branches on GitHub (require pull request reviews, no direct
pushes). Keep credentials out of the repo; they belong in the server environment and reach a
command only through an `env_profile`.

## API

| Method | Path | |
|---|---|---|
| GET | `/v1/actions` | actions the caller may invoke, with param schemas |
| POST | `/v1/actions/{name}/jobs` | `{"ref", "params", "input_job_id"}` → 202 job |
| GET | `/v1/jobs/{id}` | job status |
| GET | `/v1/jobs/{id}/logs?offset=N` | raw log bytes; headers `X-Next-Offset`, `X-Job-Status` |
| POST | `/v1/jobs/{id}/cancel` | cancel a queued or running job |

Callers see only jobs from their own repository, unless a `server.admins` rule matches.
Send `Idempotency-Key` to make retries safe (cpctl does this per workflow run attempt).

## Using it from a workflow

See `examples/workflows/homelab.yml`. The workflow needs `permissions: id-token: write`;
the `action/` composite builds `cpctl`, submits the job, streams the log and fails the step
if the job fails. Cancelling the workflow cancels the job.

## Running

```sh
go run ./cmd/controlplane -config examples/catalog.yml -check      # validate catalog
go run ./cmd/controlplane -config catalog.yml -data-dir ./data     # serve on 127.0.0.1:8080
```

Run it on the host with `deploy/controlplane.service`, not in a container: it exists for jobs
that need host access (packer qemu builds need `/dev/kvm`, docker builds need the docker daemon)
and so can't run on a containerised runner. Put it behind a TLS reverse proxy.
Cloud credentials go in the server environment (`/etc/controlplane/env`) and reach a command
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

`task test` (or `go test ./...`); `task check` runs everything CI does, `task fmt` formats. Job runs use the test binary as a fake terraform, so no tools are needed.
