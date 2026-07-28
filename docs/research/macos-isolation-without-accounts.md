# Can we isolate concurrent agent sessions on one Mac **without** per-session macOS user accounts?

**Date:** 2026-07-28 · **Host for all local measurements:** M2 Pro, macOS **26.3 (25D125)**, `xnu-12377.81.4`, Xcode **26.6 (17F113)**, SIP **enabled**, uid 501 `salih` (admin, `staff`)
**Companion document:** [`macos-session-isolation.md`](./macos-session-isolation.md) (2026-07-26). That one asked "which isolation mechanism should we pick"; this one asks the narrower question the owner actually posed — *can we skip the account machinery altogether*.

---

## 1. THE VERDICT

**No. On a single Mac, there is no way to isolate concurrent agent sessions from each other without giving each session its own uid — and "hidden volumes", encrypted disk images, ACLs and `chflags` are not a security boundary at all.** Everything on the table fails for one of exactly two reasons, both of which I reproduced on this machine today. Either **(1)** the mechanism is discretionary and the session's own uid owns it, so the session simply turns it off — a deny-ACL that blocks the owner's own read is removed with one `chmod -N`, a `uchg` flag with one `chflags nouchg`, an encrypted volume is wide open to every process on the box the moment it is mounted, and it has to be mounted for the session to work; or **(2)** the mechanism *is* mandatory and kernel-enforced, and `xcrun simctl spawn` walks straight through it, because the spawned process is a child of `launchd_sim` → `launchd`, **not** a child of the agent. I demonstrated (2) against Apple's **App Sandbox** — the supported, code-signed, documented sandbox that the deprecated `sandbox-exec` tells you to adopt instead — and separately against **TCC**, where a process that could not list `~/Desktop` listed it fine through `simctl spawn`, filenames and all. The prior document had this escape only against seatbelt; it is not a seatbelt bug, it is the shape of the Simulator's control path, and it defeats the supported sandbox too. The only mechanism that could in principle survive it is an **Endpoint Security** authorisation client, which needs an entitlement Apple grants by application (developers report multi-month silences), must answer every `AUTH_OPEN` inside a deadline or the system kills the process, and — the killer — cannot attribute a `simctl`-spawned process to a session, because the process tree that would carry that attribution has been severed. **So: yes, you have to build the account machinery, or run one session per Mac.** Worth knowing before you argue with this: the one shipped tool built for exactly this problem — sandboxing AI agents on macOS, from NTT Labs, July 2025 — implements it with **separate macOS user accounts**, `su`/`sudo`/`rsync` and the `useradd` equivalent. Everyone who has actually shipped this has landed on the same answer.

**Only two things genuinely change the picture**, and neither removes the accounts: if all sessions belong to the **same customer** the threat model collapses from "attacker" to "accident", and `simctl --set` plus per-session directories is an afternoon of work that solves the real problem there (§6). And **one session per Mac** remains, as in the prior document, the boundary that costs zero engineering.

---

## 2. WHAT I MEASURED ON THIS HOST

Everything in this table was run on this machine today, non-destructively, without `sudo`. This is the spine of the argument; §4 explains each row.

