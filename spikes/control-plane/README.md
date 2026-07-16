# Control-plane runtime spike

This spike compares Go and TypeScript/Node control-plane candidates through one black-box HTTP/SSE contract. It does not select a runtime by familiarity: the committed report records behavior, readiness, shutdown, restart and resident-memory evidence for both candidates.

## Candidate contract

Both processes:

- bind only to `127.0.0.1` and accept `--port=0` for an OS-assigned port;
- write exactly one JSON readiness line to stdout after the listener is active;
- write human diagnostics to stderr;
- return `200` from `GET /healthz` after readiness;
- stream retained fixture events `1` and `2` from `GET /events`;
- resume after `Last-Event-ID: 1` with event `2` only;
- keep streams open with a 15-second heartbeat comment by default;
- expose aggregate, content-free counters at `GET /stats`;
- treat an SSE disconnect as transport state, never as a job-cancel request; and
- close active streams and the listener on `SIGTERM` within five seconds.

The Go candidate uses only `net/http`. The Node candidate is strict TypeScript compiled with the pinned workspace compiler and uses only typed `node:http` APIs. The harness has no runtime-specific HTTP path.

## Profiles

Run the bounded profile used by normal verification:

```bash
scripts/spikes/control-plane-runtime quick
```

The quick profile opens 25 retained streams, performs exactly 10 explicit reconnects and completes one restart for each candidate.

Generate full evidence from a clean source commit:

```bash
PALAI_SPIKE_REPORT_OUT=spikes/.evidence/control-plane-runtime.json \
  scripts/spikes/control-plane-runtime evidence
```

The evidence profile opens 1,000 streams, performs exactly 100 explicit reconnects and completes one restart per candidate. It allows zero sequence gaps, duplicates, request errors or disconnect-driven cancel requests. Readiness, idle/connected RSS and shutdown duration are recorded without PIDs, ports, hostnames, absolute paths, request content or credentials.

The raw `.evidence` directory is ignored. A redacted report is promoted to `spikes/reports/control-plane-runtime.json` only after the source commit is clean and the report validator confirms its Git tree identity.
