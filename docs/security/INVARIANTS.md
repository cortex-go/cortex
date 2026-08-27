# Cortex security invariants

| ID | Invariant | Checkpoint |
| --- | --- | --- |
| CTX-ROUTE-001 | Every `/api/` route has a unique boundary and method policy. | CX00 verified |
| CTX-AUTH-001 | Only setup, login state and OAuth initiation/callback are public APIs. | CX00 verified; lifecycle CX02 |
| CTX-HTTP-001 | Request parsing and response work are bounded. | CX01 planned |
| CTX-CSRF-001 | A cross-site request cannot mutate authenticated state. | CX03 planned |
| CTX-PROXY-001 | Forwarding headers affect authority only through configured proxy trust. | CX03 planned |
| CTX-ROOT-001 | A browser-selected workspace cannot escape the configured root. | CX04 planned |
| CTX-SECRET-001 | Credentials do not enter public responses, logs or conversations. | CX05 planned |
| CTX-PROC-001 | Agent lifecycle termination leaves no orphan OpenCode process. | CX06 planned |
| CTX-STREAM-001 | Provider output cannot forge trusted UI state or execute browser code. | CX07 planned |
| CTX-DATA-001 | Conversation and run state is transactional and recoverable. | CX08 planned |

