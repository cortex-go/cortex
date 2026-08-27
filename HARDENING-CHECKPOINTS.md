# Cortex battle-hardening campaign

This programme turns Cortex's existing controls into tested, documented
guarantees. It is deliberately smaller than Warden's campaign: Cortex does not
ship a terminal, editor, Git client, system administration, website management,
multi-account authorization or OS-user provisioning. Its narrower authority is
still substantial because an authenticated browser can launch an auto-authorized
coding agent inside a selected workspace.

The checkpoints are sequential. A checkpoint is complete only when product
changes, adversarial evidence, public documentation and repository commits are
all complete. Planned work must never be presented as completed evidence.

## Campaign rules

For every checkpoint:

1. Start from clean Cortex, website-source and generated-website repositories.
2. State the threat, trusted inputs, hostile inputs and invariant being tested.
3. Add a regression test before or with every repair.
4. Run focused tests, the full Go suite, race tests, vet, a production build,
   frontend checks and Nift status.
5. Update `docs/battle-tested`, affected feature documentation and retained
   risks. Claims are evidence-backed and identify platform limitations.
6. Rebuild both Nift projects and validate generated links and assets.
7. Commit generated website output first, website source second and Cortex last.
8. Record commands, results, commit IDs, retained risks and follow-up work.

No crash, race, authorization bypass, workspace escape, secret disclosure,
orphaned process, corrupt recovery or fuzz finding is dismissed without a
reproducible explanation and retained regression.

## Evidence required throughout

- `go test ./...`
- `go test -race ./...` on supported race-enabled hosts
- `go vet ./...`
- clean production build
- JavaScript syntax and frontend smoke checks
- `nift build` and current `nift status` for application and website
- generated-site link and asset validation
- clean repository status and inspected ignored outputs
- no credentials, binaries, caches, databases or private evidence committed

## CX00 - Baseline, threat model and evidence ledger

### Cortex

- Inventory every HTTP route, cookie, OAuth exchange, filesystem read,
  subprocess, provider credential, SQLite table and persistent file.
- Model browser, reverse proxy, Cortex, workspace root, OpenCode subprocess,
  provider and host credential-store trust boundaries.
- Create machine-readable route and invariant registries plus a retained-risk
  ledger; fail tests when an authoritative surface is unclassified.
- Capture baseline test, race, vet, build, dependency and frontend evidence.

### Website

- Publish Battle Tested with campaign scope, evidence rules and an honest CX00-
  CX11 ledger.
- Reconcile homepage, Security, Agent, Provider and Remote-use claims with the
  threat model and remove stale claims.

### Exit evidence

- Every authoritative surface is classified.
- Baseline commands and toolchain versions are reproducibly recorded.
- Known risks have owners, acceptance conditions or public limitations.

## CX01 - HTTP, browser and request parsing boundary

### Cortex

- Apply server read-header, read, write and idle timeouts without breaking
  deliberate NDJSON streaming.
- Bound every request body, response, query, header and collection; reject
  trailing JSON, wrong content types, unsupported methods and encodings.
- Assert CSP, MIME, frame, referrer, cache and download headers on success,
  authentication errors, redirects and handler failures.
- Exercise slow bodies, malformed JSON, disconnects and panic containment.

### Website

- Document request and stream limits and browser-response protections.
- Add exact corpus counts and retained compatibility limits to Battle Tested.

### Exit evidence

- The malformed-input corpus causes no panic, leak or unbounded operation.
- Route-wide security-header assertions cover success and failure responses.

## CX02 - Password, sessions, TOTP and OAuth

### Cortex

- Review password derivation, minimums, rehash migration and constant-time
  verification; test concurrent first-run setup.
- Add bounded login throttling and exercise IPv4/IPv6 identity handling.
- Test fixation resistance, renewal, expiry, logout, password change, restart
  semantics and global revocation.
- Test TOTP enrollment expiry and replay, concurrent enable/disable and secret
  cleanup; test OAuth state, PKCE, callback replay, redirect binding, provider
  failures, email normalization and account binding.

### Website

- Document the exact session, MFA, OAuth and recovery model.
- Publish authentication matrix evidence and provider-dependent limitations.

