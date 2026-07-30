# Agent Kernel Semantics

## Run lifecycle

The authoritative lifecycle is intentionally small. Product-specific progress
belongs in events and checkpoints, not new kernel states.

| State | Meaning | Terminal |
| --- | --- | --- |
| `created` | Resource is valid but not admitted to execute. | no |
| `admitted` | Policy and capacity accepted the run. | no |
| `running` | A runtime lease is active or executable work is available. | no |
| `waiting_for_event` | Waiting for a timer or external event. | no |
| `waiting_for_agent` | Waiting for a child or remote agent. | no |
| `waiting_for_approval` | Waiting for an authenticated human decision. | no |
| `blocked` | Policy or an unresolved condition prevents progress. | no |
| `failed` | Execution ended unsuccessfully. | yes |
| `cancelled` | An authorized actor ended execution. | yes |
| `completed` | Execution completed its contract path. | yes |

Allowed transitions:

| From | To |
| --- | --- |
| `created` | `admitted`, `failed`, `cancelled` |
| `admitted` | `running`, `blocked`, `failed`, `cancelled` |
| `running` | any waiting state, `blocked`, `failed`, `cancelled`, `completed` |
| any waiting state | `running`, `blocked`, `failed`, `cancelled` |
| `blocked` | `admitted`, `running`, `failed`, `cancelled` |
| terminal state | none |

Rules:

- Creation always begins at `created`.
- A terminal state cannot be reopened by a normal event.
- `completed` means the execution contract finished; it does not by itself mean
  the release gate returned `auto_merge`.
- `blocked` is recoverable because policy facts, evidence, or human authority
  may change.
- A reason is required when entering `blocked`, `failed`, or `cancelled`.
- Every transition uses the next contiguous run-event sequence.

## Runtime identity and preflight

`vouch runtime init` creates an ignored `.vouch/runtime.json` containing one
random, opaque Runtime ID. The file identifies this local repository Runtime;
it is not a user identity, signing key or fleet enrollment credential.

Before listening, `vouchd` loads that identity, acquires the repository-local
same-host, same-UID `.vouch/runtime.lock`, validates and canonicalizes the
transaction staging root, then acquires the separate ledger lock and opens
SQLite with the expected Runtime ID and enforcement profile. Existing metadata
is inspected before schema initialization. A conflicting Runtime ID,
incompatible nonempty profile, or nonempty unbound production ledger fails
startup before socket setup. The failed attempt may create lock files and
private transaction-root components, but it does not mutate the rejected
ledger, create a missing socket directory or socket path, or listen.

`POST /v0/namespaces/{namespace}/runtime/preflight` is a non-authoritative
readiness contract. It requires the client's exact expected Runtime ID and may
require `development` or `production`. For an OCI selection, the daemon also
checks its image policy and inspects the exact digest-pinned image without
pulling or running it. Health, engine and image stages have separate bounded
deadlines. A success reports the actual Runtime ID and enforcement profile but
grants no task or execution authority.

For every configured-daemon run/transaction lifecycle or read request, the
server requires an exact `Vouch-Runtime-ID` header before the handler runs.
`/healthz` and `/readyz` are deliberately exempt. Preflight and current v1 task
admission instead require the expected identity in their validated bodies;
the product client also sends the header when configured. Legacy v0 replay is
the narrow no-new-authority compatibility exception described below.

The product `run`, transaction, low-level `kernel` and `action` CLI surfaces
load `.vouch/runtime.json` and keep the resulting client binding for all calls,
including long-running agent and verifier operations. Direct
configured-daemon clients must set the same exact header. The header is
correlation metadata, not authentication or a secret.

The local identity, repository lock and ledger lock catch accidental
same-host, same-UID split-brain and repository/ledger mix-ups, including
different environment or database paths. They do not establish cryptographic
same-UID daemon attestation, cross-UID exclusion or cross-host uniqueness. A
dedicated OS account is therefore part of the hardened deployment boundary.
Normal Git clones do not copy the ignored identity and receive a new one;
deliberately copying all ignored identity and ledger state to another
repository deliberately clones the trust target. Fleet enrollment,
attestation and revocation remain future Control Plane responsibilities.

## Task admission

`POST /v0/namespaces/{namespace}/task-admissions` is the production authority
creation boundary; the `/v0` HTTP path and the versioned admission body are
independent contracts. A current v1 request contains the caller's expected
Runtime ID and enforcement profile plus caller-owned intent, sponsor, agent
profile, contract limits and an idempotency key. It cannot contain event
envelopes, lifecycle state, timestamps or authoritative digests.

For a new admission, `vouchd` derives and persists the exact `AgentTask`,
content-digest `ExecutionContract`, `AgentRun`, initial capability grants and
`AgentTransaction` in one database transaction. It authors
`run.created`, `capabilities.granted`, `run.state_changed` to `admitted`, and
`transaction.created`. The admission result and transaction binding carry the
same Runtime ID and enforcement profile, and the store requires them to match
the ledger metadata.

