# Production Runtime Operations

Vouch's production profile is a single-node Vouch Runtime for autonomous
software changes released through local Git refs. It enforces the Runtime
boundary; it does not turn the current implementation into a multi-tenant or
highly available Vouch Control Plane. This narrow, single-tenant Git profile is
supported only for revisions that pass the mandatory Go formatting,
module, test, vet, and `govulncheck` checks; kernel race tests; VouchBench,
VouchKernelBench, VouchTransactionBench, VouchRuntimeBench; and production OCI
acceptance. Vouch Contracts, enterprise connector drivers, multi-tenancy and HA
architecture remain beta.

## Enforced production boundary

The daemon refuses to start unless it has:

- At least one digest-pinned OCI image in its allowlist.
- An Ed25519 approval trust document with at least one trusted reviewer.
- A static OIDC trust document containing an HTTPS issuer, accepted audiences,
  claim mappings, and trusted EdDSA or RS256 public keys.
- At least one daemon-owned verifier profile with a digest-pinned image, exact
  argument vector, and bounded timeout.
- At least one allowed full Git branch-ref pattern.
- A resolvable daemon-owned OCI engine.
- A non-root numeric UID and GID for agent and verifier containers.

Production trust, verifier-profile, and model-policy files must be regular
files, not symlinks, and must not be group- or world-writable.

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

- Executes agents and verifiers in the daemon, not in the CLI.
- Persists the exact task intent and its selected agent-profile, image, command,
  transaction and run bindings in a digest-bound `vouch.agent_task.v0`
  resource. The agent receives that envelope through a separate read-only
  `/vouch/task.json` mount with `VOUCH_TASK_PATH` and `VOUCH_TASK_DIGEST`;
  it is never written into the mutable worktree.
- Rejects client-asserted agent and verification receipts.
- Uses a read-only container root filesystem, no Linux capabilities,
  no-new-privileges, resource limits, and a non-root identity.
- Verifies every non-health API request as a signed OIDC token, enforces
  viewer/operator/approver roles and namespace membership, rejects actor
  impersonation, and binds the verified issuer and claims digest into events.
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
- Records `development` or `production` in SQLite metadata on first binding.
  An empty ledger may be rebound; after any run or transaction history exists,
  it cannot switch profiles. Production refuses a pre-existing nonempty
  unmarked ledger.
- Publishes a prepared Git commit with a compare-and-swap update of an allowed
  local ref. It does not push, merge, or deploy.
- Reconciles a crash after Git publication and records partial or unknown
  outcomes explicitly.
- Hash-chains transaction events in a synchronous SQLite WAL.
- Holds an operating-system lock so only one daemon can own a ledger.
- Cleans up and marks an active agent execution interrupted during startup
  recovery.
- Disables the legacy client-supervised mutation APIs in production: clients
  cannot author agent start/finish receipts or submit external verification
  results. Production agents and verifiers run through daemon-owned OCI
  endpoints.
- Permanently denies mediated filesystem reads and writes through any `.git`
  or `.vouch` path component, regardless of the requested capability.
- Optionally gives an agent model access through a transaction-specific broker.
  The agent receives no provider credential and has no direct Internet route;
  the broker enforces the provider origin, model/tool allowlists, byte and token
  budgets, stateless requests, and a durable hash-chained receipt ledger.
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
- No approval private keys.

Treat that account, the dedicated host or VM, the OCI engine, and the
root- or daemon-owned parents of every configured path as one trusted boundary.
The daemon rejects symlinked or group/world-writable production config files,
but it does not prove that every parent directory is protected from replacement.
Secure those parents with operating-system ownership and permissions.

Place the source repository and Git object database, SQLite database/WAL,
transaction worktrees and immutable verifier materializations under the
transaction root, and evidence on a dedicated quota-controlled filesystem or
coordinated dedicated volumes. Configure both a byte quota and an
inode/file-count quota for every backing volume, reserve capacity for
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
  --trust-file /etc/vouch/approval-trust.json