### Exit evidence

- Concurrent setup produces one valid owner configuration.
- Expired, replayed and revoked credentials cannot regain authority.

## CX03 - CSRF, origins, hosts and reverse-proxy trust

### Cortex

- Require CSRF protection on cookie-authenticated mutations and test missing,
  duplicated, stale and cross-site tokens.
- Validate Origin and Host for local and configured remote deployments.
- Parse forwarded headers only from explicitly trusted direct peers; fuzz
  forwarded chains, malformed addresses and scheme confusion.
- Exercise documented Caddy and nginx configurations with real streaming,
  cookies, redirects and disconnects.

### Website

- Update Caddy, nginx, Remote use and Security with verified configurations and
  explicit trust assumptions.
- Add the proxy/origin matrix to Battle Tested.

### Exit evidence

- Direct clients cannot manufacture proxy trust or cross-site mutations.
- Both documented proxy stacks preserve secure cookies and NDJSON streams.

## CX04 - Workspace and filesystem confinement

### Cortex

- Build a traversal corpus covering absolute and relative paths, alternate
  separators, symlink chains, dangling links, replacement races, hard links,
  case behaviour, device files, FIFOs and permission failures.
- Revalidate paths at use time and define behaviour if the configured root or a
  selected workspace is replaced after selection.
- Bound directory listings and file reads by entry count, file size and time.
- Verify conversation restoration cannot smuggle an out-of-root workspace.

### Website

- Document the tested root boundary, symlink policy, read limits and the fact
  that OpenCode retains process authority inside the selected workspace.
- Add platform-specific corpus evidence to Battle Tested.

### Exit evidence

- No corpus case reads or launches work outside the configured root.
- Race cases fail closed or preserve a documented safe invariant.

## CX05 - Provider credentials, configuration and secret lifecycle

### Cortex

- Replace or formally justify plaintext-at-rest provider configuration; verify
  file permissions, atomic writes, corruption handling and recovery boundaries.
- Test secret redaction across HTTP errors, OpenCode logs, streams, persisted
  conversations, process inspection and exported diagnostics.
- Validate provider and model identifiers, OAuth credential copying, environment
  construction and per-session data-directory isolation.
- Exercise configuration update races, partial writes and key removal.

### Website

- Explain credential storage, process exposure, OAuth reuse and backup/recovery
  limitations precisely.
- Add secret-leak corpus and configuration recovery evidence to Battle Tested.

### Exit evidence

- Secrets do not enter public settings, logs, streams or conversation history.
- Corrupt configuration fails safely without silently discarding credentials.

## CX06 - Agent subprocess authority and lifecycle

### Cortex

- Audit fixed executable selection, argument arrays, environment filtering,
  working directory and OpenCode configuration generation.
- Add explicit run ownership, bounded concurrency and cancellation endpoints.
- Terminate process groups on Stop, logout, disconnect, timeout and shutdown;
  prove no tested lifecycle leaves an orphan.
- Exercise missing binaries, spawn failure, hung children, large stderr, signals,
  abrupt network loss and concurrent runs.

### Website

- Document auto-authorized OpenCode authority, supervision responsibilities,
  concurrency limits, cancellation and shutdown behaviour.
- Publish subprocess lifecycle and orphan checks on Battle Tested.

### Exit evidence

- User-controlled values never cross a shell parser.
- Every tested lifecycle reaches a durable terminal state with no orphan process.

## CX07 - Hostile provider streams and frontend rendering

### Cortex

- Treat provider NDJSON, tool events, Markdown, restored text and error strings as
  hostile structured input.
- Bound scanner tokens, total output, event counts, nesting, Markdown work and
  retained DOM; define truncation without corrupting the stream protocol.
- Test forged tool events, embedded HTML, dangerous links, Unicode controls,
  malformed JSON, partial lines and prompt-output confusion.
- Verify model text cannot create trusted execution state or reveal credentials.

### Website

- Document rendering and stream limits, trusted versus untrusted UI states and
  prompt-injection non-guarantees.
- Add hostile-stream and browser corpus evidence to Battle Tested.

### Exit evidence

