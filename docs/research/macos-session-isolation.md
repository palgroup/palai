# Isolated macOS environments for Palai agent sessions — decision document

**Date:** 2026-07-26 · **Host used for all local measurements:** M2 Pro, macOS 26.3 (25D125), 16 GiB, Xcode 26.6 (17F113), MacOSX26.5.sdk

---

## 1. THE ANSWER IN THREE SENTENCES

Give each agent session a whole Mac — from a pool of monthly-rented Apple Silicon boxes at roughly **$125–250/month each** — and build no isolation mechanism at all to start, because whole-machine separation is the only boundary here that is simultaneously airtight, licence-clean, and zero engineering. When utilisation makes that wasteful, buy density with **separate non-admin macOS user accounts** (each session auto-logged into its own GUI session), *not* VMs: accounts are uncapped, share one 39 GB Xcode+runtime install, and sit outside the SLA clause that specifically restricts *virtualised* copies used for "service bureau … relay service", whereas macOS VMs are hard-capped at **two concurrent guests per physical Mac** and would cap your unit economics forever. Reject `sandbox-exec`/seatbelt outright — we reproduced a live escape in which a sandboxed process denied all writes to `$HOME` writes there anyway via `simctl spawn booted`, and the only profile rule that closes it destroys Simulator control entirely.

---

## 2. THE OPTIONS

| | What it actually isolates | Can a hostile process cross it? | Does the Simulator work? | Resource cost | Operational complexity | Licensing risk |
|---|---|---|---|---|---|---|
| **macOS VM** (Virtualization.framework + Tart/Orka) | Full guest kernel, filesystem, process table, network (NAT). Real boundary. | Only via a VM escape. Strongest option on this axis. | **Probably — unverified by us.** Tart's image build runs `simctl` inside a guest; MacStadium Orka advertises "native Apple simulators" in VMs. Against: tart #702 (Metal APIs crashing the paravirtual GPU *and the VM*, FB10025528, went stale), tart #1032 "No GPU passthrough in MacOS guest?" open since 2025-02-27, Flutter #150169 crash frames in `MTLSimDriver…sendXPCMessageWithReplySync`. | ~64 GiB pull / 140 GB provisioned disk **per image** (Tart `disk_size = 140`); 12–16 GiB RAM per guest. **Max 2 guests/host.** | High. Host needs an unlocked `login.keychain` just to boot a guest (macOS 15+, undocumented). Guest needs auto-login anyway. Orka on Apple Silicon has **no stop/start/suspend/resume**. Save/restore is host-bound and rejects newer OS builds. Images are currently broken (see §5). | **Highest.** SLA §2B(iii) grants exactly two, for dev/test only, and expressly excludes "service bureau, time-sharing, terminal sharing, relay service". The tooling (Tart) is now `openai/tart` under **FSL-1.1-ALv2** with a Competing-Use clause, licensor = OpenAI. |
| **Separate macOS user accounts** | Home directories (mode bits), per-user `CoreSimulatorService` and device set, per-user keychain, per-user launchd domain. | **Yes, if it wants to.** `sudo` defeats it entirely (→ non-admin accounts, no sudo). Three local-root LPEs shipped in 2026 alone (CVE-2026-20614 fixed in 26.3; CVE-2026-28915 and CVE-2026-28951 fixed in **26.5**). Default `/Users/<u>` is `drwxr-x---` group `staff` with umask 022, so a second staff user traverses the top level — the real boundary is the 700 on `~/Library`, `~/.ssh`, `~/Documents`. | **Yes, structurally.** Measured: `CoreSimulatorService` is a per-user XPC job (owned by `salih`); of ~182 simulator processes, **exactly one** is root (`simdiskimaged`, shared). Concurrency across two logged-in users is untested — see §5. | ~640 MB RSS + ~179 processes per booted device (measured, drifting upward over hours). **One shared Xcode (4.0 GB) + shared runtimes (35 GB) serves every account.** 4–8 sessions on a 64 GB box. | Medium. Every session user needs an **Aqua GUI session** — console for one, Screen Sharing for the rest. Auto-login required. Account teardown via `sysadminctl -deleteUser` needs a secure-token admin credential if FileVault is on. | **Lowest.** No additional copies of macOS exist, so §2B(iii) and its service-bureau sentence — which govern *virtualised* copies — do not engage. Xcode agreement's "Authorized Developers" clause is the open question, not the macOS SLA. |
| **seatbelt / `sandbox-exec`** | Nothing useful for this workload. | **Yes — reproduced live on this host.** `(deny file-write* (subpath "/Users/salih"))` + `simctl spawn booted … > ~/file` → file written. Same profile without simctl → `Operation not permitted`, file never created. `(deny network*)` → `direct=000` but `via-simctl=200`. Closing it with `deny mach-lookup …CoreSimulatorService` → *"Simulator services will no longer be available."* **There is no middle setting.** | Yes — but only because the sandbox isn't containing it. | ~Free. | Low to build, worthless to run. Deprecated in the man page **dated 9 March 2017**; SBPL undocumented for third parties (Quinn, DevForums 661939: *"it would be unwise to build a product based on it"*); `apple/containerization#737` asking for a removal timeline opened 2026-05-12, still unanswered. Anthropic's own docs call macOS Seatbelt *"not a complete isolation boundary."* | None. |
| **Cloud Macs** | **Nothing, by itself.** This is a *sourcing* decision, not an isolation mechanism. One rented Mac running N sessions has exactly the problem you started with. One rented Mac *per session* isolates perfectly. | N/A — inherits whatever you run on it. | Yes on bare metal; same VM caveats if you use the provider's VM layer. | See §6. | Low (rent) to medium (fleet lifecycle, image drift, patch currency). | **You are fine as the lessee; you are not fine as a lessor.** §3.A(iii) requires the lessee have "sole and exclusive use and control of the Apple Software *and the Apple-branded hardware on which it is installed*"; §3.D lets a lessor virtualise only "a **single** instance … as a provisioning tool". Selling 2 VM slots on one box to 2 customers is not §3-compliant. Renting whole machines to yourself is. |

