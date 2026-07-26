# Local Agent Kernel

Vouch includes a single-machine agent control plane. This document describes
the lower-level run/action kernel; the production transaction runtime adds the
OCI sandbox, OIDC authorization, signed approvals, model broker, verification,
and Git commit coordinator described in
[Production Runtime Operations](PRODUCTION.md). The kernel is the durable
process and authority substrate beneath
[Agent Transaction Control](../AGENT_TRANSACTION_EXECUTION_PLAN.md), not a
claim that the full operating system is finished.

The implemented path is:

```text
ExecutionContract
  -> time-bounded CapabilityGrant
  -> ActionRequest
  -> durable request and authorization events
  -> rooted filesystem driver
  -> committed | failed | unknown receipt
```

The existing release-contract compiler and gate remain independent and keep
their existing CLI behavior.

## What is implemented

- Eight strict `vouch.*.v0` kernel resource schemas and Go models.
- Deterministic run lifecycle reducer and stable kernel error codes.
- Append-only hash-chained events plus byte-replayable materialized run state.
- SQLite transactions, optimistic event cursors, namespace predicates, and
  restart recovery.
- `vouchd` over a mode-`0600` Unix socket and `vouch daemon` as a convenience.
- Run create, inspect, list, event, pause, resume, cancel, and capability
  installation commands.
- Execution-contract compilation into stable, expiring capability grants.
- Typed filesystem read/write actions with output limits and workspace
  containment.
- Durable `requested -> authorized -> executing -> committed` action history,
  including denied requests.
- Startup conversion of interrupted authorized/executing actions to `unknown`;
  recovery never retries the effect.

Filesystem access uses Go's [`os.Root`](https://pkg.go.dev/os#Root) API. Operations are resolved beneath an
already-open repository and workspace root, including symlink traversal checks.
The driver writes through a synced temporary file and an in-root atomic rename.

## Local walkthrough

Start the daemon in one terminal:

```sh
vouch --repo /path/to/repo daemon
```

An `AgentRun` JSON file pins an image digest, execution-contract digest,
namespace, run principal, workspace, deadline, and budgets. Create and admit
it, then make it runnable:

```sh
vouch --repo /path/to/repo run create --file run.json
vouch --repo /path/to/repo run transition --namespace demo --id run:demo --to admitted
vouch --repo /path/to/repo run transition --namespace demo --id run:demo --to running
```

Compile the pinned `ExecutionContract` into grants:

```sh
vouch --repo /path/to/repo run grant \
  --namespace demo --id run:demo --contract execution-contract.json
```

All filesystem paths are logical paths rooted by the contract, such as
`workspace/output.txt`:

```sh
vouch --repo /path/to/repo action fs-write \
  --namespace demo --run run:demo \
  --path workspace/output.txt --input ./candidate-output.txt

vouch --repo /path/to/repo --json action fs-read \
  --namespace demo --run run:demo \
  --path workspace/output.txt
```

Inspect the authoritative history:

```sh
vouch --repo /path/to/repo --json run events \
  --namespace demo --id run:demo
```

The client reads the run cursor before every transition or action. A competing
write returns `KERNEL_CONFLICT`; it is not resolved by last-write-wins.

## Acceptance harness

Run the kernel-specific acceptance path separately from the existing
VouchBench release suite:

```sh
scripts/vouchkernelbench.sh --out /tmp/vouchkernelbench
```

It builds both binaries, performs an allowed write, proves a path escape is
denied and audited, restarts the daemon, and compares the complete run
projection and event history before and after restart.

## Security boundary and current limitations

This lower-level run/action slice mediates only actions submitted to its
filesystem broker. It cannot stop a process that also has direct host
filesystem access. Use the production transaction runtime for untrusted agent
execution: it launches the agent in the daemon-owned OCI boundary, authenticates
operator requests, and removes direct model credentials.

Not yet implemented:

- MCP, GitHub API, database, or cloud action drivers.
- OIDC discovery/JWKS refresh, identity lifecycle provisioning, or a
  multi-tenant network control plane.
- Capability revocation events.
- Leases, schedulers, or distributed stores.
- Portable canonical JSON signatures; the current event digest is a local v0
  tamper-evidence format.

See the [kernel boundary ADR](architecture/ADR-001-agent-kernel-boundary.md),
[threat model](architecture/THREAT_MODEL.md), and
[kernel semantics](architecture/KERNEL_SEMANTICS.md) for the governing
invariants.

The next layer is documented in [Local Agent Transactions](TRANSACTIONS.md).
