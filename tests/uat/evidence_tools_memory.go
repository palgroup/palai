package uat

// The E21 T7 EXIT-gate proof type (plan §T7) — the `tools-memory-0.1.0` sign-off.
//
// It lives in its own file rather than growing evidence.go further (the E18 T10 precedent), but it is the
// SAME package and the SAME discipline: Complete() gates the structure a claim marker requires, and every
// counter that MATTERS is RECOMPUTED from bytes the proof carries rather than read off a declared number —
// the rule SweepActionableElements established in E20 and the rule six fabricated checksums bought in E18 T8.
//
// WHAT THIS BUNDLE CLAIMS, and the distinction is in its name: `tools-memory`, never "tools verified against
// a real workspace". It certifies a TOOL AND KNOWLEDGE LAYER that is
//
//   - correct against the PUBLISHED vendor contracts (§3.5 M1..M19, every row with the URL it was read from);
//   - a conversation that OVERRUNS its context and does not explode — the history is windowed to a byte
//     budget deterministically, the fold is VISIBLE, and folding twice yields identical bytes;
//   - a model that can address a human but CANNOT MINT THE TOKEN — it writes a typed intent, our renderer
//     mints `<@U…>` from the delivery row's frozen requester id, and its own prose is still neutralised;
//   - no byte arriving from outside gaining AUTHORITY — an MCP result and a Slack search result are both
//     untrusted data under E17 T3's remote-A2A rule;
//   - a workspace's history SEARCHED but never COPIED, which is not a preference but the vendor's own written
//     term of use (M5).
//
// WHAT IT DOES NOT CLAIM: that any of it ran in a real Slack workspace against a real MCP server with a real
// model. That is §6 legs 1 and 4, and ToolsMemoryPeer is STRUCTURALLY the literal "fake" so no bundle in this
// family can ever say otherwise.

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// ToolsMemoryBundle is the E21 EXIT bundle's release name.
const ToolsMemoryBundle = "tools-memory-0.1.0"

// ToolsMemoryCaseIDs are the five UAT ids E21 OPENS, and the `TLM-` prefix is a GATE decision rather than an
// aesthetic one. An `SLK-` id would hit one of two doors, both measured:
//
//   - adding it to `expectedExtensionsCatalog` regenerates the SHIPPED extensions-0.1.0 bundle (that map IS
//     its case list, and CapabilityClaims feeds a digest folded into its every checksum);
//   - adding it to AgentSurfaceCaseIDs regenerates the SHIPPED slack-agent-surface-0.1.0 bundle.
//
// And the worse door: PromoteGateFor selects the E20 family by AgentSurfaceCaseIDs MEMBERSHIP, so an unlisted
// `SLK-013` would silently fall through to a WEAKER gate that knows nothing about this epic — the
// promote-gate-family-dispatch defect, verbatim. A distinct prefix removes the ambiguity from dispatch.
var ToolsMemoryCaseIDs = []string{"TLM-001", "TLM-002", "TLM-003", "TLM-004", "TLM-005"}

// ToolsMemoryPeer is the ONE honest naming a ToolsMemoryProof may carry — the SlackMappingProof /
// SlackAgentSurfaceProof literal, for the same reason and with more force. E21's counterparties are a fake
// Slack, a fake MCP server and a fake provider; the Real-time Search API this epic's crown feature is built
// on went GA in February 2026 and the fake standing in for it is the YOUNGEST in this repository. A bundle
// that cannot type the word "real" into this field cannot overclaim by accident.
const ToolsMemoryPeer = "fake"

