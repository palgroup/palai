-- Session-level standing authorization for approvals (E30 T1, spec §22.4).
--
-- THE OWNER'S ASK, VERBATIM: "ayrıca auto-approve yapabilmek istiyorum session'u" — the SESSION is the
-- scope they named, and it is also the right one. A session is one sitting: a human opened a chat, is
-- watching it, and is willing to say in advance "run the build commands, stop asking me each time". A
-- project-level switch would arm every run the project ever births — a schedule at 3am, an inbound
-- webhook, a Slack thread nobody is reading — which is the opposite of "I am here, go ahead". A run-level
-- switch would have to be re-sent every turn and could not express the sitting at all.
--
-- TWO COLUMNS, NOT ONE, AND THAT IS THE WHOLE POINT OF THIS MIGRATION.
--
-- Auto-approving a gated TOOL call means the session runs `xcodebuild` and `simctl` without asking. The
-- effects land in the run's own workspace, they land while a human is watching, and on the native
-- posture (PALAI_SHELL_NATIVE=unsandboxed-host) they land on the operator's machine — which is why the
-- demo that wants this points at a throwaway project.
--
-- Auto-approving a PUBLICATION means writing to somebody's repository with no human in the loop. That
-- write OUTLIVES the session, lands outside anything the session owns, and is the single gate this whole
-- product puts in front of it. One flag for both would mean the operator who wanted to stop confirming
-- every `xcodebuild -list` had also, in the same click, stopped confirming every push. They are not the
-- same risk and they do not get the same switch.
--
-- BOTH DEFAULT FALSE. Every session alive today, and every session created by a client that has never
-- heard of these columns, behaves bit-for-bit as it did: the gate parks and a human decides. A security
-- control that changes behaviour for people who never asked for it is an outage, not a control
-- (ApproverAllowed's own words, config.go:71).
--
-- SET_BY IS NOT AUDIT DECORATION — IT IS THE PRINCIPAL THE AUTO-DECISION IS MADE AS. An armed session
-- does not approve as a machine; it approves as THE HUMAN WHO ARMED IT, standing behind the calls in
-- advance. That is what makes the switch honest on the approvals screen ("approved by <them>", which is
-- true) and what makes it impossible to use for escalation: the decision still passes
-- ApproverAllowed(set_by), so an armed session grants exactly what that person could have granted by
-- clicking, and a project whose `approvers` list does not name them auto-approves NOTHING.
ALTER TABLE sessions
  ADD COLUMN IF NOT EXISTS auto_approve_tools BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN IF NOT EXISTS auto_approve_publications BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN IF NOT EXISTS auto_approve_set_by TEXT NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS auto_approve_set_at TIMESTAMPTZ;

INSERT INTO schema_migrations (version) VALUES (56) ON CONFLICT DO NOTHING;
