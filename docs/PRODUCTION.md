# Production Runtime Operations

Vouch's production profile is a single-node Vouch Runtime for autonomous
software changes released through local Git refs. It enforces the Runtime
boundary; it does not turn the current implementation into a multi-tenant or
highly available Vouch Agent OS. It is the hardened operator profile for the
same kernel used by the Vouch Developer Runtime, not a fleet product. This
narrow, single-tenant Git profile is supported only for revisions that pass the
mandatory Go formatting,
module, test, vet, and `govulncheck` checks; kernel race tests; VouchBench,
VouchKernelBench, VouchTransactionBench, VouchRuntimeBench; and production OCI
acceptance. Vouch Contracts has separate beta status. Enterprise connector
drivers, multi-tenancy, HA and the Control Plane are not implemented in this
profile.

## Enforced production boundary

The daemon refuses to start unless it has:

- A valid, local-only `.gatemole/runtime.json` created for the exact repository
  root by `vouch runtime init`.
- At least one digest-pinned OCI image in its allowlist.
- An Ed25519 approval trust document with at least one trusted reviewer.
- A static OIDC trust document containing an HTTPS issuer, accepted audiences,
  claim mappings, and trusted EdDSA or RS256 public keys.
- At least one daemon-owned verifier profile with a digest-pinned image, exact
  argument vector, and bounded timeout.
- At least one allowed full Git branch-ref pattern.
- A resolvable daemon-owned OCI engine.
- A non-root numeric UID and GID for agent and verifier containers.

Production trust, verifier-profile, and model-policy files must be owned by
root or the daemon account, must be regular files rather than symlinks, and
must not be group- or world-writable. The daemon reads each file into one
bounded, inode-stable startup snapshot; parsing and policy evidence use those
same bytes. Changing a source path after startup does not change live policy.

The documented production boundary also requires deployment controls that the
daemon cannot establish by itself:

- A dedicated single-tenant host or VM. Untrusted users and workloads must not
  share the daemon account, OCI engine, repository, or filesystem.
- A dedicated non-root daemon account, with agent and verifier workloads using
  the same numeric UID and GID.
- Root- or daemon-owned parent directories for every repository, database,
  transaction root, socket, trust document, verifier profile, model policy,
  and daemon executable. No untrusted identity may replace or rewrite a parent
  path.
- Dedicated filesystems or volumes with operating-system byte and inode quotas
  covering the source repository and Git object database, SQLite database/WAL,
  transaction worktrees, immutable verifier materializations, and evidence.
  Container memory, PID, tmpfs, and image-layer limits do not constrain a
  writable bind mount; an agent can otherwise exhaust host bytes or inodes.

The production runtime:

- Loads the repository's Runtime ID, acquires the repository-local
  `.gatemole/runtime.lock`, validates the canonical private transaction staging
  root, and opens the Runtime-bound SQLite ledger before creating its Unix
  listener.
- Binds the exact Runtime ID and `production` enforcement profile to SQLite
  metadata before recovery or request handling. A ledger belonging to another
  Runtime or a conflicting nonempty profile fails startup.
- Executes agents and verifiers in the daemon, not in the CLI.
- Persists the exact task intent and its selected agent-profile, image, command,
  transaction and run bindings in a digest-bound `gatemole.agent_task.v0`
  resource. The agent receives that envelope through a separate read-only
  `/gatemole/task.json` mount with `GATEMOLE_TASK_PATH` and `GATEMOLE_TASK_DIGEST`;
  it is never written into the mutable worktree.
- Rejects client-asserted agent and verification receipts.
- Uses a read-only container root filesystem, no Linux capabilities,
  no-new-privileges, resource limits, and a non-root identity.
- Verifies every non-health API request as a signed OIDC token, enforces
  viewer/operator/approver roles and namespace membership, rejects actor
  impersonation, and binds the verified issuer and claims digest into events.
- Requires the exact `Gatemole-Runtime-ID` header on every configured-daemon
  lifecycle and read request. Runtime preflight and admission v1 carry the same
  expected identity in their validated bodies. Health and readiness remain
  unbound liveness/readiness endpoints.