---

## 3. THE BINDING CONSTRAINTS

These hold regardless of what anyone prefers.

**C1 — Two concurrent macOS guests per physical Mac.** `VZErrorVirtualMachineLimitExceeded = 6` is present in the current SDK (verified on this host); Apple's docs Discussion reads *"This error occurs when starting a VM would exceed the system's limit on the number of simultaneously running virtual machines."* Apple staff, DevForums 729580 (May 2023): *"The limit of 2 virtual Macs is part of the macOS End-User License Agreement."* **Apple publishes the error, never the number.** The number 2 comes from third-party reproduction (eclecticlight 2022 and 2026-04-29; khronokernel 2023-08-08) plus the SLA's separate licence limit. It is a **kernel quota** (`hv_apple_isa_vm_quota` in XNU), not a framework check — khronokernel demonstrated 9 concurrent guests on an M2 Pro using a Kernel Debug Kit development kernel and `hv_apple_isa_vm_quota=0xFF`. That bypass costs SIP/boot-policy downgrade on **every** host and does not touch the licence cap, which is the real ceiling. The cap covers macOS guests; **Linux guests appear uncapped** (Oakley + observation; no Apple statement, and Apple's own error text says "virtual machines" guest-agnostically).

**C2 — The virtualisation grant is conditional and purpose-limited.** SLA §2B(iii), macOS Tahoe (**EA1955, 07/11/2025**), verbatim: *"to install, use and run up to two (2) additional copies or instances of the Apple Software … within virtual operating system environments on each Apple-branded computer you own or control that is already running the Apple Software, for purposes of: (a) software development; (b) testing during software development; (c) using macOS Server; or (d) personal, non-commercial use."* Note it lives under **§2B "Mac App Store License"**; **§2A** (preinstalled / single-copy) grants one copy and contains **no virtualisation grant at all**. In practice any Mac that takes an automatic download lands on §2B — but "two VMs per Mac, full stop" is not what the document says.

**C3 — The service-bureau/relay clause is the one that actually constrains a hosted product.** Verbatim, with the preamble that is usually dropped: *"**Except as expressly permitted in Section 3,** the grant set forth in Section 2B(iii) above does not permit you to use the virtualized copies or instances of the Apple Software in connection with service bureau, time-sharing, terminal sharing, **relay service** or other similar types of services."* "relay service" is **new in Sequoia** (mechanically verified: 0 hits Big Sur→Sonoma, 1 in Sequoia and Tahoe) and appears **only** in the virtualisation restriction. This is the strongest single technical-legal argument for user accounts over VMs in a multi-tenant agent product: **the clause governs virtualised copies, and user accounts create none.** Not legal advice — get counsel — but it should drive the design.

