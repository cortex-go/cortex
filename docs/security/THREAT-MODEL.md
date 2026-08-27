# Cortex threat model

Last verified: CX00, 2026-08-27.

## Authority and assets

Cortex runs with the operating-system authority of its process account. Its
primary assets are the configured workspace tree, provider and OAuth
credentials, authentication material, conversation history, SQLite state and
the integrity of OpenCode subprocess execution.

## Trust boundaries

1. The browser is untrusted until a Cortex session is established. Browser
   input, restored state and rendered model output remain hostile afterward.
2. A reverse proxy is not trusted merely because forwarding headers exist.
   Proxy trust is examined in CX03.
3. Cortex is trusted to enforce authentication, root selection, request limits,
   secret handling and subprocess construction.
4. OpenCode is an external executable with automatic tool permission inside the
   selected workspace. It is not a sandbox and may exercise the process
   account's reachable OS authority.
5. Provider responses are hostile structured input. Provider availability,
   model behaviour and billing are external dependencies.
6. SQLite and the data directory are process-private persistent state. A host
   administrator who can read the process account's files is outside Cortex's
   application security boundary.

## Starting retained risks

- Cookie-authenticated mutations do not yet have an explicit CSRF token; CX03.
- Forwarded scheme trust is implicit for loopback peers rather than explicitly
  configured; CX03.
- Sessions are memory-only and unbounded; lifecycle hardening is CX02.
- Provider keys are stored in an owner-readable settings file; secret lifecycle
  is CX05.
- Workspace resolution is lexical and needs symlink/race adversarial proof;
  CX04.
- OpenCode process-group cancellation and resource limits are not yet proven;
  CX06 and CX09.