// ToolsMemoryContracts is the CANONICAL ledger of the published vendor requirements E21 implements — or,
// for M1..M4, the requirements it read in order to REFUSE something. It is the gate's OWN copy (the
// AgentSurfaceContracts / WiringContracts discipline): a proof's contracts must EQUAL this table, so a bundle
// cannot implement a surface while quietly dropping the row that named its gap.
//
// M20 IS DELIBERATELY ABSENT AND ITS ABSENCE IS THE HONEST ANSWER: that row is the FOUR things the vendor
// documentation does NOT state, and a ledger of "published requirements we implement" is the wrong home for
// four things nobody could read. They live in docs/operations/known-gaps-1.0.md as their own rows and are
// measured by §6 leg 2. TestToolsMemoryLedgerRefusesToCarryTheUnconfirmedRow pins it.
var ToolsMemoryContracts = []ContractRequirement{
	{
		Divergence:  "M1",
		SourceURL:   "https://docs.slack.dev/ai/slack-mcp-server/",
		Requirement: "Slack ships an official MCP server at https://mcp.slack.com/mcp over JSON-RPC 2.0 Streamable HTTP, stating \"We do not support SSE-based connections or Dynamic Client Registration at this time\" — so the TRANSPORT was never the obstacle: this repo's mcp/http.go already speaks Streamable HTTP and registers connections by hand",
	},
	{
		Divergence:  "M2",
		SourceURL:   "https://docs.slack.dev/ai/slack-mcp-server/",
		Requirement: "that server authenticates with CONFIDENTIAL OAuth 2.0 and a USER token (\"You'll need to use your app's client_id and client_secret for Slack OAuth\", authorize at https://slack.com/oauth/v2_user/authorize) — and Palai has NO authorization-code flow for MCP connections at all (their secret model is a static handle: resolve, send, drop), so connecting is an epic and not a task. THE REJECTION IS THE CONTRACT'S CONSEQUENCE, not a preference",
	},
	{
		Divergence:  "M3",
		SourceURL:   "https://docs.slack.dev/ai/slack-mcp-server/",
		Requirement: "\"Only directory-published apps or internal apps may use MCP. Unlisted apps are prohibited from using MCP\" — an internal app already satisfies the distribution requirement, which is what PROVES the obstacle is M2 alone and not our app's eligibility",
	},
	{
		Divergence:  "M4",
		SourceURL:   "https://slack.com/blog/news/slackbot-mcp-security",
		Requirement: "the MCP server is \"Off by default. No server is available to users until an admin explicitly enables it\", and \"every MCP call is made as the authenticated Slack user\" — so the as-the-user posture we wanted really does exist on that path; it is unreachable for the reason M2 names",
	},
	{
		Divergence:  "M5",
		SourceURL:   "https://docs.slack.dev/apis/web-api/real-time-search-api/",
		Requirement: "\"You must not store or copy any of the data retrieved from this API. You may not use any of this data for training\" — so NOT vectorising Slack is the VENDOR'S TERM rather than our architectural taste, and `knowledge-vector` staying disabled beside a working search is that term expressed as a capability tier",
	},
	{
		Divergence:  "M6",
		SourceURL:   "https://docs.slack.dev/reference/methods/assistant.search.context/",
		Requirement: "assistant.search.context ACCEPTS A BOT TOKEN under `search:read.public` — \"All API calls made using a bot token require an action_token. API calls made using a user token do not require an action_token\" — so the CAPABILITY is reachable with the credential we already hold even though the MCP SERVER is not, and E21 builds an internal tool rather than an OAuth epic",
	},
	{
		Divergence:  "M7",
		SourceURL:   "https://docs.slack.dev/apis/web-api/real-time-search-api/",
		Requirement: "\"An app can obtain the action_token needed to query the Real-time Search API from either a message event or an app_mention event\" — the search's authority arrives WITH the message that started the run, so it is bound to that conversation rather than being a standing power, and it is spent on the call and written nowhere",
	},
	{
		Divergence:  "M8",
		SourceURL:   "https://docs.slack.dev/reference/scopes/search.read.public/",
		Requirement: "\"The searching user need not be a member of the public channels, just of the workspace, for the channels to be included in the search results\" — the boundary is WORKSPACE membership, not channel membership, and no documented parameter narrows it; search:read.private/.im/.mpim are USER-token scopes a bot cannot hold, so private channels, DMs and group DMs are structurally out of reach",
	},
	{
		Divergence:  "M9",
		SourceURL:   "https://docs.slack.dev/apis/web-api/real-time-search-api/",
		Requirement: "\"The Real-time Search API will only return results from public channels, regardless of whether or not the user has more granular scopes\" when called from within a public channel — the vendor's own ceiling narrows M8 again, and it is NOT counted as one of our defences because we never requested the granular scopes",
	},
	{
		Divergence:  "M10",
		SourceURL:   "https://docs.slack.dev/reference/methods/assistant.search.context/",
		Requirement: "\"For most teams, the limit is 10+ requests per minute\" plus \"a user-level limit of 10 requests per minute with burst\" — the tightest ceiling of anything this repository calls (against chat.appendStream's 100+/min and setStatus's 600/min), which is why pacing is PER WORKSPACE rather than per channel and why a run carries a search budget that refuses IN WORDS when spent",
	},
	{
		Divergence:  "M11",
		SourceURL:   "https://docs.slack.dev/reference/methods/assistant.search.context/",
		Requirement: "the documented arguments are query (required), action_token, channel_types, content_types, include_context_messages (default false), limit (max 20, default 20), before/after, sort/sort_dir and highlight — so the tool schema is DERIVED from this page rather than invented, and channel_types/content_types are pinned in the wire rather than exposed, because a parameter is a way for the model to ASK for the scope we declined",
	},
	{
		Divergence:  "M12",
		SourceURL:   "https://docs.slack.dev/apis/web-api/real-time-search-api/",
		Requirement: "\"The RTS API is available for directory-published apps and internal apps only\" and an app must \"be present within a workspace (and thus approved by an admin)\" — an internal app already qualifies, so the whole owner handover is one scope and one reinstall",
	},
	{
		Divergence:  "M13",
		SourceURL:   "https://docs.slack.dev/reference/block-kit/blocks/markdown-block/",
		Requirement: "the markdown block exists FOR LLM output (\"a markdown response from an LLM that can get lost in translation rendering in Slack\"), carries no interactive element, ignores block_id (\"ignored in markdown blocks and will not be retained\"), and \"the cumulative limit for all markdown blocks in a single payload is 12,000 characters\" — so the budget is per PAYLOAD rather than per block",
	},
	{
		Divergence:  "M14",
		SourceURL:   "https://docs.slack.dev/reference/block-kit/blocks/table-block/",
		Requirement: "a table carries at most 100 rows of at most 20 cells with at most 20 column_settings, and \"a single table's character count across all cells cannot exceed 10,000 characters\"; cell types are rich_text / raw_text / raw_number — E20's constants were EXACTLY right, and the only honest enrichment left was typing a numeric cell",
	},
	{
		Divergence:  "M15",
		SourceURL:   "https://docs.slack.dev/reference/block-kit/blocks/task-card-block/",
		Requirement: "a task card's details and output are rich_text ENTITIES and its status vocabulary is pending|in_progress|complete|error — E20 already renders it correctly, so E21 TOUCHES NOTHING here; the row is in the ledger to record that the check was made and the answer was no work",
	},
	{
		Divergence:  "M16",
		SourceURL:   "https://docs.slack.dev/reference/block-kit/blocks/alert-block/",
		Requirement: "\"Alert blocks are currently only supported in modals\" — structurally dead for a message surface, and opening a modal surface is its own authorization path under §2, so it is refused with a reason rather than left unmentioned",
	},
	{
		Divergence:  "M17",
		SourceURL:   "https://docs.slack.dev/reference/block-kit/blocks/data-visualization-block/",
		Requirement: "the data_visualization block is real and buildable (line/bar/area/pie, 1-20 points per series, 1-12 series) — and it is REFUSED anyway because no case in this tree asks for a chart, and render code with no user is the YAGNI this plan rejects. The door stays open: a variant and a mapping table, when something asks",
	},
	{
		Divergence:  "M18",
		SourceURL:   "https://docs.slack.dev/reference/block-kit/blocks/",
		Requirement: "actions, input, context_actions, icon_button and feedback_buttons are actionable, and card / carousel / container CARRY actionable children — every one is out of scope under §2 because a click is an action, and an action deserves the authorization path approve/deny earned (a feedback button is the tempting one, and it is refused for exactly that reason)",
	},
	{
		Divergence:  "M19",
		SourceURL:   "https://docs.slack.dev/messaging/formatting-message-text/",
		Requirement: "a mention is <@U012AB3CD>, and \"Slack uses &, < and > as control characters for special parsing in text objects, so they must be converted to HTML entities\" — NeutralizeBroadcasts's escape-rather-than-delete IS the vendor's own mechanism, and it defines the ORDER E21 depends on: the model's words are defused FIRST and our minted token is added SECOND",
	},
}

