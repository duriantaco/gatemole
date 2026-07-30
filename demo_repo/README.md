# Demo Repo

This directory is a tiny password-reset fixture for the Gatemole compiler MVP.

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
gatemole --repo demo_repo compile
gatemole --repo demo_repo compile --emit ir
gatemole --repo demo_repo evidence import junit artifacts/junit-pass.xml
GITHUB_STEP_SUMMARY=/tmp/gatemole-summary.md gatemole --repo demo_repo gate --github-summary || true
gatemole intent parse --intent demo_repo/.gatemole/intents/auth.password_reset.yaml --out /tmp/auth.password_reset.ast.json
gatemole intent compile --intent demo_repo/.gatemole/intents/auth.password_reset.yaml --out /tmp/auth.password_reset.json
gatemole ir build --spec demo_repo/.gatemole/specs/auth.password_reset.json --out /tmp/auth.password_reset.ir.json
gatemole plan build --spec demo_repo/.gatemole/specs/auth.password_reset.json --manifest demo_repo/.gatemole/manifests/pass.json --out /tmp/auth.password_reset.plan.json
gatemole artifacts build --spec demo_repo/.gatemole/specs/auth.password_reset.json --out /tmp/gatemole-artifacts
gatemole --repo demo_repo spec lint
gatemole --repo demo_repo --manifest demo_repo/.gatemole/manifests/blocked.json evidence
gatemole --repo demo_repo --manifest demo_repo/.gatemole/manifests/pass.json evidence
```

Expected gate results:

| Manifest | Decision |
| --- | --- |
| `blocked.json` | `block` |
| `pass.json` | `canary` |

The local tests can pass while Gatemole still blocks if security, runtime, or rollback evidence is missing. That is the point of the demo: test results are evidence for required-test obligations, not proof that every release obligation is covered.
