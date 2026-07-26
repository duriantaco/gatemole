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
  requires driver reconciliation or human resolution.
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
| `KERNEL_DRIVER_UNAVAILABLE` | Required resource driver cannot accept work. |
| `KERNEL_INTERNAL` | Unexpected trusted-kernel failure. |

Unknown or internal errors fail closed at admission and action boundaries.