**C4 — 24-hour minimum lease, and it is in the SLA, not just in vendor T&Cs.** §3.A(ii): *"each lease period must be for a minimum period of twenty-four (24) consecutive hours."* That is why AWS ("a 24-hour minimum allocation period to comply with the Apple macOS Software License Agreement"), Scaleway ("The minimum lease period for Apple silicon is 24 hours due to licensing restrictions") and MacStadium all impose it. §3 also requires advance notice to Apple Developer Relations, defines "Permitted Developer Services" as **continuous integration services**, and via §3.D confirms leasing **does not lift** the 2-VM cap.

**C5 — Every session host needs an auto-logged-in Aqua GUI session.** SSH gives no Aqua session (aahlenst, 2021-12-20; Aqua sessions are created by `loginwindow`). tart #918: simulator tests over SSH fail — LaunchDaemon context, no GUI session. DevForums 791583 (July 2025): codesign in a headless guest fails with `errSecInternalComponent` / `CSSMERR_CSP_NO_USER_INTERACTION`, works over VNC, fixed by `security unlock-keychain`. GitHub's own runner-images ship `configure-autologin.sh` (writes `kcpassword`, sets `autoLoginUser`) — revealed preference from the largest macOS CI fleet on earth. macOS **does** support multiple simultaneous GUI sessions via Screen Sharing (Quinn, DevForums 692749: *"in no way are they 'suspended'"*), which is what makes the multi-account path possible. **On macOS 15+ there is a second, independent keychain gate: Virtualization.framework refuses to start a guest at all without an unlocked `login.keychain` on the host** (tart FAQ, undocumented).

**C6 — RAM: the "~8 GB VM" target is the wrong number.** Framework floor is 4 MiB; ceiling measured = `hw.memsize` exactly (17,179,869,184 on this 16 GB host — **one data point**, do not generalise). Apple's own restore image declares `minimumSupportedMemorySize = 4 GiB`, `minimumSupportedCPUCount = 2`. Reality: one booted iOS 26.5 device = **179 processes / 640.3 MB RSS**, and RSS is a floor (this host was carrying 19.3 GB of a 20 GB swap store). An 8 GB guest boots and runs one simulator plus a small build; it is not a comfortable Xcode host. **Size guests at 12–16 GB.** Also delete the supporting belief: Apple's base Mac config has been **16 GB since the M4 Mac mini (Oct 2024)** — no current Mac ships with 8 GB.

**C7 — Disk asymmetry decides the density question.** VM route: ~64 GiB to pull, **140 GB provisioned per image** (`disk_size = 140` in `xcode.pkr.hcl`; provisioned, not occupied — sparse APFS won't consume it all on first clone). Account route: **one** Xcode (4.0 GB) and **one** set of runtimes (35 GB measured for two) serves every account on the box.

**C8 — One Xcode version per Mac, assume.** idb/FBSimulatorControl: *"CoreSimulatorService can only be aware of a single Xcode at any point in time; different versions of Xcode cannot be used concurrently on the same host"*, and switching Xcode restarts the service and kills booted simulators. `CoreSimulatorService` is per-user here, so per-user `DEVELOPER_DIR` *might* work — untested. Plan for one Xcode per host.

**C9 — The Simulator is not a VM and isolates nothing.** Verified by direct inspection, not inference: runtime binaries are host-architecture Mach-O with `LC_BUILD_VERSION platform IOSSIMULATOR`, `launchd_sim` is an ordinary host userspace executable, **there is no kernelcache or `.im4p` anywhere in the runtime**, and all runtime daemons are host processes on the host kernel (`xnu-12377.81.4`). Two consequences: (a) §2B's prohibition on running iOS "in virtual operating system environments" does not reach the Simulator (*our reading, not Apple's*); (b) whatever isolates sessions must isolate **host processes**, because that is all a simulator is.

**C10 — Patch currency is part of the isolation design.** A user-account boundary is one unpatched local-root LPE away from nothing. Three shipped in 2026: CVE-2026-20614 (fixed 26.3), CVE-2026-28915 and CVE-2026-28951 (both fixed **26.5**). This research host runs 26.3 and is therefore behind two of them.

---

## 4. RECOMMENDATION