- Mounts the transaction worktree read/write for the agent. A verifier receives
  a separate read-only materialization created from the exact immutable Git
  tree revision frozen by staging, never the mutable agent worktree.
- Allows only preloaded, digest-pinned agent, verifier, and broker images
  (`--pull=never`).
- Resolves verification from daemon-owned profiles. A caller cannot replace a
  configured verifier image, command, timeout, or profile name.
- Freezes the effect set, staged-state digest, verification inputs, release
  target, and expected target version before approval.
- Verifies short-lived, single-use, Ed25519-signed approvals.
- Prevents the sponsor, participating agents, and identities that exercised
  transaction authority from approving the same transaction. Approval
  signatures, expiry, trust, and separation are checked again at release.
- Requires the release caller to be distinct from every approver. Multiple
  required approvals must also come from distinct identities.
- Binds the identity-trust document, enforcement profile, complete
  verifier-profile set, approval-trust document, and verifier OCI runtime
  policy into the frozen authority-policy digest. Changing any of those inputs
  invalidates prepared authority and requires preparation and approval again.
- Records the Runtime ID and `development` or `production` in SQLite metadata
  on first binding. The Runtime ID is never rebound automatically. An empty
  ledger may switch profile; after any run or transaction history exists, it
  cannot switch profiles. Production refuses a pre-existing nonempty ledger
  without both bindings.
- Publishes a prepared Git commit with a compare-and-swap update of an allowed
  local ref. It does not push, merge, or deploy.
- Reconciles a crash after Git publication and records partial or unknown
  outcomes explicitly.
- Hash-chains transaction events in a synchronous SQLite WAL.
- Holds both a repository-local same-host, same-UID Runtime lock and a ledger
  lock so one daemon account cannot accidentally start two daemons for one
  repository or one ledger, even with different environment or database paths.
- Cleans up and marks an active agent execution interrupted during startup
  recovery.
- Disables the legacy client-supervised mutation APIs in production: clients
  cannot author agent start/finish receipts or submit external verification
  results. Production agents and verifiers run through daemon-owned OCI
  endpoints.
- Permanently denies mediated filesystem reads and writes through any `.git`
  or `.gatemole` path component, regardless of the requested capability.
- Gives an agent model access only when admission explicitly declares the
  daemon provider, such as `vouch run --model-provider openai`. The
  transaction-specific broker remains a daemon ceiling: the agent receives no
  provider credential and has no direct Internet route; the broker enforces
  the provider origin, model/tool allowlists, byte and token budgets, stateless
  requests, and a durable hash-chained receipt ledger.
  Completed, failed, and unknown call counts are bound into the execution
  receipt. Missing/invalid provider usage is marked unknown and conservatively
  exhausts the input budget.
- Bounds Git-stage inspection to 10,000 changed files, 64 MiB per before/after
  payload, 512 MiB total inspected payloads, and 128 MiB of captured diff.
- Bounds whole-tree scanning and immutable materialization to 100,000 entries,
  256 MiB per Git blob, and 1 GiB of aggregate blob payload. Gitlinks, and
  therefore submodules, are unsupported because they do not provide the fully
  pinned file tree required by this profile.
- Captures at most 16 MiB of verifier stdout and 16 MiB of verifier stderr.
  Exceeding either limit cancels and fails that verification.
- Admits at most two OCI workloads globally across agents and verifiers.
  Additional work fails closed rather than waiting in an unbounded queue.

Task intent is durable audit data, not a secret transport. Operators must not
put credentials or unredacted secrets in task intent. Protect the SQLite ledger
and its backups accordingly.

## Immutable Git-tree staging and verification

Staging does not derive effects from a mutable filesystem walk. Vouch first
constructs a private Git index, writes an immutable tree object, and derives the
effect ledger, patch digest, and staged-state digest from that exact tree.
Preparation later commits only the tree revision bound into the frozen plan.

