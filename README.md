# Palai

Palai is a provider-neutral, self-hostable agent execution platform. It exposes one durable execution kernel through Responses, Sessions, reusable Agents, and interoperability adapters.

The current repository contains the product specification under review and the implementation plans derived from it. Runtime code has not started yet.

## Development

The reference toolchain is pinned in `.tool-versions`. Bootstrap and run the
same foundation checks used by CI:

```bash
make bootstrap
make verify
```

Provider credentials are not required for foundation checks. Never place a
provider key in the repository, a command argument, or committed evidence; the
local CLI will accept credentials through a write-only bootstrap path when the
model-broker phase is available.

## Documents

- [Product and architecture specification](MASTER-SPEC.md)
- [Self-hosted implementation master plan](docs/superpowers/plans/2026-07-16-self-hosted-master-plan.md)
- [First local live-proof implementation plan](docs/superpowers/plans/2026-07-16-local-live-proof.md)

## Initial delivery target

The first milestone is a complete local stack proven against a real model provider and consumed from a Next.js application through the TypeScript SDK. Managed SaaS product work is intentionally outside the current implementation plan.

## License

Apache License 2.0. See [LICENSE](LICENSE).
