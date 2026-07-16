# Palai

Palai is a provider-neutral, self-hostable agent execution platform. It exposes one durable execution kernel through Responses, Sessions, reusable Agents, and interoperability adapters.

The repository has completed its E01 technology baseline: executable spike
reports select the implementation toolchain before production runtime work
starts. This evidence accepts technology decisions only; it does not claim that
the LP-0 local stack or a self-host release is complete.

## Development

The reference toolchain is pinned in `.tool-versions`. Bootstrap and run the
same foundation checks used by CI:

```bash
make bootstrap
make verify
bash scripts/verify/e01.sh
```

Provider credentials are not required for foundation checks. Never place a
provider key in the repository, a command argument, or committed evidence; the
local CLI will accept credentials through a write-only bootstrap path when the
model-broker phase is available.

## Documents

- [Product and architecture specification](MASTER-SPEC.md)
- [Self-hosted implementation master plan](docs/superpowers/plans/2026-07-16-self-hosted-master-plan.md)
- [First local live-proof implementation plan](docs/superpowers/plans/2026-07-16-local-live-proof.md)
- [Accepted architecture decisions](docs/adr)
- [Checksummed E01 report index](spikes/reports/index.json)

## Initial delivery target

The first milestone is a complete local stack proven against a real model provider and consumed from a Next.js application through the TypeScript SDK. Managed SaaS product work is intentionally outside the current implementation plan.

## License

Apache License 2.0. See [LICENSE](LICENSE).