Before each verifier starts, Vouch re-inspects the agent worktree into an
immutable tree and requires its tree, effect-set, and staged-state digests to
match the frozen transaction. It then materializes that exact tree revision
into a new private directory outside both the source repository and agent
worktree. Regular and executable blobs are read by object ID, symlinks are
created only after non-links, and the completed directories/files are hardened
read-only before the OCI engine mounts the workspace read-only. The
materialization is removed after verification, and the mutable agent worktree
is checked again.

The verifier therefore consumes the frozen Git-tree bytes even if the agent
worktree changes concurrently. The trusted host, Git executable, and source
repository's Git object database remain part of the deployment boundary.
Whole-tree limits are enforced before and during materialization, and a gitlink
fails closed rather than recursively consulting a mutable submodule checkout.

## Deployment

Run the daemon as a dedicated unprivileged operating-system account. That
account needs:

- Read/write access to the repository, transaction root, database directory,
  and socket directory.
- Permission to use the configured OCI engine.
- No long-lived approval private keys in the daemon configuration or
  environment. The current approval CLI limitation described below still
  requires the signing process to use the daemon account while it connects.

Every CLI process that connects to the Unix socket must run with the daemon
account's effective UID. OIDC principals and roles still distinguish operator,
reviewer and releaser authority, but the Unix peer boundary does not support
separate human Unix accounts.

Treat that account, the dedicated host or VM, the OCI engine, and the
root- or daemon-owned parents of every configured path as one trusted boundary.
The daemon rejects production config files that are symlinks, are owned by an
identity other than root or the daemon account, or are group/world-writable,
and retains the exact bytes it parsed and digested. It separately validates
every transaction-root component, allowing a writable parent only when its
sticky bit protects the daemon-owned child. It does not prove that every other
configured parent directory is protected from replacement at the next
restart. Secure those parents with operating-system ownership and permissions.

Place the source repository and Git object database, SQLite database/WAL,
transaction staging root, immutable verifier materializations and evidence on
a dedicated quota-controlled filesystem or coordinated dedicated volumes. The
transaction staging root must remain outside and disjoint from the source
repository: neither path may contain the other. Configure both a byte quota and
an inode/file-count quota for every backing volume, reserve capacity for
Git-object creation and SQLite/WAL recovery, and alert before any quota is
exhausted. This is required: the writable agent workspace is a host bind mount,
so OCI memory, tmpfs, PID, and image-storage limits do not stop an agent from
filling its backing host filesystem.

Preload every allowed image on the host. The daemon never pulls an image while
executing a transaction.

Create approval material:

```sh
vouch approval keygen \
  --key-id key:security-reviewer-1 \
  --principal human:security-reviewer-1 \
  --issuer https://login.example.com/ \
  --class security-reviewer \
  --private-key /secure/operator/reviewer.key \
  --trust-file /etc/gatemole/approval-trust.json
```

Give the daemon configuration only the public trust document. The current
`tx approve` command reads the private key, fetches the pending package and
submits the signed decision in one process. Because the socket accepts only the
daemon account's effective UID, Vouch does not yet provide a separate offline
sign-and-submit flow that keeps the reviewer key inaccessible to that OS
account while approval runs. If that limitation is acceptable inside the
trusted single-tenant host boundary, make the key available to the reviewer-
authorized CLI process only for the command and remove it immediately
afterward. If hard private-key custody separation is required, do not treat
this profile as satisfying it until an offline or hardware-backed approval
path exists. Restarting the daemon after rotating the public trust document
loads a new snapshot, changes the approval-trust digest, and invalidates
transactions prepared under the previous trust set.

Configure `/etc/gatemole/identity-trust.json` from the issuer metadata and JWKS.
Vouch currently consumes a static trust document rather than performing OIDC
discovery:

```json
{
  "version": "gatemole.oidc_trust.v0",
  "issuer": "https://login.example.com/",
  "audiences": ["gatemole-production"],
  "clock_skew_seconds": 60,
  "max_token_lifetime_seconds": 3600,
  "principal_id_claim": "gatemole_principal_id",
  "principal_kind_claim": "gatemole_principal_kind",
  "namespace_claim": "gatemole_namespaces",
  "roles_claim": "gatemole_roles",
  "jwks": {"keys": [{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "…", "n": "…", "e": "AQAB"}]}
}
```