// toolsMemoryContractParts flattens the canonical ledger into hashParts input, so the digest is re-derivable
// from the CODE table alone and a bundle cannot present a self-consistent digest over an edited ledger.
func toolsMemoryContractParts() []string {
	parts := make([]string, 0, 3*len(ToolsMemoryContracts))
	for _, req := range ToolsMemoryContracts {
		parts = append(parts, req.Divergence, req.SourceURL, req.Requirement)
	}
	return parts
}

// ToolsMemoryContractsDigest is hashParts over the CANONICAL contract ledger — the E21 bundle's checksum
// anchor. A dropped or reworded §3.5 row moves every checksum in the release.
func ToolsMemoryContractsDigest() string { return hashParts(toolsMemoryContractParts()...) }

// DecodedStrings walks DECODED JSON and returns every string VALUE in it, sorted by the path it was found at.
//
// IT EXISTS BECAUSE A RAW SUBSTRING SCAN OVER MARSHALLED JSON IS VACUOUS, and this repository has paid that
// lesson twice: encoding/json escapes `<`, `>` and `&`, so a mention token `<@U0ASKER>` travels on the wire
// as `<@U0ASKER>` and an assertion looking for the literal can NEVER fire — neither to pass nor to
// fail. Every recompute below decodes first (E20 T4's `decodedStrings`, E21 T6's neutralisation sweep).
func DecodedStrings(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("no bytes to decode: a sweep over nothing is vacuous")
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("the carried bytes are not JSON, so nothing can be re-derived from them: %w", err)
	}
	return collectStrings(decoded), nil
}