### Primary: one agent session per Mac, from a pool of monthly-rented Apple Silicon boxes. No isolation mechanism.

The requirement is "session A must not see session B's projects, files, or simulators." A separate physical Mac satisfies that completely, costs about **$125–250/month**, requires **zero** isolation engineering, has **zero** licensing exposure, and has **no cap**. Every other option on the table is an optimisation that trades away one of those four properties to raise density. Don't buy the optimisation before you have the utilisation problem.

Concretely: rent Apple Silicon boxes monthly, keep a warm pool, assign one session per box, and reset a box between sessions by deleting and recreating the working user (`sysadminctl -deleteUser` removes `~/Library/Developer/CoreSimulator/Devices` with the home directory). One auto-logged-in GUI user per box. Non-admin, no sudo. Keep hosts on the current macOS point release.

### Then, only when utilisation justifies it: add density with separate non-admin user accounts on each box.

Each session gets its own account, auto-logged into its own Aqua session (console for the first, Screen Sharing for the rest), sharing one Xcode. `chmod 700 ~` for every session user — the default `drwxr-x---` + umask 022 lets a sibling `staff` user read default-mode subdirectories. Forbid `/Users/Shared` and `/private/tmp` for project state (both are `drwxrwxrwt`). This gets you 4–8 sessions per 64 GB box instead of 1, on shared disk, with no licence change. It is not a boundary you would bet a breach on — sudo and any local-root LPE defeat it — so keep **different customers on different Macs** and use accounts only to pack one customer's concurrent sessions onto one box.

### Explicitly rejected

- **macOS VMs.** Two per host is a permanent ceiling on your unit economics, the image costs 140 GB provisioned instead of a shared 39 GB, the only good tooling is now `openai/tart` under FSL-1.1-ALv2 with a Competing-Use clause held by a competitor in agent infrastructure, the published Tahoe Xcode images are broken right now (§5), and §2B(iii)'s service-bureau/relay sentence targets exactly the *virtualised* case. It also doesn't solve the GUI/keychain problem — it **adds** one (host-side keychain gate). The one thing VMs genuinely buy you is a real kernel boundary; buy that with a separate Mac instead, which costs less and is licence-clean.
- **seatbelt / `sandbox-exec`.** Reproduced escape, no middle setting, deprecated since 2017, undocumented profile language, unanswered removal-timeline issue. Not a guardrail for this workload; it is theatre.

### Fallback if the primary fails

If the multi-account concurrency test (below) fails, **stay at one session per Mac and grow the fleet.** At $125/month a Mac, that is still cheaper than the 2-VM-per-host path (which needs the same number of Macs above 2 sessions, plus the VM tooling, plus the licence exposure). Do not fall back to VMs; fall back to more Macs.

### What I would do FIRST, this week, for $0

**Test 1 — the load-bearing assumption, on the M2 Pro you already have. Half a day. Free.**
Create two non-admin users. Log the first in at the console with auto-login; log the second in over Screen Sharing to `localhost`. In **both simultaneously**: `xcrun simctl boot` a device, run `xcodebuild test -destination 'platform=iOS Simulator,…'`, and drive an `idb` tap/swipe/long-press. Then assert the isolation: from user B, try to read user A's `~/Documents`, `~/Library/Developer/CoreSimulator/Devices`, and `simctl list devices` output. Record whether the Screen-Sharing session's simulator is as stable as the console one over an hour.

Pass → the recommendation is fully de-risked, and you have your density lever.
Fail → one session per Mac; fleet size is the only dial.

**Test 2 — one rented box, one month, ~$125.** Run the real end-to-end agent flow on a single Scaleway (or equivalent) M4 before signing anything longer. Do not take MacStadium's M4 tier for this — its own pricing page says *"We are accepting orders of M4 models with an annual contract in quantities of 3 or more."*

Do not buy hardware, sign an annual contract, or build a VM image pipeline until both pass.

### Where the evidence is genuinely mixed, and which way I'd bet