The custom claims must contain a Vouch principal ID, `human`, `service`, or
`operator` kind, one or more namespaces, and one or more roles from `viewer`,
`operator`, `approver`, or `admin`. Configure those claims in the enterprise
IdP. `vouch identity keygen` and `vouch identity issue` exist only for local
bootstrap and acceptance testing; the daemon is not an identity provider.

Configure `/etc/gatemole/verifier-profiles.json`. Every profile in the document is
a mandatory verification gate for every production transaction:

```json
{
  "version": "gatemole.verifier_profiles.v0",
  "profiles": [
    {
      "name": "test-suite",
      "image": "registry.example/verifier@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
      "command": ["/usr/local/bin/verify", "--strict"],
      "timeout_seconds": 600
    }
  ]
}
```

The document is strict JSON: unknown fields, duplicate keys, duplicate profile
names, mutable image tags, empty arguments, trailing values, and files over
2 MiB are rejected. The production daemon also caps each verifier timeout at
15 minutes. Changing any profile requires transactions prepared under the old
profile set to be prepared and approved again.

Initialize the canonical repository once as the dedicated daemon account,
using the exact agent profile that will be deployed:

```sh
vouch --repo /srv/vouch/repository runtime init \
  --agent coding-agent \
  --image registry.example/agent@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
  --source-digest sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  -- /usr/local/bin/agent
```

This creates `.gatemole/runtime.json` with mode `0600` and writes local-state
ignore rules. The identity must remain untracked. Commit the agent profile and
`.gatemole/.gitignore`, not the Runtime identity. A normal deployment from a Git
clone therefore gets a new Runtime ID; do not seed a second independent
Runtime by copying another deployment's ignored identity or ledger.

Gatemole does not auto-migrate pre-cutover `.vouch` state. If that legacy path
exists as a directory, file, or symlink, CLI and daemon startup fail before
creating a ledger or socket. Archive it for incident retention or remove it
through an explicit operator procedure, then initialize `.gatemole`; never
copy signed or digest-bound artifacts between namespaces.

Start the production daemon:

```sh
gatemoled \
  --repo /srv/vouch/repository \
  --db /var/lib/gatemole/kernel.db \
  --socket /run/gatemole/gatemoled.sock \
  --transaction-root /var/lib/gatemole/transactions \
  --runtime-profile production \
  --runtime-engine /usr/bin/docker \
  --allowed-images 'registry.example/agent@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef' \
  --verifier-profiles /etc/gatemole/verifier-profiles.json \
  --approval-trust /etc/gatemole/approval-trust.json \
  --identity-trust /etc/gatemole/identity-trust.json \
  --allowed-git-refs 'refs/heads/agent-release/*'
```

Startup loads the Runtime identity and binds its exact ID plus the
`production` profile to SQLite before creating the socket. It fails closed on
a different Runtime ID, incompatible ledger profile, or nonempty legacy ledger
without the required bindings. A rejected ledger validation can leave the
required Runtime and database lock files plus validated private transaction
root components, but does not mutate the rejected ledger, create a missing
socket directory, create the socket path, or listen. Socket setup happens only
after ledger validation and recovery succeed.

The documented production profile runs the daemon as a dedicated non-root
account and uses that same numeric UID/GID for agent and verifier containers.
Linux bind-mounted worktrees and broker receipt directories must have the same
owner. Vouch refuses a mismatched non-root production configuration rather
than starting containers that cannot write their staged state. A root-run
daemon configuration is outside this narrow hardened execution boundary.

Set the caller's short-lived access token for every CLI process:

```sh
export GATEMOLE_IDENTITY_TOKEN='eyJ…'
vouch --repo /srv/vouch/repository tx list \
  --socket /run/gatemole/gatemoled.sock \
  --namespace engineering
```

The actor ID and kind passed by a mutation command must match the token.
Approval commands additionally require the `approver` role and a signed
approver whose issuer exactly matches the OIDC issuer.