| # | Test | Result |
|---|---|---|
| T1 | `chmod +a "user:salih deny read…"` on own file, then `cat` | **Permission denied** — a deny-ACL *does* bind the owning uid |
| T2 | …then `chmod -N` as the same uid | **ACL stripped, file read.** One command. |
| T3 | `chflags uchg`, write | Denied. `chflags nouchg` → **cleared by owner**, write succeeds |
| T4 | `chflags hidden` a directory | `ls` **still lists it**; `cd` works. Not a boundary in any sense |
| T5 | AES-256 APFS sparsebundle, mounted `-nobrowse` outside `/Volumes` | Mounts **`noowners`**; visible in `mount` and `df`; **any same-uid process reads it** |
| T6 | `grep` the *detached* sparsebundle bands for the plaintext | **No plaintext.** At-rest encryption is real — that is the one true property |
| T7 | Re-`attach` the sparsebundle with no password | **Fails** (authentication error); password not auto-stored in the keychain |
| T8 | `simctl spawn <udid> sh -c 'cat mnt/secret.txt > out'` | **Reads straight into the mounted encrypted volume** |
| T9 | App Sandbox (`com.apple.security.app-sandbox`, ad-hoc signed `.app`), read/write outside container | **DENIED** — the sandbox works |
| T10 | Child process of that sandboxed app writes outside container | **DENIED** — the sandbox **is** inherited across `exec` |
| T11 | `/usr/bin/xcrun` inside the App Sandbox | **`xcrun: error: cannot be used within an App Sandbox.`** Hard refusal |
| T12 | Real `Xcode.app/…/usr/bin/xcodebuild -version` inside the App Sandbox | **Works** — prints `Xcode 26.6`. The refusal is in the `/usr/bin` shim, not the toolchain |
| T13 | `simctl bootstatus <udid>` inside the App Sandbox | **Works** — real simulator control from inside the sandbox |
| T14 | **`simctl spawn` writing outside the container, from inside the App Sandbox** | **ESCAPED.** Direct write → `Operation not permitted`; via `simctl` → file created, 25 bytes |
| T15 | Ancestry of a `simctl spawn`'d process | `ppid` = **`launchd_sim`** (pid 74664) → **`launchd` (1)**, uid **501**. Agent's tree **severed** |
| T16 | `opendir("~/Desktop")` directly (ad-hoc binary, 3 runs) | **DENIED** ×3 |
| T17 | Same binary **via `simctl spawn`** (3 runs) | **ALLOWED** ×3 — and a follow-up listed **real Desktop filenames** |
| T18 | `~/Library/Containers/com.apple.{Notes,Safari,stocks}/Data` via `simctl spawn` | **DENIED** — honest negative; TCC App Data protection **holds** |
| T19 | Read + overwrite a file inside another app's sandbox **container** as a plain same-uid process | **Read ALLOWED, write ALLOWED** (ad-hoc app; `com.apple.Dictionary`/`Chess` also listable) |
| T20 | `chroot` as non-root | `Operation not permitted` — needs root |
| T21 | `simctl --set <dir>` device sets | Cleanly partitioned… |
| T22 | …but `simctl --set ~/Library/Developer/CoreSimulator/Devices` from "another session" | **Lists and controls the other set's booted devices** |
| T23 | `container` CLI / `Containerization.framework` present? | **Neither installed**; not in `/System/Library/Frameworks` |

---

## 3. THE ROOT CAUSE (why the whole class fails)

Every same-uid mechanism that is actually enforced by the kernel is enforced **per process**, and is inherited **by descent**. That is fine until you notice how the Simulator is driven:

```
agent process                    simctl spawn'd process
  └─ claude (16634)                pid 63660
      └─ … (14745)                 ppid 74664  launchd_sim
          └─ /bin/zsh (63571)          └─ ppid 1  launchd
              └─ child (63600)     uid 501  (same user, full privileges)
```

`CoreSimulatorService` is a **per-user launchd job** — measured here as pid 764, `ppid 1`, running as `salih`. When a session calls `simctl spawn`, it is not forking a child; it is asking a **sibling daemon** to create a process on its behalf. The new process is a descendant of `launchd`, runs as uid 501, and inherits **none** of the agent's sandbox, because there is no parent-child edge to inherit along.

This is why:

- **seatbelt** was escaped (prior document, §2),
- the **App Sandbox** is escaped (T14 — new here),
- **TCC** is laundered for `~/Desktop` (T16/T17 — new here),
- and an **Endpoint Security** gate could see the syscall but could not tell you *which session* made it (T15).

It is not a defect in any one sandbox. It is that **the isolation boundary and the Simulator control path are on different axes**, and the only boundary that runs on the same axis as the daemon is the uid — because `CoreSimulatorService` is instantiated *per user*. That is the whole reason accounts work and nothing weaker does.

---

## 4. MECHANISM BY MECHANISM

Each rated on the four axes asked for: **(a)** does it stop session A reading session B's files, **(b)** does it survive the `simctl spawn` class, **(c)** do Xcode/Simulator still work, **(d)** is it supported.

### 4.1 APFS volumes, encrypted volumes, sparse bundles, "hidden" volumes

- **(a) No.** T5: the mount is `noowners`, so ownership is *ignored* on the volume; every process of the uid reads it. `chmod 700` on the mountpoint is undone by the same uid (tested). 
- **(b) No.** T8: `simctl spawn` read the secret out of the mounted encrypted volume.
- **(c) Yes** — irrelevant, since (a) already fails.
- **(d) Supported**, but not as a security boundary.