- The corpus produces no script execution, trusted-event forgery or unbounded UI.
- Truncation and malformed streams leave conversations understandable.

## CX08 - Durable conversations, SQLite and accounting integrity

### Cortex

- Test transactional conversation/event/run writes, restart recovery, duplicate
  IDs, concurrent saves, stale clients and interrupted imports.
- Bound list/search/import/export work and add cursor pagination where required.
- Validate migrations from every supported schema, future-schema refusal,
  corruption diagnostics, WAL handling and clean shutdown.
- Reconcile token/cost accounting across streamed, recovered, retried, failed and
  cancelled runs without presenting estimates as provider billing facts.

### Website

- Expand Conversations & data with consistency, migration, limits, backup and
  accounting semantics.
- Publish database/recovery matrix evidence on Battle Tested.

### Exit evidence

- Failure injection never exposes a partially imported conversation.
- Restart recovery deterministically resolves interrupted runs.

## CX09 - Concurrency, resource exhaustion and long-lived operation

### Cortex

- Stress authentication, settings, workspace listing, conversations and agent
  runs under the race detector and realistic parallel load.
- Bound sessions, OAuth states, temporary directories, subprocesses, goroutines,
  streams, database connections and browser-retained history.
- Run disconnect/reconnect, cancellation storms and long-lived soak tests while
  measuring memory, descriptors and cleanup.
- Define overload responses and prove recovery after pressure is removed.

### Website

- Document quotas, overload behaviour and sizing assumptions.
- Add race, load and soak evidence with environment details to Battle Tested.

### Exit evidence

- Resource use remains bounded by documented controls.
- No observed leak, race or deadlock remains unexplained.

## CX10 - Installation, updates, releases and deployment matrix

### Cortex

- Harden install and update scripts against partial downloads, wrong platforms,
  checksum mismatch, redirects, permissions and interrupted replacement.
- Build release artifacts for all declared targets and install each into clean
  consumers without repository-relative dependencies.
- Verify version synchronization, checksums, archive contents, executable modes,
  startup, upgrade and rollback; audit repository history for binaries/secrets.
- Exercise loopback, Caddy and nginx deployment on supported host classes.

### Website

- Keep installation, update, remote and proxy instructions synchronized with
  tested artifacts and supported platforms.
- Publish artifact and deployment evidence on Battle Tested.

### Exit evidence

- Every release artifact installs, starts and upgrades from a clean environment.
- Failed verification cannot replace the installed executable.

## CX11 - Adversarial whole-product closure

### Cortex

- Fuzz parsers and boundary helpers; run full race, vulnerability, dependency,
  static-analysis, filesystem, browser and end-to-end corpora.
- Perform an integrated attack exercise across proxy, auth, workspace, secrets,
  provider stream, subprocess and persistence boundaries.
- Run backup/restore and clean-machine deployment drills and close or publish
  every retained risk.
- Freeze the initial stable security contract and CI gates.

### Website

- Convert Battle Tested from a campaign ledger into a release evidence page with
  exact scope, platforms, commands, limitations and last-verified version.
- Audit all public pages against the final product and remove stale claims.

### Exit evidence

- The complete gate passes from clean source and clean deployment environments.
- Every material claim links to reproducible evidence or a stated limitation.

## Completion record

Append one record per checkpoint when it is completed:

```text
Checkpoint:
Threat and invariant:
Cortex commit:
Website source commit:
Generated website commit:
Focused evidence:
Full gates:
Platforms/environment:
Findings repaired:
Retained risks:
Public claims changed:
```

### CX00 completion

```text
Checkpoint: CX00
Threat and invariant: all authoritative HTTP surfaces must be classified
Cortex commit: this checkpoint commit
Website source commit: e22f4df
Generated website commit: f45feb0
Focused evidence: 19 unique classified routes; five deliberate public routes
Full gates: Go test, race and vet passed; website 14/14 current
Findings repaired: ad hoc registration; logout no longer bypasses session middleware
Retained risks: CX01-CX11
Public claims changed: Battle Tested and Security now distinguish inventory from proof
```

### CX01 completion