**Does the Simulator work inside a macOS guest?** Nobody in this research ran `xcodebuild test -destination 'platform=iOS Simulator'` in a VM. I'd bet **yes, it mostly works** — Tart's own image build runs `xcrun simctl list runtimes` and `simctl runtime dyld_shared_cache update --all` inside a guest, and MacStadium sells "native Apple simulators" in Orka VMs. But the GPU path is the known-bad one (tart #702's Metal-crashes-the-VM report went stale; tart #1032 open since Feb 2025; Flutter #150169's crash frames sit in `MTLSimDriver`, i.e. exactly the layer that becomes paravirtual in a guest). **This doesn't change the recommendation** — I'm not recommending VMs — and it's the reason I wouldn't recommend them even if the 2-cap didn't exist.

**Would I change my mind about VMs?** Yes, on one condition: if Apple lifts or documents an increase to the concurrent-guest limit **and** clarifies that agent-session hosting is not a "relay service". Absent both, the ceiling and the clause make VMs a dead end for this product, not a preference.

---

## 5. WHAT WE COULD NOT CONFIRM

Nothing in the preceding sections rests on an unmarked unverified claim. Explicitly:

1. **Whether a simulator boots and stays healthy in a non-console Screen-Sharing Aqua session, concurrently with another user's simulator.** Untested by anyone here. **This is the single load-bearing assumption of the recommendation** — Test 1 exists to settle it. (Supporting but not decisive: `launchctl` shows the simulator's job in the `user/501` domain, not `gui/501` — but that was measured on a host *with* a GUI session logged in, so it is structural inference, not proof.)
2. **Whether two macOS accounts genuinely keep simulators separate under load.** Structural evidence is good (per-user `CoreSimulatorService`; exactly one shared root daemon, `simdiskimaged`) but that shared daemon is an untested single point of failure across accounts.
3. **Whether per-user `DEVELOPER_DIR` lets two accounts run different Xcode versions.** Untested.
4. **Whether `xcodebuild test -destination 'platform=iOS Simulator'` works in a Virtualization.framework guest.** See §4.
5. **The number 2 as an Apple-published figure.** It is not published anywhere by Apple. Apple publishes only the error. Do not write "Apple documents a limit of 2".
6. **Whether Linux guests are exempt from that limit.** No Apple statement; Apple's error text is guest-agnostic ("virtual machines"). Third-party observation only.
7. **Whether §3's Developer Relations notice is notification or approval.** Unresolved; the contact URL is behind Apple ID auth (confirmed 302 to `idmsa.apple.com`).
8. **Whether "service bureau … relay service" reaches an agent platform that streams a simulator to a customer.** No authority, no test case. Legal question. Our reading that the Simulator is not a "virtual operating system environment" is *our* interpretation, not Apple's — the process-table evidence behind it is solid, the mapping onto the SLA phrase is not Apple's word.
9. **Whether FSL-1.1's Competing-Use clause bites Palai building on Tart**, now that the licensor is OpenAI. Legal question. Related: `tart.run/licensing` still advertises paid tiers (Gold $12,000/yr, Platinum $36,000/yr, Diamond $12/core/yr) and is stale relative to the FSL relicense merged 2026-06-05.
10. **`maximumAllowedMemorySize == host RAM` as a general rule.** True on this one 16 GB host. One data point.
11. **"Metal ≈92% / CPU ≈55% in a VM."** Single secondary source, unspecified machine, unspecified guest core count. Not a number you can size against.
12. **APNs in a macOS guest.** The widely-circulated "confirmed Apple DTS defect" **is not one**: the paragraph in DevForums 796868 was posted by an employee of Veertu (vendor of one of the products it names) and is explicitly presented as GPT-5 output. Apple confirmed nothing; the underlying report is one user with an open feedback ID (FB19629940).
13. **Mac mini M4 retail price as of July 2026.** Not verified in this research (it was $599 at Oct 2024 launch). Re-check before quoting the capex numbers in §6.
14. **The Apple agreements behind sub-24-hour per-minute macOS vendors.** No public agreement found. You can buy the product today; do not architect on the assumption that the model persists.
15. **VM clone/boot wall-clock.** The only figure anywhere is Cirrus's own **1.35 s clone + 9.87 s boot ≈ 11.2 s**, on their own tuned image, **April 2024** — one vendor, two years old. Tart's copy-on-write clone is `FileManager.copyItem`; Apple does not document that as guaranteeing an APFS `clonefile` reflink.
16. **TCC prompts** when sharing a host directory into a guest or into a launchd-daemon context. Untested.
17. **Whether the Simulator boots in a guest with no graphics device.** Untested.
18. **CVE-2026-20658.** Three of the four 2026 local-root CVEs were verified from Apple's security releases; this one was not re-fetched.
19. **Scaleway per-model price mapping.** Six SKU prices verified exact (€75 / €115 / €139 / €149 / €199 / €335); which price belongs to which chip was not captured. Confirm before ordering.

