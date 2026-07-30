# Security Policy

Vouch is security-sensitive infrastructure. Reports that could let an agent
escape its transaction boundary, forge authority, bypass verification, or leak
credentials are especially important.

## Supported versions

Vouch does not yet publish versioned stable releases. Security fixes are
applied to the latest code on `main`; older commits and development branches
should be treated as unsupported.

The only supported production deployment profile is the mandatory-gate-tested,
single-node, single-tenant Vouch Runtime that publishes to allowed local Git
refs. Vouch Contracts is an optional verification module. The future Vouch
Control Plane, enterprise connector drivers, multi-tenancy and HA are not
implemented production claims. There is no production push, merge or deployment
connector driver.
Mandatory gates include reachable Go vulnerability scanning and
VouchRuntimeBench in addition to test, vet, race, compiler/kernel/transaction
benchmarks, and production OCI acceptance.

## Reporting a vulnerability

Do not open a public issue for a suspected vulnerability.

Report it privately through
[GitHub Security Advisories](https://github.com/duriantaco/vouch/security/advisories/new).
Include:

- The affected commit, command, resource, or deployment profile.
- Reproduction steps or a minimal proof of concept.
- The expected and observed security boundary.
- The likely impact and any known prerequisites.
- Whether the issue is already public or actively exploited.

If GitHub Security Advisories is unavailable, contact the repository owner
privately through the contact method on their GitHub profile and ask for a
secure reporting channel. Do not include exploit details in the initial public
message.

We target an acknowledgement within three business days and an initial
assessment within seven business days. These are response targets, not a
service-level agreement. We will coordinate status updates and disclosure
timing with the reporter.

## Security scope

High-priority reports include:

- OCI, filesystem, worktree, or network-boundary escapes.
- OIDC, namespace, role, capability, or approval bypasses.
- Forged or replayed events, receipts, evidence, or signatures.
- Mutation after verification or approval without invalidation.
- Credential, prompt, model-provider token, or sensitive evidence disclosure.
- Unsafe retry or reconciliation of ambiguous non-idempotent effects.
- Git release outside the frozen and approved target.
- A nonempty ledger changing between development and production enforcement
  profiles, or production accepting a nonempty unmarked legacy ledger.
- Prepared authority surviving a change to identity trust, approval trust,
  verifier profiles, enforcement profile, or verifier OCI runtime policy.
- Approval by a transaction participant, or release by the same identity that
  approved the transaction.
- Bypass of verifier output, Git-inspection, or global OCI-workload limits.
- A verifier receiving bytes other than the exact frozen Git tree, accepting a
  gitlink/submodule, or exceeding tree scan/materialization bounds.
- Production acceptance of client-supervised execution/verification mutation,
  or mediated filesystem access to `.git`, `.gatemole`, or the permanently
  reserved legacy `.vouch` control-state namespace.

Vouch's documented deployment limits remain relevant when assessing a report.
For example, direct host access granted outside Vouch is not considered a
sandbox escape by Vouch itself, but a way to obtain that access through the
documented production boundary is.

## Production trust boundary

The narrow supported profile requires a dedicated host or VM, one security
tenant, a dedicated non-root daemon account, and agent/verifier containers
using the same numeric UID and GID. The repository, OCI engine, daemon
executable, runtime state, and parent directories of every trust/config file
are trusted operator-controlled inputs. There is no HA or multi-tenant
isolation claim.

Dedicated quota-controlled storage must apply operating-system byte and inode
limits to the source repository and Git object database, SQLite/WAL,
transaction worktrees, immutable verifier materializations, and evidence. The
agent workspace is a writable host bind mount; container memory, tmpfs, PID,
and image-layer limits do not prevent host filesystem exhaustion.

Staging writes a private Git index to an immutable tree, and each verifier gets
a separate read-only directory materialized by object ID from that exact tree.
Scanning and materialization fail above 100,000 entries, 256 MiB per blob, or
1 GiB aggregate, and gitlinks/submodules are rejected. The Git executable,
source object database, host, and materialization parent remain trusted
operator-controlled inputs.

## Coordinated disclosure

Please allow time to reproduce the issue, prepare a fix, and notify affected
users before publishing details. We will credit reporters who want attribution.
This project does not currently operate a bug-bounty program.