**On "hidden" specifically — it is not a boundary at all, in any variant.** `chflags hidden` only sets a Finder flag: `ls` listed the directory and `cd` entered it (T4). Dot-prefixing is a shell globbing convention. `-nobrowse` only keeps it out of the Finder sidebar — `mount` and `df` both printed the mountpoint (T5). Mounting outside `/Volumes` changes nothing; I mounted into a scratch directory and it was still in `mount`. **Obscurity here is not even weak security; it is zero.**

**The one real property**, and it is worth stating precisely because it is the only honest win in this section: an **unmounted** encrypted image is genuinely opaque. I grepped the detached sparsebundle's bands for the plaintext and found nothing (T6), and re-attaching without the password failed (T7). So *at rest*, with the passphrase **not** held by the session, an encrypted image is a real boundary. The problem is that this describes a session that is **not running**. The moment session A's volume is mounted so session A can work, session B — same uid — reads it. You would need the volume mounted only while its session runs *and* the mount invisible to other sessions, and mount namespaces per-process do not exist on macOS. This is the idea behind the owner's "hidden volumes" question, and it dies here.

### 4.2 POSIX permissions, ACLs, `chflags` under one uid

**Can a process be denied access to a path its own uid owns? Yes — and it can then un-deny it, which is the whole story.**

T1 is a genuinely surprising result and I want to give it its due: a deny-ACL **does** bind the owning uid. `cat` on my own file returned `Permission denied`, and so did `ls -le`. macOS ACLs are evaluated before the mode bits and deny entries win. If you only read the first half of that test you would conclude ACLs work.

T2 is the second half: `chmod -N acltest.txt` — one command, same uid, no `sudo` — and the file read fine. The owner holds `WRITE_SECURITY` on their own file. Same for `chflags`: `uchg` blocked the write, `chflags nouchg` cleared it (T3). These are **discretionary** controls. They stop mistakes. They do not stop a process that wants the data, and an agent session running arbitrary model-authored code is exactly a process that might want the data.

- **(a) No** (self-revocable). **(b) No** — and it never even gets there. **(c) Yes.** **(d) Supported.**

### 4.3 `chroot`

- **(a)** Would help, **(b)** no — `simctl spawn` originates outside the jail entirely, **(c)** no, **(d)** effectively no.

`chroot` needs root (T20: `Operation not permitted` as uid 501), so the agent would need a root helper — which immediately gives you a privilege-escalation surface worse than the problem. Beyond that, a chroot has no `/System`, no `launchd` bootstrap, no Mach bootstrap namespace, and macOS GUI/XPC workloads need all three; `CoreSimulatorService` lives in the *user's* bootstrap namespace outside the jail. `chroot` is not a security boundary on any Unix (root escapes it by definition) and is not used for GUI workloads on macOS by anyone. **Dead on arrival.**

### 4.4 App Sandbox (entitlements + code signing) on the agent process

This is the one that deserved a real experiment rather than a citation, because it is what Apple's own `sandbox-exec` deprecation notice points you at: *"WARNING: sandbox-exec is deprecated. Consider adopting the App Sandbox instead."*

I built it: a C binary in a minimal `.app` bundle, ad-hoc signed with `com.apple.security.app-sandbox`. Findings, in order:

1. **It works.** Reads and writes outside the container are denied (T9). (A bare command-line tool with the entitlement is killed with SIGTRAP — you need a real bundle with `CFBundleIdentifier`. Noting it because it cost me a test.)
2. **It is inherited across `exec`** (T10) — a `/bin/sh` child could not write outside the container either. That is the property you would be relying on.
3. **`xcrun` refuses outright** (T11): `xcrun: error: cannot be used within an App Sandbox.` Every standard Xcode invocation path — `xcodebuild`, `xcrun simctl`, `swift build` — goes through this shim. On its own that is close to disqualifying.
4. **But the toolchain itself is not blocked** (T12): calling `Xcode.app/Contents/Developer/usr/bin/xcodebuild -version` directly printed `Xcode 26.6`, and `simctl bootstatus` genuinely monitored a booted device (T13). So the refusal is policy in the shim, not a capability limit. Degraded, though: `simctl list` reported the runtimes as `Unavailable` and emitted the Xcode-licence error, because the sandbox cannot read the licence state.
5. **And then it is escaped** (T14). From *inside* the sandbox: a direct write to a path outside the container gave `Operation not permitted` and no file; the identical write routed through `simctl spawn` produced the file. **The supported sandbox falls to the same trick as the deprecated one.**

