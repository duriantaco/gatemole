# Local Agent Kernel

Gatemole Runtime includes a single-machine `gatemoled` authority kernel. This
document describes its lower-level run/action substrate; the supported
production Runtime path adds the OCI sandbox, OIDC authorization, signed
approvals, model broker, verification and Git commit coordinator described in
[Production Runtime Operations](PRODUCTION.md). This subsystem is not a claim
that the complete Gatemole Agent OS is finished.

The implemented path is:

```text
ExecutionContract
  -> time-bounded CapabilityGrant
  -> ActionRequest
  -> durable request and authorization events
  -> rooted filesystem connector driver
  -> committed | failed | unknown receipt
```

The existing release-contract compiler and gate remain independent and keep
their existing CLI behavior.

## What is implemented

- Eight strict `gatemole.*.v0` kernel resource schemas and Go models.
- Deterministic run lifecycle reducer and stable kernel error codes.
- Append-only hash-chained events plus byte-replayable materialized run state.
- SQLite transactions, optimistic event cursors, namespace predicates, and
  restart recovery.
- `gatemoled` over a mode-`0600` Unix socket and `gatemole daemon` as a convenience.
- Runtime-bound task admission that creates the transaction, paired run and
  capabilities atomically, plus run inspect, list, event, pause, resume and
  cancel commands. Pause, resume and cancel currently transition only the
  lower-level run record; they do not control a running production OCI
  workload. Raw run creation and grant installation remain embedded/unbound
  compatibility operations and configured `gatemoled` rejects them.
- Paired execution start, settlement and startup recovery across the admitted
  run and transaction ledgers. The run binds the exact active execution, then
  cumulatively charges elapsed wall time and verified model usage when that
  execution finishes or is recovered.
- Execution-contract compilation into stable, expiring capability grants.
- Typed filesystem read/write actions with output limits and workspace
  containment.
- Durable `requested -> authorized -> executing -> committed` action history,
  including denied requests.
- Startup conversion of interrupted authorized/executing actions to `unknown`;
  recovery never retries the effect.

Filesystem access uses Go's [`os.Root`](https://pkg.go.dev/os#Root) API. Operations are resolved beneath an
already-open repository and workspace root, including symlink traversal checks.
The connector driver writes through a synced temporary file and an in-root
atomic rename.

## Local walkthrough

Start from a Git repository with a `HEAD` commit and a `workspace/` directory,
then initialize the repository-owned Runtime identity once:

```sh
mkdir -p /path/to/repo/workspace
gatemole --repo /path/to/repo runtime init
```

Then start the development daemon in one terminal:

```sh
gatemole --repo /path/to/repo daemon
```

In another terminal, use the development-only manual transaction admission
command. It binds the local Runtime identity and atomically creates the
transaction, paired `AgentRun` and workspace capabilities. Then transition the
run to `running`:

```sh
gatemole --repo /path/to/repo tx create \
  --id tx:demo \
  --namespace demo \
  --intent "Write only inside the workspace" \
  --run run:demo

gatemole --repo /path/to/repo kernel run transition \
  --namespace demo --id run:demo --to running
```

All filesystem paths are logical paths rooted by the contract, such as
`workspace/output.txt`:

```sh
gatemole --repo /path/to/repo action fs-write \
  --namespace demo --run run:demo \
  --path workspace/output.txt --input ./candidate-output.txt

gatemole --repo /path/to/repo --json action fs-read \
  --namespace demo --run run:demo \
  --path workspace/output.txt
```

Inspect the authoritative history:

```sh
gatemole --repo /path/to/repo --json kernel run events \
  --namespace demo --id run:demo
```

The client reads the run cursor before every transition or action. A competing
write returns `KERNEL_CONFLICT`; it is not resolved by last-write-wins.

## Acceptance harness

Run the kernel-specific acceptance path separately from the existing
GatemoleBench release suite:

```sh
scripts/gatemolekernelbench.sh --out /tmp/gatemolekernelbench
```

It builds both binaries, performs an allowed write, proves a path escape is
denied and audited, restarts the daemon, and compares the complete run
projection and event history before and after restart.

## Security boundary and current limitations

This lower-level run/action slice mediates only actions submitted to its
filesystem broker. It cannot stop a process that also has direct host
filesystem access. Use the production Gatemole Runtime path for untrusted agent
execution: it launches the agent in the daemon-owned OCI boundary,
authenticates operator requests and removes direct model credentials.

Not yet implemented:

- MCP, GitHub API, database or cloud connector drivers.
- OIDC discovery/JWKS refresh, identity lifecycle provisioning, or a
  multi-tenant Runtime API.
- Capability revocation events.
- Leases, schedulers, or distributed stores.
- Portable canonical JSON signatures; the current event digest is a local v0
  tamper-evidence format.

See the [kernel boundary ADR](architecture/ADR-001-agent-kernel-boundary.md),
[threat model](architecture/THREAT_MODEL.md), and
[kernel semantics](architecture/KERNEL_SEMANTICS.md) for the governing
invariants.

The next layer is documented in [Local Agent Transactions](TRANSACTIONS.md).
