# Runner transport and OCI supervisor spike

This spike proves the E01 runner boundary against real TLS handshakes and a real
Docker Engine. It is not the production runner daemon.

The controller accepts only TLS 1.3 clients signed by the configured CA and with
the exact runner DNS SAN. The runner owns no inbound listener configuration and
dials the controller with `wss`. Lease offers carry a fenced run/attempt identity,
an immutable image ID and digest, a deadline, and explicit resource/output limits.

The supervisor uses Moby client 0.5.0 with API negotiation. It creates a
network-disabled, read-only, non-privileged container by immutable image ID,
drops every Linux capability, applies memory/process/time bounds, and supplies
only the fixture mode plus run and attempt IDs. It never inherits the host
environment and never mounts the Docker socket or runner key.

Stdout and stderr are demultiplexed into independent bounded buffers. Stdout is
strict JSONL protocol data: malformed, trailing, unknown-field, oversized, or
out-of-sequence frames fail the attempt. Stderr truncation is recorded without
corrupting stdout. Every exit path force-removes the labeled container with a
separate bounded cleanup context.

Run the one-iteration development profile:

```bash
scripts/spikes/runner quick
```

From a clean source commit, run five repetitions and create raw evidence:

```bash
PALAI_SPIKE_REPORT_OUT=spikes/.evidence/runner-supervisor.json \
  scripts/spikes/runner evidence
```

The script cross-compiles the fixture engine for the Docker daemon architecture,
builds a `FROM scratch` image, retains exactly one labeled fixture image as an
intentional cache, and requires zero labeled containers after the run.
