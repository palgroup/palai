# Security Policy

## Supported versions

Palai has not published a stable runtime release yet. Security fixes currently
target the `main` branch. A supported-release table will be published before the
first self-host stable release.

## Private vulnerability reporting

Use GitHub's **Security → Report a vulnerability** flow for
[`palgroup/palai`](https://github.com/palgroup/palai/security/advisories/new).
Do not open a public issue or discussion for a suspected vulnerability.

Include affected commit or version, deployment assumptions, reproduction steps,
impact, and suggested mitigations if known. Use synthetic data only. Never send
real provider keys, customer credentials, access tokens, private repository
content, or production database extracts.

Maintainers will acknowledge a complete report, coordinate validation and a
fix, and disclose it after affected users have a reasonable remediation path.
Response-time commitments will be added with the stable support policy.

## Scope

Control-plane authentication and tenant isolation, runner enrollment, sandbox
escape, secret handling, provider/tool brokers, artifact access, recovery and
replay, dependency integrity, and release provenance are in scope. Availability
reports must demonstrate an impact beyond ordinary local resource exhaustion.