func collectStrings(node any) []string {
	var found []string
	switch v := node.(type) {
	case string:
		found = append(found, v)
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys) // deterministic: the finding list is quoted in refusals
		for _, key := range keys {
			found = append(found, collectStrings(v[key])...)
		}
	case []any:
		for _, el := range v {
			found = append(found, collectStrings(el)...)
		}
	}
	return found
}

// SweepSearchBytes reports which of `needles` appear anywhere in the DECODED strings of `haystack`. It is the
// RECOMPUTE behind the crown negative of this release — "no byte retrieved from the Real-time Search API was
// stored" (M5) — and it is used in BOTH directions on purpose:
//
//   - pointed at what the run PERSISTED it must find NOTHING, or the vendor's term of use was broken;
//   - pointed at what reached the MODEL it must find EVERYTHING, or the zero above is vacuous because the
//     search returned nothing worth storing in the first place.
//
// A guard that has never found anything is not a guard, and a "we stored zero bytes" over a search that
// produced zero bytes is the exact shape of green this family keeps catching.
func SweepSearchBytes(needles []string, haystack json.RawMessage) ([]string, error) {
	if len(needles) == 0 {
		return nil, fmt.Errorf("no needles: a search-byte sweep with nothing to look for can never fail")
	}
	strs, err := DecodedStrings(haystack)
	if err != nil {
		return nil, err
	}
	joined := strings.Join(strs, "\n")
	var found []string
	for _, needle := range needles {
		if needle == "" {
			return nil, fmt.Errorf("an empty needle would match everything, so the sweep would be meaningless")
		}
		if strings.Contains(joined, needle) {
			found = append(found, needle)
		}
	}
	return found, nil
}

// mentionToken is Slack's mention syntax opener (M19). The recompute counts these in DECODED strings.
const mentionToken = "<@"

// SweepMentions returns every `<@…>` token in the DECODED strings of `raw`, in order of appearance. The E21
// crown claim rests on it: our renderer must be the ONLY minter, so the set of tokens on the wire must be
// exactly {`<@` + the delivery row's frozen requester id + `>`} and nothing else. A model that wrote a raw
// `<@U012AB3CD>` in its prose contributes NO token here, because NeutralizeBroadcasts already turned its `<`
// into `&lt;` before the renderer ever saw it.
func SweepMentions(raw json.RawMessage) ([]string, error) {
	strs, err := DecodedStrings(raw)
	if err != nil {
		return nil, err
	}
	var found []string
	for _, s := range strs {
		rest := s
		for {
			i := strings.Index(rest, mentionToken)
			if i < 0 {
				break
			}
			rest = rest[i:]
			end := strings.Index(rest, ">")
			if end < 0 {
				// An unterminated opener is not a mention Slack would render, but it IS the model getting
				// closer than it should. Reported so it can never be silently tolerated.
				found = append(found, rest)
				break
			}
			found = append(found, rest[:end+1])
			rest = rest[end+1:]
		}
	}
	return found, nil
}