There is a second, independent reason not to lean on containers: **a sandbox container is a one-way boundary.** It constrains the sandboxed process from reaching *out*; it does **not** stop other same-uid processes reaching *in*. T19: a plain non-sandboxed process read and then overwrote a file inside my app's container. Some Apple containers (`Notes`, `Safari`, `stocks`) *are* protected — but by **TCC**, not by the sandbox, and only some (`Dictionary`, `Chess` listed fine). This contradicts the commonly repeated claim, quoted on mysk.blog, that *"the sandbox prevents other apps and processes, including non-sandboxed ones like Terminal and even root processes, from reading or writing files inside another app's container."* On this host, for an ad-hoc-signed app, a plain `cat` and a plain `echo >` both succeeded. **Caveat, stated plainly:** my app is ad-hoc signed and not provisioned; it is possible Apple's container protection keys on a proper provisioning profile or App Store distribution. I did not test a Developer-ID-signed, provisioned app. Treat T19 as "not to be relied on" rather than "proven absent for all apps".

- **(a) Partially** — the container constrains outbound, but does not protect inbound (T19). **(b) No** (T14). **(c) Degraded** — `xcrun` refuses; direct binaries work; a full `xcodebuild` of a real project inside a container was **not tested**. **(d) Supported and current** — the only option here that is.

### 4.5 Endpoint Security / `ES_EVENT_*` authorisation

This is the only mechanism with the right *shape*: ES clients subscribe to `ES_EVENT_TYPE_AUTH_OPEN` and call `es_respond_flags_result` to allow or deny, so a file-access gate is genuinely expressible. Four things kill it in practice:

1. **The entitlement is gated by Apple.** `com.apple.developer.endpoint-security.client` is obtained through the Apple Developer System Extensions Request Form; it requires an app bundle and a provisioning profile. Developer Forums threads on the request process report multi-month waits with no response. You cannot plan a product ship date around it.
2. **AUTH events carry a deadline** (reported around 30–60s) and **the system kills your client if you miss it**. A gate on every file open in an Xcode build — hundreds of thousands of opens — is a latency and liveness risk on the critical path of the thing you are selling.
3. **It cannot attribute the event to a session** (T15). ES hands you the process's audit token and ancestry; for a `simctl spawn`'d process that ancestry is `launchd_sim` → `launchd`. You would know *a* process on the box opened session B's file. You would not know it was session A. You could try to track "processes CoreSimulatorService spawned on behalf of session A", but the service is per-user and does not tell you, and that is exactly the attribution the kernel just threw away. *(This is my reasoning from the measured process tree, not a tested ES implementation — see §7.)*
4. **You would be building an EDR product to avoid running `sysadminctl`.** That is the real argument. The effort is not comparable.

- **(a) In principle yes.** **(b) It would *see* the event — but could not attribute it.** **(c) Yes.** **(d) Supported, but entitlement-gated and slow.**

### 4.6 TCC

**What TCC does:** gates access to specific privacy-sensitive locations (`~/Desktop`, `~/Documents`, `~/Downloads`, some app containers, camera, mic) per **application code identity**, prompting the user.

**What TCC does not do, and this is the part that matters here:** it does not partition arbitrary project directories, and it keys on the **responsible application**, not the process. Every session your agent spawns shares one responsible code identity, so **TCC cannot tell session A from session B — they are the same application to it.** TCC is a consent mechanism for a human at a Mac, not a tenancy mechanism.

And it is partly launderable. T16/T17, reproduced three times each: an ad-hoc binary run from the agent's shell could **not** `opendir("~/Desktop")`; the identical binary run through `simctl spawn` **could**, and a follow-up listed real filenames (redacted here — they included personal documents). The direct process inherits the agent's responsible-process verdict; the `simctl`-spawned one is attributed elsewhere and gets a different, more permissive answer.

