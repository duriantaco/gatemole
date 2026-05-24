# GitHub Actions

This repo uses GitHub Actions for two things:

1. publishing the Vouch docs site to GitHub Pages
2. running the current Vouch gate in pull request workflows

Published docs URL:

```text
https://duriantaco.github.io/vouch/
```

GitHub Pages is configured with `build_type: workflow`, so content is published
by `.github/workflows/pages.yml` after changes land on `main`.

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
docs/site/
docs/site/assets/vouch.png
```

The workflow builds the MkDocs site into `_site/`, uploads that artifact, and
deploys it to the `github-pages` environment.

Official references:

- [Configuring a publishing source for your GitHub Pages site](https://docs.github.com/en/pages/getting-started-with-github-pages/configuring-a-publishing-source-for-your-github-pages-site)
- [actions/upload-pages-artifact](https://github.com/actions/upload-pages-artifact)
- [actions/deploy-pages](https://github.com/actions/deploy-pages)

## Vouch PR Workflow

The current Vouch PR workflow should start in shadow mode. It creates a PR
manifest from the diff, attaches evidence artifacts, appends a job summary, and
uploads the manifest/build/evidence bundle without blocking the PR.

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
        run: vouch compile

      - name: Create PR manifest
        env:
          VOUCH_TASK_ID: pr-${{ github.event.pull_request.number }}
          VOUCH_TASK_SUMMARY: ${{ github.event.pull_request.title }}
          VOUCH_RUN_ID: ${{ github.run_id }}.${{ github.run_attempt }}
          VOUCH_RUNNER_IDENTITY: https://github.com/${{ github.repository }}/.github/workflows/vouch.yml@${{ github.ref }}
        run: |
          vouch manifest create \
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
          vouch manifest attach-artifact \
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

Keep normal CI test jobs in place during shadow mode. This workflow measures
release-contract coverage; it should not be the only test enforcement path until
the team switches to enforced mode.

Upload these paths for every shadow run:

- `.vouch/manifests/`: PR manifest and attached artifact references.
- `.vouch/build/`: compiler outputs and `gate-result.json`.
- `.vouch/artifacts/`: raw evidence such as JUnit XML or SARIF.

## Code References

- CLI command and `--github-summary` flag:
  [`internal/vouch/cli.go`](https://github.com/duriantaco/vouch/blob/main/internal/vouch/cli.go)
- `$GITHUB_STEP_SUMMARY` handling:
  [`appendGitHubSummary`](https://github.com/duriantaco/vouch/blob/main/internal/vouch/cli.go)
- Markdown summary rendering:
  [`RenderGitHubSummary`](https://github.com/duriantaco/vouch/blob/main/internal/vouch/render.go)
- Gate result JSON:
  [`GateResultFromEvidence`](https://github.com/duriantaco/vouch/blob/main/internal/vouch/render.go)
- Default release policy:
  [`DefaultReleasePolicy`](https://github.com/duriantaco/vouch/blob/main/internal/vouch/policy.go)

## Enforced Mode

After a shadow-mode pilot, remove job-level `continue-on-error: true`. The CLI
exits non-zero only when the final decision is `block`. If the test evidence
step records a non-zero exit, the workflow does not attach the JUnit artifact,
so the gate reports missing required-test evidence instead of treating failed
tests as passing evidence.