// ToolsMemoryProof is the evidence a tools_memory_claim requires (plan §T7 — the E21 EXIT anchor). Its six
// fields are the plan's six, in order:
//
//	(a) HistoryTurns / FoldedTurns / FoldDigests — how many turns a context-overrunning conversation carried,
//	    how many the window DROPPED, and the SAME history folded TWICE producing byte-identical output;
//	(b) ToolsAdvertised / ToolsCalledThroughRealOrchestrator — the tool surface, measured at the REAL
//	    Orchestrator rather than at the broker seam every prior proof stopped at (§3.6 D3);
//	(c) ExternalToolResults beside ExternalToolAuthorityGained (which MUST be zero) — an MCP result is
//	    untrusted data under E17 T3's remote rule, and a zero over no results at all would be vacuous;
//	(d) SearchResultsReturned / SearchResultNeedles / SearchReachedTheModel / PersistedSurface /
//	    StoredSearchBytes (which MUST be zero and is RE-DERIVED, never believed) — M5's term of use;
//	(e) RequesterUserID / MentionsMinted / MentionsOutsideOurRenderer (which MUST be zero) / AnswerBlocks —
//	    re-derived from the blocks the answer actually carried, which ALSO carry E20's actionable sweep;
//	(f) Contracts — every vendor requirement with its source URL and §3.5 divergence id.
//
// HONEST CEILING, MECHANICALLY ENFORCED: Peer must be the literal "fake". This bundle is STRUCTURALLY
// incapable of claiming a real-workspace receipt or a real MCP server run.
type ToolsMemoryProof struct {
	Peer string `json:"peer"`

	// (a) The context window. A fold that dropped NOTHING proves no budget was ever reached; a fold that
	// dropped EVERYTHING is a truncation rather than a window. Both are refused below.
	HistoryTurns int `json:"history_turns"`
	FoldedTurns  int `json:"folded_turns"`
	// FoldDigests is the SAME history folded twice. Two entries, and they must be EQUAL — the determinism
	// claim history.go's contract rests on (a resumed attempt must re-derive the same run.start), which a
	// model-written summariser would have destroyed.
	FoldDigests []string `json:"fold_digests"`

	// (b) The tool surface, at the real Orchestrator.
	ToolsAdvertised                    int `json:"tools_advertised"`
	ToolsCalledThroughRealOrchestrator int `json:"tools_called_through_real_orchestrator"`

	// (c) The authority boundary for bytes that came from outside.
	ExternalToolResults         int `json:"external_tool_results"`
	ExternalToolAuthorityGained int `json:"external_tool_authority_gained"`

	// (d) The search boundary, RE-DERIVED. SearchResultNeedles are distinctive substrings of what the search
	// returned; SearchReachedTheModel is what the run put in front of the model; PersistedSurface is
	// everything the run WROTE DOWN. StoredSearchBytes is the declared count and it is never trusted.
	SearchResultsReturned int             `json:"search_results_returned"`
	SearchResultNeedles   []string        `json:"search_result_needles"`
	SearchReachedTheModel json.RawMessage `json:"search_reached_the_model"`
	PersistedSurface      json.RawMessage `json:"persisted_surface"`
	StoredSearchBytes     int             `json:"stored_search_bytes"`

	// (e) The mention boundary, RE-DERIVED from the answer's own blocks.
	RequesterUserID            string          `json:"requester_user_id"`
	MentionsMinted             int             `json:"mentions_minted"`
	MentionsOutsideOurRenderer int             `json:"mentions_outside_our_renderer"`
	AnswerBlocks               json.RawMessage `json:"answer_blocks"`

	// (f) The published contracts, anchored to the code table.
	Contracts       []ContractRequirement `json:"contracts"`
	ContractsDigest string                `json:"contracts_digest"`
}

