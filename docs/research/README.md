# Research

Measured investigations that outlived the conversation that produced them. Each answers ONE
question, states its verdict first, and separates what was measured from what was read.

| Document | Question it answers | Verdict in one line |
|---|---|---|
| [`macos-session-isolation.md`](./macos-session-isolation.md) | How should Palai isolate agent sessions that need Xcode and the iOS Simulator? | `sandbox-exec` is out (a live escape was reproduced here), macOS VMs are capped at two per host by kernel AND licence, so the boundary is a uid. |
| [`macos-isolation-without-accounts.md`](./macos-isolation-without-accounts.md) | Can we skip the per-account machinery — hidden volumes, ACLs, the App Sandbox? | No. 22 measurements on this host; Apple's SUPPORTED App Sandbox was escaped via `simctl spawn`. Different customers → different Macs; same customer → `simctl --set` and separate directories. |
| [`deployment-and-product-shape.md`](./deployment-and-product-shape.md) | Where does a self-hoster deploy Palai, and where would our SaaS run? | The question was premature: **nothing is published anywhere**, so the only way to run Palai is to clone this repo and build it. |

**Reading order for someone new:** start with `deployment-and-product-shape.md` — it is about
the product rather than a subsystem, and its sequencing section is the honest list of what
stands between today and a stranger running Palai on their own infrastructure.