The product CLI also loads this repository's `.gatemole/runtime.json` and adds its
exact ID to requests from the `run`, transaction, low-level `kernel` and
`action` surfaces, including reads, lifecycle changes and long-running agent
or verifier calls. Preflight and admission v1 validate the same binding in
their versioned bodies. Direct API clients must provide the equivalent
`Gatemole-Runtime-ID` header for other configured-daemon requests.

With the daemon running, verify its exact Runtime ID and enforcement profile
through authoritative preflight:

```sh
vouch --repo /srv/vouch/repository doctor \
  --socket /run/gatemole/gatemoled.sock \
  --namespace engineering \
  --agent coding-agent \
  --require-enforcement-profile production
```

For hosted-model agents, preload the digest-pinned broker image, provide the
provider secret only to the daemon, and add:

```sh
export OPENAI_API_KEY='provider-secret-visible-only-to-gatemoled'
gatemoled \
  … \
  --model-broker-image 'registry.example/gatemole-model-broker@sha256:…' \
  --model-broker-policy /etc/gatemole/model-policy.json \
  --model-provider-token-env OPENAI_API_KEY
```

The initial broker supports only OpenAI-compatible `POST /v1/responses`. Its
policy must use an HTTPS upstream and `force_store_false: true` in production.
The agent sees a transaction-scoped broker credential as `OPENAI_API_KEY` and
the internal broker URL as `OPENAI_BASE_URL`; it never sees the provider key.
Model access is still absent by default. Admit it for one task with:

```sh
vouch --repo /srv/vouch/repository run \
  --socket /run/gatemole/gatemoled.sock \
  --namespace engineering \
  --require-enforcement-profile production \
  --intent "Fix the approved authentication regression" \
  --agent coding-agent \
  --model-provider openai
```

Omitting `--model-provider` produces a networkless agent container. Requesting
a provider that does not exactly match the daemon broker policy is denied
before a broker or agent workload starts.

Use a dedicated release ref that is not checked out in the source repository.
Vouch rejects publication to a checked-out ref because the worktree and index
would not be updated atomically with the ref. Publication ends at the local
compare-and-swap ref update; Vouch has no remote Git push, pull-request merge,
or deployment connector in this profile.

The Unix socket is mode `0600`. Put its parent directory on a local filesystem
owned by the daemon account. OIDC protects API requests, but the current server
still has no network listener, TLS termination, distributed rate limiting, or
multi-tenant control-plane hardening. Do not expose it through an ad-hoc
network proxy.

Runtime preflight correlates the caller's expected Runtime ID with the daemon
and ledger reached through that socket; it is not cryptographic daemon
attestation. Before sending HTTP or a bearer token, the client requires a
stable private socket path and verifies that the connected process's
kernel-reported UID owns that socket. The daemon likewise accepts only clients
with its own effective UID. The protected parent and same OS account remain
part of this same-host boundary; same-UID malware or root can still impersonate
the daemon. Run the daemon under a dedicated OS account; neither peer-UID
checks nor `Gatemole-Runtime-ID` provide cryptographic same-UID attestation.
Operators, reviewers and releasers must therefore invoke the connected CLI
under that same OS account. Their OIDC tokens preserve logical separation of
duties, but do not create an OS-account boundary.

For OCI tasks, preflight completes before task admission or worktree creation.
Admission v1 then persists the exact Runtime ID and daemon enforcement profile
with the transaction. The kernel independently checks those bindings again;
preflight itself grants no authority. Subsequent lifecycle and read requests
must present the matching Runtime header before their handlers execute.

`GET /healthz` reports process liveness. `GET /readyz` performs a bounded
SQLite read and a rollback-only metadata write, runs the OCI engine's
server-readiness check, and inspects every configured pinned agent, verifier,
and model-broker image. It never pulls an image. Readiness results are cached
for five seconds and the request is bounded by a two-second timeout; both
success and failure can therefore remain visible until the cache expires.
Required production verifier profiles are also checked before reporting ready.
Both endpoints intentionally omit internal error details.

