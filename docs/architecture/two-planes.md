# Two planes: the API edge and the runner gateway

A Palai deployment listens on **two** surfaces, and confusing them is the source of most "why can my Mac
not join" and "why is my database reachable" questions. They carry different traffic, authenticate
differently, and one of them is the only thing a fleet machine ever talks to.

This page exists because a reader could previously reach two opposite wrong conclusions from the same
repository: that a remote Mac already works end to end, or that remote execution is absent entirely. Every
row below cites something that resolves against this tree — a Go test or a UAT case id — and
`TestTwoPlanesEvidenceResolves` fails if one stops existing.

## The two surfaces

| Surface | Who talks to it | How it authenticates | Evidence |
|---|---|---|---|
| **API edge** — tenant traffic: sessions, responses, artifacts, the console and the SDKs | People and programs, over the public internet | API keys, project-scoped | `TestEdgeSurfaceHoldsForTheShippedOverlay` |
| **Runner gateway** — the fleet: enrolment, the session websocket, settings polls | Machines only, outbound | mTLS. The machine proves a keypair it generated and never sent; the certificate is issued at enrolment | `TestAMachineWithNowhereToWriteIsRefusedBeforeTheGatewayIsCalled` |

The edge proxies `/v1/*` and nothing else — no `/metrics`, no `/healthz`, no database, no object store. A
production overlay that publishes any other host port is refused before it starts
(`TestEdgeSurfaceRejectsUnresetPorts`, `TestEdgeSurfaceRejectsCatchAllProxy`).

## A device dials out and is never dialled

A fleet machine opens a websocket to the runner gateway and keeps it. It listens on nothing:

| Claim | Evidence |
|---|---|
| The device binary opens no listener on any port | `TestTheDeviceOPENSNoListener` |
| The device binary links no server or admin implementation | `TestTheDeviceBinaryLinksNoServerOrAdminCode` |
| A machine's identity is a keypair it generated; the private half never leaves it | `TestTheDeviceKeyIsGeneratedOnceAndNeverAgain` |
| A machine that cannot hold a session's files never becomes capacity | `TestAMachineWithNowhereToWriteClaimsNOMode` |

This is why a Mac behind a home router joins a fleet with no port forwarding, and why nothing on the
internet can reach it.

## Where a run's work actually happens

**Tools execute on the machine that holds the attempt's lease.** Until Phase A.3 they ran in the control
plane's own process, which meant a Mac pool routed a run's *engine* to a Mac and ran `xcodebuild`
wherever the control plane was. That ceiling is closed and its history is recorded as `FLT-P15` in
[known-gaps-1.0.md](../operations/known-gaps-1.0.md).

| Claim | Evidence |
|---|---|
| A shell command runs on the machine and answers over the wire | `TestExecRequestRunsOnTheMachineAndAnswersOverTheWire` |
| The command runs under the session's own account, not the agent's | `TestTheSessionUidSurvivesTheWire` |
| Two concurrent sessions get disjoint session directories | `TestHostShellGivesConcurrentAllocationsDisjointSessionDirectories` |
| A session's record outlives the machine it ran on | `TestAFinishedSessionIsREADABLEAfterItsMachineIsGone` |

**What is NOT claimed:** no Mac pool has been driven from a control plane on *another host over public
DNS*. The contract has been proven on loopback (device plan milestone A0); publishing the gateway is
still open.

## Configuration flows one way

Nothing is edited on a machine. It is enrolled once with a URL and a pool key, and everything after that
is the admin plane's:

| Claim | Evidence |
|---|---|
| The pool comes from the enrolment key, and a machine naming another pool is refused | `TestPoolBirthReachesTheWaitingRoomAndTheApproveRoute` |
| A panel edit reaches a machine that is already running | `TestAPanelEditReachesAMachineThatIsAlreadyRunning` |
| The applied value is shown only after the machine says it applied it | `TestAPanelEditReachesAMachineThatIsAlreadyRunning` |
| The server's occupancy ceiling follows the concurrency the machine reported | `TestTheCeilingFollowsTheConcurrencyTheMachineSaysItApplied` |
| A machine at its ceiling refuses the next occupancy rather than counting it | `TestTheMachineCeilingIsCountedAcrossTenantsFromTheDatabase` |

## The plane never serves binaries

An agent is installed and updated from a release location, and it verifies what it downloaded against a
**separately served** checksum manifest. A control plane that handed out binaries would put "whatever my
plane served me" into every deployment's trust story.

| Claim | Evidence |
|---|---|
| A release serves every device the installer can resolve | `TestAReleaseServesEveryDeviceTheInstallerCanResolve` |
| A tampered archive installs nothing | `TestAnArchiveThatDoesNotMatchTheManifestInstallsNOTHING` |
| An upgrade leaves the machine's identity and config untouched | `TestAnUpgradeLeavesTheDEVICEIdentityUntouched` |

See [install.md](../operations/install.md) for the operator's side and
[runner-host.md](../operations/runner-host.md) for the package's contents.
