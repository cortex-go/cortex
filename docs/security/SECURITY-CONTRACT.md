# Cortex initial hardening contract

The CX00-CX11 campaign freezes these regression-backed boundaries:

- all HTTP routes are typed as public or session-authenticated;
- unsafe cookie requests require same-origin CSRF credentials;
- proxy forwarding is accepted only from an explicitly trusted direct loopback peer;
- filesystem targets must resolve within the canonical configured root;
- secrets remain server-side and are redacted from surfaced failures;
- agent runs are bounded, owned, cancellable and process-group supervised on Unix;
- provider streams and browser-retained history are bounded;
- conversation imports are transactional and interrupted runs recover deterministically;
- overload is explicit and recoverable;
- release archives are checksum-verified before atomic installation.

This contract is not an OS sandbox, a model truth guarantee, multi-node orchestration, or a promise that upstream OpenCode/provider behaviour is safe. Those retained boundaries must remain public until the architecture changes and new evidence replaces them.