Client-supervised host execution is disabled by default. The
`--allow-unsafe-host-execution` switch exists only for explicit development
testing and is rejected by the production profile.

## Acceptance gate

Do not deploy the production profile unless every mandatory CI job passes for
the exact revision. The equivalent local gates are:

```sh
go test ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...
go test -race -count=1 ./internal/kernel/...
scripts/vouchbench.sh --out /tmp/vouchbench
scripts/vouchkernelbench.sh --out /tmp/vouchkernelbench
scripts/vouchtransactionbench.sh
scripts/vouchruntimebench.sh
image="$(scripts/vouchproductionfixture.sh --tag gatemole-production-fixture:acceptance)"
GATEMOLE_PRODUCTION_IMAGE="$image" scripts/vouchproductionbench.sh
```

The normal Go CI job also enforces formatting and a tidy module graph. The
Skylos scan is advisory and is not one of the production gates.

Together, the mandatory Go, race, benchmark, and OCI gates prove the listed
identity, authority-revalidation, and compare-and-swap negative cases. The
production benchmark itself uses a real daemon, SQLite ledger, Git repository,
and OCI engine. It exercises authenticated event provenance, non-root
execution, read-only root filesystems, blocked direct agent egress,
policy-controlled model brokering without provider-credential disclosure,
exact immutable Git-tree verifier materialization, rejection of a
caller-substituted verifier command, signed independent approval, a separate
releaser, source-worktree isolation, and successful atomic local Git-ref
publication.

Successful benchmark runs remove their private repository, Go caches, ledger,
and evidence so local acceptance does not accumulate thousands of temporary
files. Set `GATEMOLE_PRODUCTION_KEEP=1` only when those artifacts are needed for
debugging.

## Backup and restore

Treat the following as one recovery set:

- The local `.gatemole/runtime.json` Runtime identity.
- SQLite database and its WAL state.
- Transaction worktree/evidence root.
- Source Git repository and released refs.
- Approval and OIDC public trust documents, model policy, and daemon
  configuration.

Keep the source Git repository/object database, SQLite database/WAL,
transaction worktrees and immutable verifier materializations, and evidence
inside quota-controlled recovery volumes or coordinated snapshots. A quota
change is an operational change and should preserve enough reserved bytes and
inodes for Git-object creation, WAL rollback, evidence finalization, and
incident recovery.

For a simple consistent backup, stop the daemon cleanly, verify no `gatemoled`
process owns the database lock, then snapshot the complete recovery set. Do not
copy only the SQLite main file while the daemon is running; committed pages may
still be in the WAL.

Restore the complete set to the same paths and start exactly one daemon.
Preserving both `.gatemole/runtime.json` and its bound ledger restores the same
logical Runtime. Never run the original and restored copy concurrently on
different hosts: `.gatemole/runtime.lock` and the ledger lock are same-host and
same-UID only. Startup verifies every transaction event chain before recovery.
If an agent execution was active, Vouch removes its deterministic container and
appends an `interrupted` receipt. A cleanup or ledger-integrity failure
prevents startup.

To create a separate Runtime, start from a normal Git clone, run
`vouch runtime init` to create a new ignored identity and use a new empty
ledger. Deliberately copying both the ignored identity and ledger deliberately
clones the trust target; the local identity mechanism does not detect that
cross-host operation.

## Operations and incidents

Monitor:

- Daemon exit and restart count.
- Free bytes, free inodes, and quota headroom for the source Git object
  database, SQLite/WAL, transaction worktrees, verifier materializations, and
  evidence.
- Transactions in `committing`, `partially_committed`,
  `manual_recovery_required`, or `release_failed`.
- Approval and verification expiry.
- OCI engine availability.
- `/healthz` and `/readyz` status.
- OIDC key rotation and access-token issuance failures.
- Model-broker unknown calls, exhausted budgets, and receipt-ledger integrity.

Use `vouch tx get` and `vouch tx events` as the primary incident record. Do not
edit the SQLite database or transaction worktree to force a state transition.
Preserve the database, evidence directory, target Git ref, and daemon log
before manual recovery.

