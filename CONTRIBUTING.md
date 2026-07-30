# Contributing

Thanks for helping build Vouch Runtime.

## Product first principle

The current Vouch Runtime provides transaction and outcome control inside a
narrow local-Git boundary. A contribution should strengthen the controlled path
from task intent to isolated execution, staged effects, exact-state
verification, authority, commit and recovery.

Keep the hierarchy clear:

- Vouch is the company and product name.
- Vouch Agent OS is the complete architecture: Control Plane plus Runtime
  fleet, transaction protocol and connector model.
- Vouch Runtime is the first sellable product and customer-side enforcement
  boundary.
- Vouch Developer Runtime is the local experience around one Runtime, using the
  same kernel rather than a separate developer-only enforcement path.
- `gatemoled` is the trusted transaction kernel inside each Runtime.
- Vouch Contracts is an optional verification module.
- Vouch Control Plane is the future central fleet, policy, approval and audit
  manager; it does not execute downstream agent actions.

Vouch is not a coding agent, generic AI code reviewer, identity provider or
universal rollback system.

## Start locally

Run the baseline checks:

```sh
go test ./...
go vet ./...
```

Build the runtime:

```sh
go install ./cmd/vouch ./cmd/gatemoled ./cmd/gatemole-model-broker
```

If the binaries are not on `PATH`:

```sh
export PATH="$(go env GOPATH)/bin:$PATH"
```

The runtime acceptance paths are separate so failures retain a useful scope:

```sh
scripts/vouchkernelbench.sh --out /tmp/vouchkernelbench
scripts/vouchtransactionbench.sh
scripts/vouchruntimebench.sh
```

The OCI production acceptance requires a working Docker-compatible engine:

```sh
image="$(scripts/vouchproductionfixture.sh --tag gatemole-production-fixture:acceptance)"
GATEMOLE_PRODUCTION_IMAGE="$image" scripts/vouchproductionbench.sh
```

The optional Contracts module has its own regression harness:

```sh
scripts/vouchbench.sh --out /tmp/vouchbench
```

## Runtime work areas

### Developer API and task experience

Useful work includes:

- A task-oriented CLI over the transaction state machine.
- Public versioned API types, OpenAPI and supported SDKs.
- Idempotent create/mutation requests and status streaming.
- A stable task envelope passed to agent images.
- Runtime configuration, diagnostics, packaging and upgrades.

Clients should not need to author event hashes, replay state or duplicate kernel
authority logic.

### Isolation and execution

Useful work includes:

- Agent adapters that preserve the daemon-owned boundary.
- OCI or microVM hardening.
- Resource, output, storage and concurrency bounds.
- Model/tool credential brokering and egress controls.
- Checkpoint, cancellation and workload reconciliation.

Do not label a path mediated when an agent retains an ambient bypass.

### Transactions and effects

Useful work includes:

- Typed effect normalization.
- Deterministic sequence and separation-of-duty policy.
- Immutable staging and exact verifier inputs.
- Idempotent commit, receipts and reconciliation.
- Honest partial-commit and manual-recovery semantics.
- Deep connector drivers with tested failure boundaries.

Unknown non-idempotent effects must be reconciled, never blindly retried.

### Identity, approval and audit

Useful work includes:

- Identity-provider integration and key rotation.
- Outcome-oriented approval packages.
- Authority delegation, expiry and revocation.
- Tamper-evident audit export and retention interfaces.
- Operator recovery and incident tooling.

An agent or verifier cannot authorize its own protected result.

### Connectors

Prefer one deep connector to many shallow wrappers. A connector should define:

- What can be staged.
- The exact expected resource version.
- Commit ordering and idempotency.
- Receipts and reconciliation.
- Whether recovery is reversible, compensatable or manual.
- Tests for crashes and ambiguous outcomes.

Remote Git/GitHub is first, followed by Kubernetes and PostgreSQL.

## Vouch Contracts contributions

The Contracts module under `internal/vouch/` remains useful as optional
verification policy. Its most valuable work connects it to runtime authority:

- Compile obligations into daemon-owned verifier requirements.
- Bind evidence and obligation coverage to exact staged state.
- Add SARIF, coverage, deployment, metric and rollback evidence.
- Publish schemas and compatibility fixtures.
- Improve contract authoring where runtime pilots show real friction.

AI verifiers may verify evidence against obligations. They must not be
presented as a generic code-review replacement or the sole authority for a
high-impact effect.

## Contribution standards

Keep changes narrow and testable.

For code changes:

- Add or update tests for changed behavior.
- Keep authoritative behavior deterministic.
- Use strict versioned resources at public boundaries.
- Preserve append-before-execute and receipt-after-execute invariants.
- Bound untrusted inputs, outputs and resource consumption.
- Test restart, conflict, tamper and cancellation behavior when applicable.
- Avoid unrelated refactors and preserve compatibility intentionally.

For documentation:

- Lead with Vouch Runtime, not the Contracts compiler.
- Distinguish implemented, supported and planned behavior.
- Preserve the single-node/single-tenant/local-Git production limits.
- Do not claim arbitrary code correctness, universal rollback, multi-tenancy or
  HA.
- Include runnable commands and state their prerequisites.

## Good first contributions

- Improve transaction CLI output or usage text.
- Add a strict schema fixture.
- Add a runtime or recovery regression test.
- Improve operator diagnostics without leaking sensitive data.
- Document one public resource or state transition.
- Add a verifier-profile example.
- Add a Contracts evidence importer with deterministic fixtures.

## Changes to avoid

Avoid changes that:

- Move agent reasoning into the kernel.
- Treat telemetry as enforcement.
- Introduce a privileged direct path around the broker.
- Hide ambiguous or partially committed state.
- Depend on nondeterministic model output for authority.
- Add a connector without reconciliation semantics.
- Present Vouch Contracts as the whole product.
- Call the current implementation a complete Vouch Agent OS.

The project should be ambitious about enforcement and conservative about its
claims.

## License

By contributing to Vouch, you agree that your contributions are licensed under
the Apache License, Version 2.0.