The idempotency key is scoped to its namespace. Retrying the same immutable
input returns the original admission representation, including after restart
or later lifecycle progress. Reusing the key with changed authority fails with
`KERNEL_IDEMPOTENCY_CONFLICT`. A failed persistence step leaves none of the
admission resources behind.

A configured Runtime cannot create legacy v0 admission authority. A nonempty
legacy ledger may be adopted only in development, where the exact request for
an already-persisted v0 admission may be replayed. A new v0 request, changed
idempotency input or production adoption fails closed. The replayed v0 result
remains readable history: a configured Runtime requires current v1 authority
before every run or transaction mutation, and permanently disables the legacy
capability-compilation route.

## Live execution authority

Before daemon-owned OCI execution performs workspace ownership changes, starts
a model broker or launches a container, `vouchd` reads one consistent
transaction-keyed authority snapshot. It verifies the complete run and
transaction event histories, then compiles an execution plan from the current
task, contract, admitted run, grants and transaction.

The image reference and command remain untrusted executable material: their
digests must match the persisted task. The caller may narrow the timeout but
cannot widen the live deadline. The current profile supports exactly
`workspace/**` read/write and optional `model` `<provider>/*` `model.invoke`
authority through the configured daemon broker. Narrower mounts, unsupported
conditions and unimplemented budgets fail before workload creation.

After preflight and non-workload preparation, launch is claimed in one SQLite
transaction: the admitted run head and transaction head must still match the
snapshot while `agent_execution.started` is appended. The authority deadline
also bounds model-broker startup. A stale or expired plan therefore starts no
broker or agent container.

The claim does not yet advance and settle both lifecycle ledgers. That paired
run/transaction mutation and durable usage charging are the next OS-3 step.

## Action lifecycle

```text
requested
  -> denied
  -> approval_required -> approved -> authorized
                       -> rejected
                       -> expired
  -> authorized -> executing -> committed
                             -> failed
                             -> unknown
```

Rules:

- `denied`, `rejected`, `expired`, `committed`, and `failed` are terminal.
- `unknown` is not success or failure. It means an effect may have occurred and
  requires connector-specific reconciliation coordinated by `vouchd` or human
  resolution.
- Authorization is persisted before `executing`.
- `committed` requires a validated receipt.
- A request's normalized argument digest is immutable. A changed request needs
  a new ID, decision, and approval.
- Retry creates a new attempt linked to the original idempotency key and prior
  attempt; it does not rewrite history.

## Event ordering

- Sequence numbers begin at 1 with `run.created`.
- Every later event increments the sequence by exactly one.
- `previous_digest` is absent for sequence 1 and required afterward.
- `digest` covers the canonical event without its `digest` field.
- Events are immutable after acknowledgement.
- Wall-clock timestamps aid operation but do not determine ordering.

Canonical JSON serialization will be specified before event signing ships.
Until then, digest fields are validated structurally but must not be described
as portable cryptographic proof across implementations.

## Error model

Kernel errors have a stable code, operation, message, and optional resource or
field. Human text may improve without changing the code.

| Code | Meaning |
| --- | --- |
| `KERNEL_SCHEMA_INVALID` | Resource is malformed or uses an unsupported version. |
| `KERNEL_UNKNOWN_FIELD` | Strict decoding found an undeclared field. |
| `KERNEL_IDENTITY_INVALID` | Principal or run identity could not be validated. |
| `KERNEL_TRANSITION_INVALID` | Requested lifecycle transition is not allowed. |
| `KERNEL_EVENT_SEQUENCE` | Event sequence is missing, duplicated, or non-contiguous. |
| `KERNEL_EVENT_CHAIN` | Previous digest or event digest validation failed. |
| `KERNEL_CAPABILITY_DENIED` | No valid grant authorizes the action. |
| `KERNEL_CAPABILITY_EXPIRED` | Matching grant is expired. |
| `KERNEL_CAPABILITY_REVOKED` | Matching grant was revoked. |
| `KERNEL_DELEGATION_EXCEEDED` | Child authority exceeds its parent ceiling. |
| `KERNEL_APPROVAL_REQUIRED` | Execution is paused pending human authority. |
| `KERNEL_APPROVAL_INVALID` | Approval identity, scope, action, or expiry is invalid. |
| `KERNEL_BUDGET_EXCEEDED` | A hard run or parent-inclusive budget is exhausted. |
| `KERNEL_ACTION_UNKNOWN` | Effect outcome is ambiguous and requires reconciliation. |
| `KERNEL_CHECKPOINT_INCOMPATIBLE` | Checkpoint cannot resume under current versions. |
| `KERNEL_NOT_FOUND` | Requested kernel resource does not exist in the caller's namespace. |
| `KERNEL_CONFLICT` | Optimistic concurrency or lease ownership failed. |
| `KERNEL_IDEMPOTENCY_CONFLICT` | An idempotency key is already bound to different immutable admission input. |
| `KERNEL_DRIVER_UNAVAILABLE` | Required connector driver cannot accept work. |
| `KERNEL_INTERNAL` | Unexpected trusted-kernel failure. |

Unknown or internal errors fail closed at admission and action boundaries.