## Current limits

The production profile is suitable only when all of these constraints are
acceptable:

- One daemon, one security tenant, and one local SQLite ledger on a dedicated
  trusted host or VM; no HA, failover or Vouch Control Plane.
- Runtime identity prevents accidental repository/ledger/socket confusion and
  the repository-local Runtime lock prevents same-host, same-UID duplicate
  ownership for that repository. It does
  not provide cross-UID or cross-host uniqueness, hardware-backed identity,
  cryptographic same-UID attestation, organization enrollment or fleet
  revocation.
- A trusted dedicated daemon account and same-UID container boundary. Vouch
  does not protect against a malicious host administrator, OCI-engine operator,
  or identity able to replace configured parent paths.
- Every connected CLI, including approval and release, must use the daemon
  account's effective UID. The current approval command has no separate offline
  signing/submission or hardware-key interface, so logical reviewer separation
  does not provide filesystem-level private-key isolation from that account.
- Mandatory OS byte and inode quotas covering the source Git object database,
  SQLite/WAL, bind-mounted transaction worktrees, immutable verifier
  materializations, and evidence. Host filesystem exhaustion is not contained
  by OCI resource flags.
- One deeply implemented release primitive: compare-and-swap updates of local
  Git refs. It is not yet behind the planned generic connector interface.
  There is no push, pull-request merge, deployment, or production database
  connector.
- Static OIDC issuer/JWKS trust only. There is no discovery, automatic JWKS
  refresh, token revocation feed, Entra/Okta provisioning adapter, or SCIM
  lifecycle integration.
- OCI isolation through the configured engine, not a VM or formally verified
  sandbox.
- The model broker supports only OpenAI-compatible Responses API calls. It is
  not a general HTTP, MCP, Anthropic, cloud, or SaaS credential broker.
- The broker's application policy pins one HTTPS origin, but host-level
  destination allowlisting still depends on deployment firewall controls.
- No PostgreSQL or Kubernetes commit/compensation coordinator.
- No automatic evidence retention, export, or SIEM integration.
- Git-stage inspection is deliberately bounded. Transactions above the
  documented file, payload, or diff limits fail closed with
  `KERNEL_BUDGET_EXCEEDED` and must be split or handled outside this profile.
- Full-tree scanning and materialization are separately bounded to 100,000
  entries, 256 MiB per blob, and 1 GiB aggregate. Repositories containing
  gitlinks/submodules are unsupported by this profile.
- Verifier output is bounded to 16 MiB each for stdout and stderr, and only two
  OCI agent/verifier workloads may run globally at once.
- Verifiers use a private, read-only materialization of the exact frozen Git
  tree. The Git executable, object database, and host materialization path
  remain trusted operator-controlled inputs.
- Legacy transaction creation and client-supervised execution/verification
  mutation APIs are disabled in production; transaction authority begins at
  Runtime/profile-bound v1 task admission. A development-adopted ledger may
  replay an already-persisted v0 admission with exact idempotency input, but a
  configured Runtime cannot create new v0 authority and production does not
  adopt an unbound nonempty ledger. Replayed v0 history cannot mutate a run or
  transaction, compile capabilities, execute an action, or yield live
  execution authority. The mediated filesystem API cannot access `.git` or
  `.gatemole` control state.
- OCI launch revalidates live admission authority and atomically pins the
  admitted run and transaction heads while recording execution start before
  any workload. Run and transaction execution events are not yet advanced and
  settled as one paired lifecycle, and run budget usage is not yet durably
  charged. That is the remaining OS-3 boundary.
- No claim of universal rollback. The implemented commit primitive is an
  atomic, version-checked Git-ref update.

Within those limits and after all mandatory gates pass, the single-node,
single-tenant Git runtime is the hardened production execution profile. This
does not imply stable product packaging or fleet operations. The compiler's
wider product surface remains experimental. Additional effect connectors,
enterprise administration, multi-tenancy and HA are planned and unimplemented,
not beta features of this profile. Cross-host Runtime enrollment, attestation
and revocation belong to the future Control Plane.