```

Keep the private key with the reviewer. Give the daemon only the public trust
document. Rotating that document changes the approval-trust digest and
invalidates transactions prepared under the previous trust set.

Configure `/etc/vouch/identity-trust.json` from the issuer metadata and JWKS.
Vouch currently consumes a static trust document rather than performing OIDC
discovery:

```json
{
  "version": "vouch.oidc_trust.v0",
  "issuer": "https://login.example.com/",
  "audiences": ["vouch-production"],
  "clock_skew_seconds": 60,
  "max_token_lifetime_seconds": 3600,
  "principal_id_claim": "vouch_principal_id",
  "principal_kind_claim": "vouch_principal_kind",
  "namespace_claim": "vouch_namespaces",
  "roles_claim": "vouch_roles",
  "jwks": {"keys": [{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "…", "n": "…", "e": "AQAB"}]}
}
```

The custom claims must contain a Vouch principal ID, `human`, `service`, or
`operator` kind, one or more namespaces, and one or more roles from `viewer`,
`operator`, `approver`, or `admin`. Configure those claims in the enterprise
IdP. `vouch identity keygen` and `vouch identity issue` exist only for local
bootstrap and acceptance testing; the daemon is not an identity provider.

Configure `/etc/vouch/verifier-profiles.json`. Every profile in the document is
a mandatory verification gate for every production transaction:

```json
{
  "version": "vouch.verifier_profiles.v0",
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

Start the production daemon:

```sh
vouchd \
  --repo /srv/vouch/repository \
  --db /var/lib/vouch/kernel.db \
  --socket /run/vouch/vouchd.sock \
  --transaction-root /var/lib/vouch/transactions \
  --runtime-profile production \
  --runtime-engine /usr/bin/docker \
  --allowed-images 'registry.example/agent@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef' \
  --verifier-profiles /etc/vouch/verifier-profiles.json \
  --approval-trust /etc/vouch/approval-trust.json \
  --identity-trust /etc/vouch/identity-trust.json \
  --allowed-git-refs 'refs/heads/agent-release/*'
```

The documented production profile runs the daemon as a dedicated non-root
account and uses that same numeric UID/GID for agent and verifier containers.
Linux bind-mounted worktrees and broker receipt directories must have the same
owner. Vouch refuses a mismatched non-root production configuration rather
than starting containers that cannot write their staged state. A root-run
daemon configuration is outside this narrow productionized boundary.

Set the caller's short-lived access token for every CLI process:

```sh
export VOUCH_IDENTITY_TOKEN='eyJ…'
vouch tx list --socket /run/vouch/vouchd.sock --namespace engineering
```

The actor ID and kind passed by a mutation command must match the token.
Approval commands additionally require the `approver` role and a signed
approver whose issuer exactly matches the OIDC issuer.

For hosted-model agents, preload the digest-pinned broker image, provide the
provider secret only to the daemon, and add:

```sh
export OPENAI_API_KEY='provider-secret-visible-only-to-vouchd'
vouchd \
  … \
  --model-broker-image 'registry.example/vouch-model-broker@sha256:…' \
  --model-broker-policy /etc/vouch/model-policy.json \
  --model-provider-token-env OPENAI_API_KEY
```

The initial broker supports only OpenAI-compatible `POST /v1/responses`. Its
policy must use an HTTPS upstream and `force_store_false: true` in production.
The agent sees a transaction-scoped broker credential as `OPENAI_API_KEY` and
the internal broker URL as `OPENAI_BASE_URL`; it never sees the provider key.

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
image="$(scripts/vouchproductionfixture.sh --tag vouch-production-fixture:acceptance)"
VOUCH_PRODUCTION_IMAGE="$image" scripts/vouchproductionbench.sh
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
files. Set `VOUCH_PRODUCTION_KEEP=1` only when those artifacts are needed for
debugging.

## Backup and restore

Treat the following as one recovery set:

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

For a simple consistent backup, stop the daemon cleanly, verify no `vouchd`
process owns the database lock, then snapshot the complete recovery set. Do not
copy only the SQLite main file while the daemon is running; committed pages may
still be in the WAL.

Restore the complete set to the same paths and start one daemon. Startup
verifies every transaction event chain before recovery. If an agent execution
was active, Vouch removes its deterministic container and appends an
`interrupted` receipt. A cleanup or ledger-integrity failure prevents startup.

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
- A trusted dedicated daemon account and same-UID container boundary. Vouch
  does not protect against a malicious host administrator, OCI-engine operator,
  or identity able to replace configured parent paths.
- Mandatory OS byte and inode quotas covering the source Git object database,
  SQLite/WAL, bind-mounted transaction worktrees, immutable verifier
  materializations, and evidence. Host filesystem exhaustion is not contained
  by OCI resource flags.
- One deeply implemented release connector: compare-and-swap updates of local
  Git refs. There is no push, pull-request merge, deployment, or production
  database connector.
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
- Legacy client-supervised execution/verification mutation APIs are disabled
  in production, and the mediated filesystem API cannot access `.git` or
  `.vouch` control state.
- No claim of universal rollback. The implemented commit primitive is an
  atomic, version-checked Git-ref update.

Within those limits and after all mandatory gates pass, the single-node,
single-tenant Git runtime is the productionized profile. The compiler's wider
product surface, additional effect connectors, enterprise administration,
multi-tenancy, and HA remain beta.