// Complete reports the six fields hold on a FAKE peer AND re-derives (d) and (e) from the carried bytes. A
// proof that declares zero stored search bytes over a persisted surface containing a search result, or zero
// foreign mentions over blocks containing somebody else's id, fails HERE — in the shape verifier — rather
// than in a dedicated test somebody could forget to run.
func (p ToolsMemoryProof) Complete() bool {
	if p.Peer != ToolsMemoryPeer || p.ContractsDigest != ToolsMemoryContractsDigest() ||
		!slices.Equal(p.Contracts, ToolsMemoryContracts) {
		return false
	}
	// (a) A window, not a summary and not a truncation: something was dropped, something was kept, and the
	// same input folded twice produced the same bytes.
	if p.HistoryTurns < 1 || p.FoldedTurns < 1 || p.FoldedTurns >= p.HistoryTurns {
		return false
	}
	if len(p.FoldDigests) != 2 || p.FoldDigests[0] == "" || p.FoldDigests[0] != p.FoldDigests[1] {
		return false
	}
	// (b) Tools were advertised AND at least one was actually dispatched through the real Orchestrator; a
	// call count above the advertised count is a proof describing a surface it did not have.
	if p.ToolsAdvertised < 1 || p.ToolsCalledThroughRealOrchestrator < 1 ||
		p.ToolsCalledThroughRealOrchestrator > p.ToolsAdvertised {
		return false
	}
	// (c) External output arrived (else the zero is vacuous) and bought nothing.
	if p.ExternalToolResults < 1 || p.ExternalToolAuthorityGained != 0 {
		return false
	}
	// (d) The search half, RECOMPUTED in both directions.
	if p.SearchResultsReturned < 1 || len(p.SearchResultNeedles) < 1 || p.StoredSearchBytes != 0 {
		return false
	}
	reached, err := SweepSearchBytes(p.SearchResultNeedles, p.SearchReachedTheModel)
	if err != nil || len(reached) != len(p.SearchResultNeedles) {
		return false // the sweep cannot FIND what it is meant to find: the zero below would certify nothing
	}
	stored, err := SweepSearchBytes(p.SearchResultNeedles, p.PersistedSurface)
	if err != nil || len(stored) != 0 {
		return false // M5: "You must not store or copy any of the data retrieved from this API"
	}
	// (e) The mention half, RECOMPUTED from the answer's blocks. Every token on the wire must be OURS, and
	// "ours" means minted from the delivery row's frozen requester id — the only identity the renderer holds.
	if p.RequesterUserID == "" || p.MentionsMinted < 1 || p.MentionsOutsideOurRenderer != 0 {
		return false
	}
	mentions, err := SweepMentions(p.AnswerBlocks)
	if err != nil || len(mentions) != p.MentionsMinted {
		return false
	}
	ours := mentionToken + p.RequesterUserID + ">"
	for _, m := range mentions {
		if m != ours {
			return false
		}
	}
	// And E20's crown claim rides along unchanged: the same answer that carries our mention carries NOTHING
	// a human can press. A richer render is not a weaker defence.
	forged, err := SweepActionableElements(p.AnswerBlocks)
	return err == nil && len(forged) == 0
}

// carriesE21ToolsMemoryCase reports whether a case is one of the five ids E21 OPENED — the FAMILY marker,
// shared by the manifest verifier and PromoteGateFor so the two can never disagree about what an E21 release
// is.
//
// THE FAMILY IS RECOGNIZED BY THE CASE IDS, NEVER BY THE tools_memory_claim THE GATE ENFORCES. Dispatching on
// the claim marker is precisely how a release DROPS it, reroutes to a weaker family and passes — the defect
// the E17 dispatch comment describes and this repository has shipped once already.
func carriesE21ToolsMemoryCase(c evidenceCase) bool {
	return slices.Contains(ToolsMemoryCaseIDs, c.ID)
}

// verifyE21ToolsMemoryPresence stops the search/mention re-derivations from being OPTIONAL: a manifest
// carrying ANY of the five E21 cases MUST carry EXACTLY ONE tools_memory_claim with its proof. "Exactly one"
// because ToolsMemoryPromoteGate judges the first while this verifier checks all of them, so a second
// fabricated proof could ride behind an honest one.
func verifyE21ToolsMemoryPresence(m evidenceManifest) []Finding {
	family, claims, withProof := false, 0, 0
	for _, c := range m.Cases {
		if carriesE21ToolsMemoryCase(c) {
			family = true
		}
		if c.ToolsMemoryClaim != "" {
			claims++
			if c.ToolsMemoryProof != nil {
				withProof++
			}
		}
	}
	if !family {
		return nil
	}
	switch {
	case claims == 0:
		return []Finding{{Kind: "missing", Detail: "tools_memory_claim (this manifest carries E21 tools-and-memory cases, so it is an E21 release and MUST carry the tools-memory anchor; without the claim marker neither the stored-search-byte re-derivation nor the mention re-derivation runs at all, and both crown security claims stand unverified — plan §T7)"}}
	case claims > 1:
		return []Finding{{Kind: "invalid", Detail: fmt.Sprintf("%d tools_memory_claims (want exactly 1): the promote gate judges the FIRST tools-memory proof while this verifier checks all of them, so a second could ride behind an honest one — one release, one re-derivation (plan §T7)", claims)}}
	case withProof != claims:
		return []Finding{{Kind: "missing", Detail: "tools_memory_proof for the manifest's tools_memory_claim (a claim marker with no proof leaves \"no byte retrieved from the search API was stored\" and \"only our renderer mints a mention\" entirely unchecked — plan §T7)"}}
	}
	return nil
}