```text
Checkpoint: CX01
Threat and invariant: malformed or slow HTTP input must remain bounded
Cortex commit: this checkpoint commit
Website source commit: 1279ea4
Generated website commit: 47bc7aa
Focused evidence: timeout, method, content type, JSON, header and panic tests
Full gates: Go test, race, vet and production build passed; website 14/14 current
Findings repaired: unbounded default server; permissive methods and trailing JSON
Retained risks: stream/process bounds are CX06-CX09
Public claims changed: exact HTTP limits and streaming exception documented
```

### CX02 completion

```text
Checkpoint: CX02
Threat and invariant: authentication replay, races and state growth must fail closed
Cortex commit: this checkpoint commit
Website source commit: c9479d1
Generated website commit: 862e1a0
Focused evidence: setup race, sessions, password rotation, throttle, TOTP and OAuth
Full gates: Go test, race and vet passed; website 14/14 current
Findings repaired: setup race, unbounded sessions/state, replay and public identity leak
Retained risks: trusted proxy client identity and CSRF are CX03
Public claims changed: exact session, TOTP and OAuth lifecycle documented
```

### CX03 completion

```text
Checkpoint: CX03
Threat and invariant: cross-site and forged proxy input cannot gain authority
Cortex commit: this checkpoint commit
Website source commit: d753bc3
Generated website commit: 2ba0fd7
Focused evidence: CSRF, Origin, Host, forwarded scheme/client and browser integration
Full gates: Go test, race, vet, build and both Nift projects passed
Findings repaired: no CSRF token; implicit loopback proxy trust; unpinned public Host
Retained risks: clean-host deployment matrix is repeated in CX10
Public claims changed: Caddy, nginx, Remote use, Security and Battle Tested updated
```

### CX04 completion

```text
Checkpoint: CX04
Threat and invariant: symlinks and restored paths cannot escape the canonical root
Cortex commit: 8bd86f7
Website source commit: 48fa46b
Generated website commit: 9f16da0
Focused evidence: lexical traversal, external/internal symlinks and restored workspace tests
Full gates: Go test, race, vet, build and both Nift projects passed
Findings repaired: lexical-only confinement; unbounded directory listing; special-file previews
Retained risks: the root is an application boundary, not an OS sandbox
Public claims changed: Agent and Battle Tested document exact path and preview limits
```

### CX05 completion

```text
Checkpoint: CX05
Threat and invariant: corrupt configuration and diagnostics cannot discard or disclose secrets
Cortex commit: 063f158
Website source commit: b5b5192
Generated website commit: 05d2251
Focused evidence: owner-only mode, corruption refusal, identifier validation and multi-secret redaction
Full gates: Go test, race, vet, build and both Nift projects passed
Findings repaired: ignored load errors; partial redaction; unbounded key/model input
Retained risks: host-account processes can inspect memory and environment
Public claims changed: Providers and Battle Tested define storage and process exposure
```

### CX06 completion

```text
Checkpoint: CX06
Threat and invariant: supervised agent children remain bounded, cancellable and owned
Cortex commit: e678226
Website source commit: f4f4687
Generated website commit: 8434b4d
Focused evidence: run registry cleanup, cancellation, concurrency cap and Windows build
Full gates: Go test, race, vet, native/Windows build and both Nift projects passed
Findings repaired: unlimited runs; request-only ownership; Unix direct-child-only cancellation
Retained risks: Windows direct-child cancellation; deliberate auto-authorization
Public claims changed: Agent and Battle Tested document four-run cap and supervision
```

### CX07 completion

```text
Checkpoint: CX07
Threat and invariant: hostile provider output cannot create unbounded or trusted browser state
Cortex commit: this checkpoint commit
Website source commit: e43cff9
Generated website commit: 645afae
Focused evidence: pinned byte/event/line bounds, diagnostic bounds and safe renderer smoke
Full gates: Go test, race, vet, build, JavaScript and both Nift projects passed
Findings repaired: 4 MiB line and unbounded aggregate stream; trusted-looking provider tool state
Retained risks: limits do not make model output truthful or defeat prompt injection
Public claims changed: Agent and Battle Tested document exact hostile-stream boundary
```
