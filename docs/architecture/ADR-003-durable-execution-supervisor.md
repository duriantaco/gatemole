# ADR-003: Durable Execution Supervisor and Circuit Breaker

- Status: accepted for the next implementation slice
- Date: 2026-08-09
- Depends on: ADR-002 and paired execution settlement
- Owners: Gatemole maintainers

## Context

Gatemole now admits a task atomically and records one agent execution in both
the run and transaction ledgers before starting its OCI workload. Normal exit
and daemon-start recovery settle that same execution in both ledgers and charge
the supported budgets.

The workload is not yet supervised. `runAgentExecution` owns the process inside
one synchronous HTTP handler, derives its cancellation context from that
request, and holds the transaction mutex until the process and model broker
finish. The public client waits for the full timeout. The lower-level run
`cancel` command cannot move an active run out of `running`, and changing that
metadata alone would not stop the named OCI container or broker.

Startup recovery is intentionally safe but coarse. It removes the deterministic
agent container and broker, verifies any retained model receipts, and settles a
still-running execution as `interrupted`. There is no durable desired workload
state, execution lease, or reconciler that can distinguish an accepted cancel
from a lost client connection.

Adding `watch` as a polling loop without changing these ownership semantics
would improve display but would not satisfy OS-4. Adding a kill endpoint backed
only by an in-memory `context.CancelFunc` would lose cancellation across daemon
restart and could race normal settlement.

## Decision

The Runtime will introduce a daemon-owned, durable supervisor before connector
work. The first implementation remains single-node and local-Git, but execution
ownership moves out of the request handler.

### Durable supervised-execution record

Each admitted workload has one internal record bound to:

- namespace, transaction ID, run ID, attempt and execution ID;
- task, command, image, Runtime-configuration and stage-binding digests;
- deterministic agent container, broker container and broker network names;
- authority deadline and the run/transaction heads accepted at submission;
- desired state: `running` or `cancelled`;
- observed state: `pending`, `starting`, `running`, `stopping`, `settled` or
  `unknown`;
- lease owner, monotonically increasing lease epoch and lease expiry;
- accepted, started, heartbeat, cancel-requested and settled timestamps; and
- terminal process status plus receipt/evidence references when known.

This is operational state, not a second authority ledger. The hash-chained run
and transaction events remain authoritative for admission, execution and
business outcome.

### Submission and launch

`POST .../executions` validates live authority and idempotency, then atomically:

1. appends the paired execution-start events;
2. creates the exact supervised-execution record in `pending`; and
3. commits no workload effect before both writes succeed.

It returns `202 Accepted` with the existing transaction, run and execution IDs.
Repeating the same submission returns those IDs; changed immutable input
conflicts. A supervisor loop claims the record with a compare-and-swap lease,
revalidates live authority immediately before launch, and starts the
deterministically named broker and agent containers. A lease epoch may launch
that execution at most once.

The current synchronous `gatemole run` becomes a client composition: submit,
then watch to a terminal execution result. A `--detach` form returns immediately
after durable acceptance. The old request-owned execution path is removed after
the composition passes compatibility tests; it does not remain as a bypass.

### Watch

`gatemole watch TRANSACTION_ID` reads the authoritative transaction, run and
execution projections and advances only by explicit event cursors. The first
slice may use bounded polling with backoff; a server-side streaming transport is
not required. Output reconnects after client interruption without creating or
settling work.

Human output reports state changes and the terminal process receipt. JSON
output uses versioned records and exact IDs. A watcher timing out or
disconnecting never changes desired execution state.

### Cancel and revocation

`gatemole cancel TRANSACTION_ID` is not a run-state shortcut. It submits an
idempotent desired-state change bound to the active execution ID and current
event cursor. The request can commit while the workload is running because the
supervisor does not hold the transaction mutex across process execution.

After accepting cancellation, the supervisor:

1. prevents any not-yet-started workload from launching;
2. cancels the local process context and force-removes the exact agent and
   broker containers;
3. confirms their absence and finalizes available model receipts;
4. records one terminal interrupted/cancelled execution settlement in both
   ledgers; and