**Two facts that are confirmed and that make the VM path worse than it looks on paper.** `macos-tahoe-xcode:latest` is Xcode **26.5, uploaded 2026-05-26**; the Xcode 26.6 Tahoe image build **failed** (run 28726848236) and the retry was **cancelled** (28746500883); the monthly rebuild workflow **failed all three "Update Base Images" jobs** on 2026-07-04 and pins tahoe's `latest: true` tag to **Xcode 26.2**, so a *successful* monthly run would push `latest` **backwards**. Separately, Cirrus Labs joined OpenAI on 2026-04-07, Cirrus CI shut down 2026-06-01, and MacStadium's own post says the tools are heading toward open source *"with no dedicated team behind them."* If you ever go the VM route, mirroring images is mandatory, not prudent. (And treat GhostVM's claim to "Run as many as you need, fully isolated" as unreliable marketing — it contradicts both the SLA and `VZErrorVirtualMachineLimitExceeded`, and the repo has no LICENSE file.)

---

## 6. COST

All prices verified 2026-07-26/27 against vendor pricing pages.

### Per-Mac, per month

| Source | Spec | Price | Conditions |
|---|---|---|---|
| **Scaleway Apple Silicon** | six SKUs | **€75 – €335/mo** (~$81–$362) | 24 h minimum lease. Per-model mapping unverified (§5.19). No annual commitment. |
| **MacStadium M4.M** | M4 | **$249/mo** | **Annual contract, quantity ≥ 3.** Max 2 VMs/node ("This is an Apple EULA requirement"). |
| **MacStadium M4.L** | M4 **Pro 12-core** (binned; AWS's mac-m4pro is 14-core) | **$349/mo** | Same annual ≥3 condition. |
| **AWS EC2 `mac-m4.metal`** | M4, 10 vCPU, 24 GiB | **$1.23/h** → **$29.52 minimum per allocation**, **~$898/mo** held continuously | 24 h minimum. **Stop → scrub "up to 4.5 hours"** on Apple Silicon, during which *"You can't start the stopped Mac instance or launch a new Mac instance"* (unbilled but unusable). Launch takes 6–20 min. No Spot, no Reserved. One Mac instance per Dedicated Host. Savings Plans up to 44% off on a 3-year commit. |
| **Owned Mac mini M4** | 16 GiB base | **capex, price unverified** (~$599 at Oct 2024 launch) | ~$17/mo amortised over 36 months + power + space. Cheapest per session-hour on sustained load. |

### Per-minute CI vendors (no 24 h floor visible to you as customer)

GitHub Actions macOS standard **$0.062/min** = **$3.72/h**; macOS 5-core M2 Pro arm64 **$0.102/min** = **$6.12/h**. Depot **$0.08/min** = **$4.80/h** (8 CPU / 24 GB / 400 GB, M4) — their own caveat: *"**Due to licensing constraints from Apple**, our macOS runner capacity is not fully elastic."* Tenki **$0.025/core-min** (Starter caps at 4 cores / 8 GB). These are CI runners, not persistent session hosts.

### The recommended path, at 10 concurrent sessions, one whole Mac per session

| | Monthly | Per session-hour @ 8 h/day, 22 d | Per session-hour @ 100% |
|---|---|---|---|
| **Scaleway @ €115 × 10** | ~**$1,250** | **~$0.71** | **~$0.17** |
| MacStadium M4.M × 10 | $2,490 | ~$1.41 | ~$0.34 |
| AWS mac-m4.metal × 10, held | **$8,980** | ~$5.10 | ~$1.23 |
| GitHub Actions arm64 M2 Pro × 10 | $10,771 | $6.12 | $6.12 |
| Owned Mac mini × 10 | ~$170 amortised (+ capex) | ~$0.10 | ~$0.02 |

**Break-even:** a €115/mo (~$125) rented box pays for itself against GitHub Actions arm64 after **~20 hours of use per month**, and against AWS EC2 Mac after **~4 days**. Per-minute billing only wins below roughly **2 hours/day/session**.

**Sourcing call:** start on **monthly rentals with no annual commitment** (Scaleway or equivalent — MacStadium's M4 tier requires an annual contract at quantity 3+, so it is not a one-box experiment). Move to **owned Mac minis** once the steady-state fleet size is known; the amortised cost is an order of magnitude below every rental option and there is no licence downside to owning. **Use AWS only if you specifically need its API and Auto Scaling and can hold instances for weeks** — at ~7× Scaleway's price plus a 4.5-hour scrub window after every stop, it is the wrong shape for per-session hosts.
---

## Ek A — Kaynaklar

Bu doküman 13 agent'lık bir araştırma turundan sentezlendi (6 bağımsız açı + her birine düşmanca doğrulama + sentez). Aşağıdaki 114 kaynak o turda fiilen çekildi. Sentez metni kaynakları satır içinde taşımıyordu; ham brief'lerden çıkarılıp buraya eklendi.

- https://512pixels.net/2026/06/wwdc26-macos27-drops-intel-support/
- https://aahlenst.dev/blog/accessing-the-macos-gui-in-automation-contexts/
- https://aws.amazon.com/about-aws/whats-new/2026/01/amazon-ec2-m4-max-mac-instances-ga
- https://aws.amazon.com/about-aws/whats-new/2026/05/amazon-ec2-m3-ultra-mac-instances-generally-available/
- https://aws.amazon.com/blogs/aws/announcing-amazon-ec2-m4-and-m4-pro-mac-instances/
- https://aws.amazon.com/blogs/compute/getting-started-with-anka-on-ec2-mac-instances
- https://aws.amazon.com/blogs/compute/getting-started-with-anka-on-ec2-mac-instances/
- https://aws.amazon.com/ec2/instance-types/mac/
- https://aws.amazon.com/ec2/instance-types/mac/faqs/
- https://circleci.com/docs/guides/execution-managed/using-macos/
- https://cirrus-runners.app/blog/2024/04/11/optimizing-startup-time-of-cirrus-runners/
- https://cirrus-runners.app/pricing/
- https://cirruslabs.org/
- https://code.claude.com/docs/en/sandboxing
- https://depot.dev/docs/github-actions/runner-types
- https://dev.classmethod.jp/en/articles/ec2-mac-m3ultra-metal-pricing-api/
- https://developer.apple.com/contact/macos-license/
- https://developer.apple.com/contact/macos-license/`:
- https://developer.apple.com/documentation/Virtualization/running-macos-in-a-virtual-machine-on-apple-silicon
- https://developer.apple.com/documentation/bundleresources/entitlements/com.apple.vm.networking
- https://developer.apple.com/documentation/virtualization
- https://developer.apple.com/documentation/virtualization/adding-the-virtualization-entitlement-to-your-project
- https://developer.apple.com/documentation/virtualization/using-icloud-with-macos-virtual-machines
- https://developer.apple.com/documentation/virtualization/vzerror/code/virtualmachinelimitexceeded
- https://developer.apple.com/forums/thread/661939
- https://developer.apple.com/forums/thread/663228
- https://developer.apple.com/forums/thread/692749
- https://developer.apple.com/forums/thread/729580
- https://developer.apple.com/forums/thread/791583
- https://developer.apple.com/forums/thread/796868
- https://developer.apple.com/forums/thread/811259
- https://developer.apple.com/forums/thread/830118
- https://developer.apple.com/videos/play/wwdc2022/10002/
- https://developer.apple.com/videos/play/wwdc2023/10007/
- https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-mac-instances.html
- https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/mac-instance-stop.html
- https://docs.github.com/en/actions/reference/runners/larger-runners
- https://docs.github.com/en/billing/reference/actions-minute-multipliers
- https://docs.hetzner.com/robot/dedicated-server/server-lines/apple-rx-server/
- https://docs.macstadium.com/orka/orka-resources/apple-silicon-based-support
- https://docs.macstadium.com/remote-desktop-vdi/macstadium-vdi-deployment/overview-architecture
- https://eclecticlight.co/2022/08/04/virtualisation-on-apple-silicon-macs-8-how-apple-limits-vms/
- https://eclecticlight.co/2023/03/22/fast-user-switching-how-it-works-and-when-to-use-it/
- https://eclecticlight.co/2026/04/29/virtualisation-on-apple-silicon-macs-is-different/
- https://example.com
- https://fbidb.io/docs/fbsimulatorcontrol/
- https://ghostvm.org/ghostvm-for-ai-agents
- https://gist.github.com/wincent/2752d8d97727577050c043e4ff9e386e
- https://github.com/actions/runner-images/blob/main/images/macos/scripts/build/configure-autologin.sh
- https://github.com/actions/runner-images/issues/10592
- https://github.com/actions/runner-images/tree/main/images/macos/templates
- https://github.com/anthropic-experimental/sandbox-runtime
- https://github.com/apple/containerization
- https://github.com/apple/containerization/issues/737
- https://github.com/cirruslabs/chamber
- https://github.com/cirruslabs/macos-image-templates
- https://github.com/cirruslabs/tart/discussions/1054
- https://github.com/cirruslabs/tart/issues/702
- https://github.com/cirruslabs/tart/issues/918
- https://github.com/facebook/idb
- https://github.com/facebook/idb/blob/main/website/docs/idb/architecture.mdx
- https://github.com/facebook/idb/blob/main/website/docs/idb/fbsimulatorcontrol.mdx
- https://github.com/facebook/idb/issues
- https://github.com/facebook/idb/issues/401
- https://github.com/flutter/flutter/issues/150169
- https://github.com/macvmio/curie
- https://github.com/microsoft/mxc/blob/main/docs/macos-support/seatbelt-backend.md
- https://github.com/mobile-dev-inc/Maestro/issues/2176
- https://github.com/openai/orchard
- https://github.com/openai/tart
- https://github.com/openai/tart/issues/1228
- https://github.com/openai/tart/issues/1272
- https://github.com/openai/tart/pull/1238
- https://github.com/redcanaryco/mac-monitor/wiki/5.-Endpoint-Security-Overview
- https://github.com/redcanaryco/mac-monitor/wiki/5.-Endpoint-Security-Overview;
- https://github.com/scaleway/docs-content/blob/main/pages/apple-silicon/faq.mdx
- https://github.com/trycua/cua
- https://github.com/utmapp/UTM
- https://github.com/webcoyote/clodpod
- https://instances.vantage.sh/aws/ec2/mac-m4.metal
- https://khronokernel.com/macos/2023/08/08/AS-VM.html
- https://macstadium.com/blog/cirrus-labs-is-joining-openai
- https://macstadium.com/pricing
- https://manp.gs/mac/1/sandbox-exec
- https://medium.com/@rnovokhatski/ios-automated-tests-in-ci-simulators-webdriveragent-and-the-macos-tax-39d5275836e3
- https://news.ycombinator.com/item?id=47868046
- https://objective-see.org/blog/blog_0x7F.html
- https://raw.githubusercontent.com/openai/tart/2.32.1/LICENSE
- https://raw.githubusercontent.com/openai/tart/2.32.1/LICENSE`
- https://raw.githubusercontent.com/openai/tart/main/LICENSE
- https://settings.blog/en/virtualizing-the-macos-27-golden-gate-beta/
- https://support.apple.com/guide/deployment/dep24dbdcf9e/web
- https://support.macstadium.com/hc/en-us/articles/28349313921563-Orka3-API-Quick-Start
- https://tart.run/
- https://tart.run/blog/2025/10/27/press-release-cirrus-labs-successfully-enforces-its-fair-source-license/
- https://tart.run/faq/
- https://tart.run/licensing/
- https://tart.run/quick-start/
- https://thenewstack.io/apple-containers-on-macos-a-technical-comparison-with-docker/
- https://tradingeconomics.com/euro-area/currency
- https://veertu.com/
- https://www.apple.com/legal/sla/docs/macOSTahoe.pdf
- https://www.apple.com/legal/sla/docs/macOSTahoe.pdf`
- https://www.apple.com/legal/sla/docs/xcode.pdf
- https://www.macincloud.com/pages/dedicated.html
- https://www.mallory.ai/vulnerabilities/CVE-2026-28951
- https://www.pillar.security/blog/escaping-antigravitys-allow-default-seatbelt
- https://www.scaleway.com/en/developers/api/apple-silicon
- https://www.scaleway.com/en/docs/apple-silicon/faq/
- https://www.scaleway.com/en/docs/apple-silicon/how-to/manage-commitment-plan/
- https://www.scaleway.com/en/pricing/apple-silicon/
- https://www.sentinelone.com/vulnerability-database/cve-2026-20614/
- https://www.sentinelone.com/vulnerability-database/cve-2026-28915/
- https://www.tenki.cloud/pricing