**Honest negative, and I want it on the record because it bounds the claim:** T18 — `~/Library/Containers/com.apple.Notes/Data` and `com.apple.Safari/Data` were **denied through `simctl spawn` too**. So this is *not* a universal TCC bypass. `~/Desktop` fell; App Data protection held. The mechanism for the difference is **undocumented** and I did not determine it.

- **(a) No.** **(b) Partially — `~/Desktop` no, App Data yes.** **(c) Yes.** **(d) Supported.**

### 4.7 Apple Containerization framework / `container` CLI

**It runs Linux, not macOS.** The README is unambiguous: *"`container` is a tool that you can use to create and run **Linux** containers as lightweight virtual machines on your Mac."* Requires macOS 26+ and Apple silicon; 1.0.0 shipped 2026-06-09. Each container is its own lightweight VM on Virtualization.framework — real isolation, for the wrong guest OS. **macOS workloads are not supported, so Xcode, `xcodebuild`, `simctl` and the Simulator cannot run in it.** Neither the CLI nor `Containerization.framework` is present on this host (T23); it is a separate open-source download, not a shipped system component.

There is a live request for exactly what you want — `apple/containerization#737`, *"Clarify `sandbox-exec` deprecation timeline and provide a replacement for non-App-Store process sandboxing"*, which explicitly asks *"Will Apple Containerization offer native process isolation without Linux VMs?"* It is **open, assigned, and unanswered**, opened 2026-05-12. As of the prior document (2026-07-26) it was unanswered; still unanswered at this fetch.

- **(a) Yes, for Linux.** **(b) N/A.** **(c) No.** **(d) Supported, wrong target.**

### 4.8 Anything else current in macOS 26

- **`sandbox-exec`**: still present at `/usr/bin/sandbox-exec` on this host, still deprecated (man page dated 2017), still with **no published replacement** for non-App-Store process sandboxing. Already rejected in the prior document on a reproduced escape; T14 now shows its recommended successor falls to the same escape.
- **CVE-2026-28910** (mysk.blog, 2026-05-19): broke App Sandbox data containers *and* TCC via Archive Utility, affecting **macOS 26.0.0 – 26.3.2**, fixed in **26.4** (2026-03-24). **This host runs 26.3 and is inside that window.** Independent of the argument here, but it reinforces prior-document constraint C10: a same-uid boundary is one unpatched CVE from nothing, and this class of CVE keeps shipping.
- **Third-party wrappers** (`bx-mac`, and similar): seatbelt/`sandbox-exec` wrappers. Inherit the reproduced escape. No.
- **Alcoholless** (NTT Labs, 2025-07-22) — the one tool purpose-built for sandboxing AI agents on macOS: *"Alcoholless just utilizes 1990s' commands (su, sudo, rsync) and the macOS equivalent of useradd to implement container-like environments."* **It is per-user accounts.** Its author's own caveat is that it stops file theft outside the working directory but not other attack classes. This is the strongest available evidence that the account machinery is not something we are failing to avoid through insufficient cleverness.

### 4.9 Non-macOS escapes: one session per Mac