5. moves the run to `cancelled` and the transaction to a non-releasable aborted
   outcome in the same durable settlement boundary.

Cancel racing normal exit has one winner. If terminal settlement commits first,
cancel returns the terminal representation and performs no kill. If cancel
commits first, later successful process exit cannot restore release authority.
Repeated cancel is idempotent.

Authority expiry or explicit revocation uses the same desired-cancel path. No
new effect may execute after the cancellation cursor commits. The supervisor
must recheck desired state and live authority before each future mediated
action; OCI termination alone is not the eventual connector circuit breaker.

### Lease and restart reconciliation

Only one daemon may own an unexpired lease. Lease acquisition and renewal are
SQLite compare-and-swap operations tied to the ledger's existing single-writer
boundary. Losing a lease cancels local workload control and forbids terminal
settlement by the stale owner.

On restart, the supervisor enumerates nonterminal records before accepting new
work:

- `pending` plus desired `running`: claim and launch once;
- desired `cancelled`: remove named resources and settle cancellation;
- observed `starting` or `running`: inspect the deterministic container names,
  remove them, reconcile model receipts and settle `interrupted`; and
- an ambiguous inspection or cleanup result: mark `unknown`, block release and
  require reconciliation rather than relaunching.

The first slice does not reattach to a surviving container after daemon crash.
Deterministic interruption is safer and satisfies one reconciled outcome.

## Required store boundary

The store needs one transaction that can settle the paired execution, persist
terminal supervised state, clear the active run binding and, for accepted
cancel, append the run-cancel and transaction-abort transitions. Implementing
those as separate public mutations would recreate the divergence window that
paired execution settlement removed.

Supervisor state must be included in ledger health, backup/restore and startup
integrity checks. Foreign transaction/run/execution bindings, a regressing
lease epoch, duplicate active records or a terminal record without matching
ledger settlement fail startup.

## Acceptance gates

OS-4 is not complete until automated fault injection proves all of the
following:

1. Repeating submission before, during and after launch starts one workload.
2. Cancel accepted before launch starts no agent or broker.
3. Cancel during execution removes both containers within a bounded interval,
   settles both ledgers once and makes release impossible.
4. Cancel racing natural success produces one declared winner and no mixed
   run/transaction outcome.
5. Client disconnect and watcher timeout do not cancel the workload.
6. Daemon exit after every durable boundary restarts to one reconciled terminal
   outcome without relaunching a previously started workload.
7. Lease expiry prevents a stale supervisor from settling or launching.
8. Authority expiry and revocation use the same kill path.
9. `watch` resumes from an event cursor without omission or duplication.
10. Race-detector and production OCI acceptance cover submit, watch, cancel,
    shutdown and restart.

## Implementation order

1. Add the strict supervised-execution model, SQLite schema, integrity checks
   and atomic paired-start/terminal store operations.
2. Extract the current OCI/broker execution body into a supervisor worker with
   injected launch, inspect, kill and clock seams.
3. Start the reconciler before the HTTP listener and add deterministic fault
   tests for leases, launch and settlement.
4. Add asynchronous submit/status/cancel endpoints and client methods.
5. Compose `gatemole run`, `watch` and `cancel` over those endpoints; retain the
   exact transaction/evidence IDs used by `review`.
6. Route the new paths through Runtime and production acceptance before
   beginning OS-5 or OS-6.

## Non-goals

- Arbitrary process checkpointing or resuming a container after daemon crash.
- Pretending `pause` can freeze an agent at any instruction; pause remains
  unavailable until a declared safe boundary exists.
- Remote scheduling, high availability or multi-tenant leases.
- Connector dispatch, compensation or the Runtime action protocol.
- Control Plane enrollment or fleet cancellation.

## Consequences

The HTTP API becomes responsive while agents run, and cancellation becomes a
durable authority decision rather than process-local control. The cost is a new
operational state machine and a larger atomic store boundary. That cost is paid
before connectors because a remote effect path without durable workload
control would widen, rather than close, the current enforcement gap.
