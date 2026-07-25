<p align="center">
  <img src="../assets/vouch.png" alt="Vouch logo" width="180">
</p>

# GitHub Actions

This repo uses GitHub Actions for two things:

1. publishing the Vouch docs site to GitHub Pages
2. running the current Vouch gate in pull request workflows

Published docs URL:

https://duriantaco.github.io/vouch/

Pages is configured with `build_type: workflow`, so content is published by
`.github/workflows/pages.yml` after changes land on `main`.

## Docs Deployment

The Pages workflow follows GitHub's custom Actions publishing flow:

- `actions/configure-pages`
- `actions/upload-pages-artifact`
- `actions/deploy-pages`

Workflow file:

```text
.github/workflows/pages.yml
```

Source files:

```text
mkdocs.yml
docs/requirements.txt
docs/site/
docs/site/assets/vouch.png
```

The workflow builds the MkDocs site into `_site/`, uploads that artifact, and
deploys it to the `github-pages` environment.

Official references:

- https://docs.github.com/en/pages/getting-started-with-github-pages/configuring-a-publishing-source-for-your-github-pages-site
- https://github.com/actions/upload-pages-artifact
- https://github.com/actions/deploy-pages

## Vouch PR Workflow

The relevant code paths are:

- CLI command and `--github-summary` flag:
  [`internal/vouch/cli.go`](../internal/vouch/cli.go)
- `$GITHUB_STEP_SUMMARY` handling:
  [`appendGitHubSummary`](../internal/vouch/cli.go)
- Markdown summary rendering:
  [`RenderGitHubSummary`](../internal/vouch/render.go)
- Final gate result shape:
  [`GateResultFromEvidence`](../internal/vouch/render.go)
- Default release policy:
  [`DefaultReleasePolicy`](../internal/vouch/policy.go)

### Shadow Mode

Use this first. It creates a manifest for the pull request diff, attaches
evidence artifacts, writes the gate result to the job summary, and uploads the
manifest/build/evidence bundle for audit. The job is advisory because
`continue-on-error: true` is set at the job level.

```yaml
name: Vouch

on:
  pull_request:

permissions:
  contents: read

env:
  VOUCH_MANIFEST: .vouch/manifests/pr-${{ github.event.pull_request.number }}-${{ github.run_attempt }}.json
  VOUCH_GATE_RESULT: .vouch/build/gate-result.json
  VOUCH_JUNIT: .vouch/artifacts/pytest.xml

jobs:
  vouch:
    runs-on: ubuntu-latest
    continue-on-error: true
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - uses: actions/setup-go@v5
        with:
          go-version: "1.26"

      - name: Install Vouch
        run: go install github.com/duriantaco/vouch/cmd/vouch@latest

      - name: Compile Vouch contracts
        run: vouch contracts compile

      - name: Create PR manifest
        env:
          VOUCH_TASK_ID: pr-${{ github.event.pull_request.number }}
          VOUCH_TASK_SUMMARY: ${{ github.event.pull_request.title }}
          VOUCH_RUN_ID: ${{ github.run_id }}.${{ github.run_attempt }}
          VOUCH_RUNNER_IDENTITY: https://github.com/${{ github.repository }}/.github/workflows/vouch.yml@${{ github.ref }}
        run: |
          vouch contracts manifest create \
            --task-id "$VOUCH_TASK_ID" \
            --summary "$VOUCH_TASK_SUMMARY" \
            --agent github-actions \
            --run-id "$VOUCH_RUN_ID" \
            --runner-identity "$VOUCH_RUNNER_IDENTITY" \
            --runner-oidc-issuer https://token.actions.githubusercontent.com \
            --base "origin/${{ github.base_ref }}" \
            --head HEAD \
            --out "$VOUCH_MANIFEST"

      - name: Run tests for evidence
        id: tests
        run: |
          mkdir -p .vouch/artifacts
          set +e
          pytest --junitxml "$VOUCH_JUNIT"
          exit_code=$?
          echo "exit_code=$exit_code" >> "$GITHUB_OUTPUT"
          exit 0

      - name: Attach JUnit evidence
        if: steps.tests.outputs.exit_code == '0'
        run: |
          vouch contracts manifest attach-artifact \
            --manifest "$VOUCH_MANIFEST" \
            --id pytest \
            --kind test_coverage \
            --path "$VOUCH_JUNIT" \
            --producer github-actions \
            --command "pytest --junitxml $VOUCH_JUNIT" \
            --exit-code "${{ steps.tests.outputs.exit_code }}" \
            --out "$VOUCH_MANIFEST"

      - name: Gate PR
        run: vouch --manifest "$VOUCH_MANIFEST" gate --github-summary --out "$VOUCH_GATE_RESULT"

      - name: Upload Vouch artifacts
        if: always()
        uses: actions/upload-artifact@v4
        with:
          name: vouch-shadow-pr-${{ github.event.pull_request.number }}
          path: |
            .vouch/manifests/
            .vouch/build/
            .vouch/artifacts/
          if-no-files-found: ignore
```

Use shadow mode until reviewers agree that Vouch is catching real
release-readiness gaps and the false-block rate is acceptable. Keep normal CI
test jobs in place during this phase; this job is measuring the release
contract workflow.

### Enforced Mode

After a shadow-mode pilot, remove job-level `continue-on-error: true`:

```yaml
jobs:
  vouch:
    runs-on: ubuntu-latest
```

`vouch contracts gate` exits non-zero only when the release decision is `block`.
`human_escalation`, `canary`, and `auto_merge` are non-blocking process exits.
If the test evidence step records a non-zero exit, the workflow does not attach
the JUnit artifact, so the gate reports missing required-test evidence instead
of treating failed tests as passing evidence.

### What The Summary Shows

`gate --github-summary` appends a Markdown report to `$GITHUB_STEP_SUMMARY`.
The report includes:

- final decision
- risk
- obligation coverage
- policy path
- reasons
- invalid evidence artifacts
- component-level covered and missing obligations
- verifier findings and required fixes

The renderer is [`RenderGitHubSummary`](../internal/vouch/render.go).

### Artifact Convention

Upload the whole Vouch bundle for each shadow run:

- `.vouch/manifests/`: PR manifest and attached artifact references.
- `.vouch/build/`: compiler outputs and `gate-result.json`.
- `.vouch/artifacts/`: raw evidence such as JUnit XML or SARIF.

The compact gate result should be written to `.vouch/build/gate-result.json`.
That file uses `vouch.gate_result.v0` and is the stable input for future status
checks or GitHub Checks integrations.

### Contracts In CI

Do not silently generate new contracts in an enforced workflow. Commit reviewed
`.vouch/intents/*.yaml`, `.vouch/specs/*.json`, and
`.vouch/policy/release-policy.json`.

For pilots, it is acceptable to run:

```sh
vouch contracts bootstrap --review
```

Use generated contracts as scaffolding only. A human should edit owners, paths,
risk, behavior, security, runtime signals, and rollback expectations before
Vouch becomes an enforced gate.

### Signed Evidence

For stricter runs, use:

```sh
vouch contracts gate --require-signed --github-summary
```

The signed-evidence checks are wired through `CollectEvidenceWithOptions` and
artifact linking in [`internal/vouch/evidence.go`](../internal/vouch/evidence.go).
Allowed signers are loaded from `.vouch/config.json`.

Use this only after your runners are producing Vouch evidence bundles and cosign
signature bundles.