Unchanged from the prior document, which costed this thoroughly (§6 there, prices verified 2026-07-26/27) — I am not re-deriving it. The relevant summary for *this* question: whole-machine separation is the only boundary that is simultaneously airtight, licence-clean, and **zero** isolation engineering, at roughly **$125–250/month** rented (~$81–362 across Scaleway's six SKUs) or ~**$17/month** amortised for an owned Mac mini.

**Is one-session-per-Mac honestly the cheaper engineering answer? For different customers, yes — and it is not close.** The alternatives are: build and operate account provisioning (`sysadminctl`, auto-login, Screen Sharing GUI sessions, teardown, secure-token handling under FileVault), or build an Endpoint Security product. Against that, "rent another Mac" is a purchase order. The account path only becomes worth its complexity when **utilisation** makes whole machines wasteful — that is a density optimisation, and per the prior document you should not buy it before you have the problem.

---

## 5. THE DECISION TABLE

| Option | Isolates A from B? | Survives `simctl spawn`? | Xcode + Simulator work? | Effort | What it costs you |
|---|---|---|---|---|---|
| **"Hidden" volume / dotted / `-nobrowse` / outside `/Volumes`** | **No** (T4, T5) | **No** (T8) | Yes | ~0 | Nothing but false confidence. Visible in `mount`/`df`, `ls`, `cd` |
| **Encrypted sparsebundle, mounted** | **No** (T5) | **No** (T8) | Yes | Low | `noowners`; open to every same-uid process while mounted |
| **Encrypted image, *unmounted*, session lacks passphrase** | **Yes, at rest** (T6, T7) | N/A (not running) | N/A | Low | Only protects sessions that aren't running. Not a runtime boundary |
| **ACLs / `chflags` under one uid** | **No** (T2, T3) | No | Yes | Low | Self-revocable in one command. Stops accidents only |
| **`chroot`** | Partly | **No** | **No** | High + root helper | Needs root; no GUI/XPC/bootstrap; new privesc surface |
| **App Sandbox on the agent** | **Partly** — outbound only, containers readable inbound (T19) | **No** (T14) | **Degraded** — `xcrun` refuses (T11) | Medium | Apple's own recommended successor, and it is escaped |
| **Endpoint Security AUTH gate** | In principle | Sees it, **can't attribute it** (T15) | Yes | **Very high** | Apple-gated entitlement (months), AUTH deadlines kill your client, you are building an EDR |
| **TCC** | **No** — same responsible identity for all sessions | Partly (`~/Desktop` no, App Data yes) | Yes | ~0 | Consent mechanism, not a tenancy mechanism |
| **Apple `container` / Containerization** | Yes, **for Linux** | N/A | **No — Linux only** | Medium | Cannot run Xcode/Simulator at all |
| **`simctl --set` per session** | **No** (T22) | No | Yes | **Low** | Namespacing, not security — but see §6 |
| **Separate non-admin user accounts** | **Yes** (modulo `sudo` + local-root CVEs) | **Yes** — per-user `CoreSimulatorService` | Yes | **Medium** — the machinery you wanted to avoid | GUI session per user, auto-login, provisioning/teardown |
| **One session per Mac** | **Yes** | **Yes** | Yes | **Zero** | ~$125–250/mo rented, ~$17/mo amortised owned |

---

## 6. WHAT CHANGES IF ALL SESSIONS ARE THE SAME CUSTOMER

A lot, and this is where the owner can genuinely save the work.

If every concurrent session belongs to one customer, the threat model stops being *"session A is hostile to session B"* and becomes *"session A must not clobber session B"*. The adversary is a confused agent, not an attacker. **Nothing in §1 applies**, because every mechanism in this document failed to a *deliberate* act — `chmod -N`, `chflags nouchg`, `simctl spawn`. None of those happen by accident.

For that case the lazy answer is the right one, and it is roughly an afternoon:

- **A per-session working directory**, and pass it as the only project root the session is told about.
- **`simctl --set <per-session-dir>`** for device sets. T21: a device created in an alternate set was invisible to the default set (`grep -c` → 0) and vice versa. This is a real, supported, documented `simctl` flag and it cleanly prevents two sessions from booting, wiping, or renaming each other's simulators. It is **not** security — T22, any same-uid process can point `--set` at another session's directory and drive its devices — but against accidents it is exactly right.
- Keep project state **out of** `/Users/Shared` and `/private/tmp` (both `drwxrwxrwt`).
- Constraint **C8 still bites**: assume one Xcode version per Mac. `CoreSimulatorService` is aware of a single Xcode at a time, and switching kills booted simulators.

That gets same-customer concurrency on one Mac with no accounts, no VMs, and no isolation engineering. **What it does not get you is a boundary you could describe to a second customer.** So the rule to write down is:

> **Different customers → different Macs (or different uids). Same customer → one Mac, separate directories and `simctl --set`.**

This also matches the prior document's staging: accounts are for packing *one* customer's sessions densely; different customers were already meant to be on different Macs.

---

## 7. WHAT I COULD NOT CONFIRM

1. **Whether a properly provisioned, Developer-ID-signed app's sandbox container resists same-uid reads.** T19 used an **ad-hoc signed** app. The container was read and overwritten by a plain process, but Apple's protection may key on provisioning. Not tested.
2. **Why `~/Desktop` is launderable through `simctl spawn` but App Data containers are not** (T17 vs T18). Undocumented. I observed the asymmetry, reproduced it, and did not determine the mechanism.
3. **Whether a full `xcodebuild` of a real project succeeds inside an App Sandbox** with the project inside the container. Only `-version`, `simctl list`, and `bootstatus` were tested. My `swiftc` attempt is **inconclusive** — the unsandboxed baseline also failed (`unable to load standard library`, from invoking `swift-frontend` directly without the driver's SDK setup), so that test proves nothing either way.
4. **Whether an ES client could reconstruct session attribution** by other means (e.g. correlating `CoreSimulatorService` XPC activity with spawn events). I reason it cannot from the severed process tree (T15); I did not build one — the entitlement is not available to test with.
5. **Whether Apple's container protection or TCC behaviour differs on 26.4+.** This host is 26.3 and inside the CVE-2026-28910 window (26.0.0–26.3.2, fixed 26.4). T18/T19 should be re-run on a patched host before being relied on in either direction.
6. **The exact TCC grant that `launchd_sim`/`CoreSimulatorService` holds** that makes T17 succeed. Not inspected — the user TCC database is SIP-protected and unreadable even by its owning uid (`authorization denied`, tested).
7. **`sandbox-exec` removal timeline.** Still unanswered by Apple (`apple/containerization#737`, open since 2026-05-12).
8. Everything carried over from the prior document's §5 — in particular that **the multi-account concurrency test (its Test 1) is still unrun**, and it remains the load-bearing assumption of the recommendation this document sends you back to.

**One thing worth raising separately from the engineering question:** T17 is a reproducible TCC bypass on a shipping macOS — a process that cannot read `~/Desktop` reads it through `simctl spawn`, filenames and contents. That is arguably worth a Feedback Assistant report to Apple, independent of what we build.

---

## 8. SOURCES

All local measurements: this host, 2026-07-28, commands as described in §2. All URLs fetched **2026-07-28**.

- https://github.com/apple/container — "Linux containers as lightweight virtual machines on your Mac"; macOS 26+, Apple silicon
- https://github.com/apple/containerization/issues/737 — sandbox-exec deprecation timeline; open, assigned, unanswered (opened 2026-05-12)
- https://mysk.blog/2026/05/19/cve-2026-28910/ — App Sandbox containers + TCC break; affects 26.0.0–26.3.2, fixed 26.4
- https://developer.apple.com/documentation/bundleresources/entitlements/com.apple.developer.endpoint-security.client — ES client entitlement (page body not retrievable via fetch; request-process details from the Forums threads below)
- https://developer.apple.com/forums/thread/133494 · https://developer.apple.com/forums/thread/736042 · https://developer.apple.com/forums/thread/759149 — ES entitlement request process, System Extensions Request Form, reported delays
- https://developer.apple.com/forums/thread/681173 — `ES_EVENT_TYPE_AUTH_*` deadlines; client killed on miss
- https://developer.apple.com/documentation/endpointsecurity/event-types — ES event types
- https://taomm.org/vol2/pdfs/CH%209%20Muting%20and%20Authorization%20Events.pdf — ES muting and authorization events
- https://medium.com/nttlabs/alcoholless-a-lightweight-security-sandbox-for-macos-programs-homebrew-ai-agents-etc-ccf0d1927301 — NTT Labs, 2025-07-22; implements agent isolation with **user accounts** (`su`/`sudo`/`rsync`/useradd-equivalent)
- https://developer.apple.com/forums/thread/661939 — "How to build a replacement for sandbox-exec?" (Quinn: unwise to build a product on SBPL)
- https://manp.gs/mac/1/sandbox-exec — deprecated man page
- https://github.com/holtwick/bx-mac — third-party seatbelt wrapper
- https://www.pillar.security/blog/escaping-antigravitys-allow-default-seatbelt — seatbelt escape class, prior art
- https://www.explainx.ai/blog/apple-container-1-linux-containers-macos-26-swift-2026 — Apple container 1.0.0 shipped 2026-06-09
- https://thenewstack.io/apple-containers-on-macos-a-technical-comparison-with-docker/ — per-container lightweight VM architecture
- https://www.heise.de/en/news/macOS-26-Native-container-support-delights-developers-and-not-just-them-10440095.html — macOS 26 container support
- [`macos-session-isolation.md`](./macos-session-isolation.md) — prior document: seatbelt escape, 2-VM cap, SLA §2B(iii)/§3, cost tables
