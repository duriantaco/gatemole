# Demo Repo

This directory is a tiny password-reset fixture for the Vouch compiler MVP.

It contains a small app surface, one high-risk feature contract, and three agent-change manifests:

- `src/auth/password_reset.py`
- `tests/auth/test_password_reset.py`
- `CODEOWNERS`
- `.gatemole/intents/auth.password_reset.yaml`
- `.gatemole/specs/auth.password_reset.json`
- `.gatemole/manifests/blocked.json`
- `.gatemole/manifests/pass.json`
- `.gatemole/manifests/traceability-blocked.json`

Run from the parent directory:

```sh
vouch --repo demo_repo compile
vouch --repo demo_repo compile --emit ir
vouch --repo demo_repo evidence import junit artifacts/junit-pass.xml
GITHUB_STEP_SUMMARY=/tmp/gatemole-summary.md vouch --repo demo_repo gate --github-summary || true
vouch intent parse --intent demo_repo/.gatemole/intents/auth.password_reset.yaml --out /tmp/auth.password_reset.ast.json
vouch intent compile --intent demo_repo/.gatemole/intents/auth.password_reset.yaml --out /tmp/auth.password_reset.json
vouch ir build --spec demo_repo/.gatemole/specs/auth.password_reset.json --out /tmp/auth.password_reset.ir.json
vouch plan build --spec demo_repo/.gatemole/specs/auth.password_reset.json --manifest demo_repo/.gatemole/manifests/pass.json --out /tmp/auth.password_reset.plan.json
vouch artifacts build --spec demo_repo/.gatemole/specs/auth.password_reset.json --out /tmp/gatemole-artifacts
vouch --repo demo_repo spec lint
vouch --repo demo_repo --manifest demo_repo/.gatemole/manifests/blocked.json evidence
vouch --repo demo_repo --manifest demo_repo/.gatemole/manifests/pass.json evidence
```

Expected gate results:

| Manifest | Decision |
| --- | --- |
| `blocked.json` | `block` |
| `pass.json` | `canary` |

The local tests can pass while Vouch still blocks if security, runtime, or rollback evidence is missing. That is the point of the demo: test results are evidence for required-test obligations, not proof that every release obligation is covered.
