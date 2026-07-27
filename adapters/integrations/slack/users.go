package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// PUTTING A NAME ON A SPEAKER (spec §36).
//
// WHY IT EXISTS. Thread history works and the summaries are good, but they read "Person 1 said… Person 2
// replied…", because a Slack message carries only a user ID and nothing here resolved it. That is not a
// cosmetic loss: a summary of a technical discussion whose participants are anonymous integers is one the
// reader has to re-attribute by hand, and the humans in that thread have names the app was granted the scope
// to read.
//
// THE AUTHORITY BOUNDARY, and it is the same one history.go draws, restated because this is a SECOND read
// primitive: `users:read` is granted, so a lookup keyed on an id somebody else chose is a read of the
// workspace's directory. This function therefore takes the id as an ARGUMENT and resolves nothing on its own.
// What may legitimately be passed is decided one layer up (slack_thread.go): only the ids of people who
// ALREADY SPOKE in a thread the app was mentioned in — a set the app has by definition already read.
//
// A NAME IS DATA, NEVER AUTHORITY. This is the E20 T3 rule and it survives unchanged: a slack_user_id never
// becomes a principal, and a display name is strictly weaker than the id it came from — it is uploader-
// controlled text with no uniqueness guarantee at all. A user who sets their display name to "admin",
// "system" or the name of a colleague gains NOTHING: the name reaches one place, the untrusted quoted-thread
// block of a prompt, and nothing anywhere reads it back. Every authorization decision in this integration
// (allowed_users, ApproverAuthorized, the run's principal) keys on the ID, and none of them can see this
// string.
//
// IT IS NOT A SENDER, so like ThreadReplies it deliberately does not ride PostMessage: no retry budget, no
// {ok,ts} decoding, and a refusal simply answers the caller, which then labels that speaker the way it did
// before this file existed.
//
// CONTRACT: https://docs.slack.dev/reference/methods/users.info/ (checked 2026-07-27) — HTTP GET; required
// `user`; optional `include_locale`; bot scope `users:read`; Tier 4 (100+ per minute); the answer is
// {ok, user:{id, name, real_name, is_bot, profile:{display_name, real_name, …}}}; a refusal is HTTP 200
// carrying {"ok":false,"error":"user_not_found"|"missing_scope"|"user_not_visible"|…}.

// MaxDisplayNameRunes bounds a resolved name. A Slack display name is short in practice; the bound is against
// a profile field somebody filled with a paragraph, which would otherwise ride into every prompt line that
// person appears on. Exported because the caller's own budget arithmetic depends on a label having a ceiling.
const MaxDisplayNameRunes = 64

// DisplayName resolves ONE Slack user id to the name that user chose to be shown as.
//
// The four candidate fields are tried in the order a human would expect to be called, because they are not
// interchangeable and the first one is frequently EMPTY (Slack only sets profile.display_name when the user
// fills it in): profile.display_name → profile.real_name → real_name → name (the @handle, always present).
//
// The returned name is FLATTENED and BOUNDED. The caller renders "<name>: <message>" lines into a prompt, so
// a newline inside a name forges a speaker — sanitising here rather than there is what keeps the next caller
// from having to rediscover it. An empty or whitespace-only result is an error, not "", so a caller can
// always tell "no name" from a name.
func DisplayName(ctx context.Context, doer Doer, apiBase string, token []byte, userID string) (string, error) {
	if userID == "" {
		return "", fmt.Errorf("slack: a user lookup needs a user id")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		apiBase+"/users.info?"+url.Values{"user": {userID}}.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("slack: build user lookup: %w", err)
	}
	if len(token) > 0 {
		// The header and nothing else, as everywhere in this package.
		req.Header.Set("Authorization", "Bearer "+string(token))
	}
	resp, err := doer.Do(req)
	if err != nil {
		return "", fmt.Errorf("slack: user lookup: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Includes 429. Not retried: a name is the most optional thing this integration reads.
		return "", fmt.Errorf("slack: user lookup answered HTTP %d", resp.StatusCode)
	}
	// 1 MiB is far past any profile and far short of a memory problem.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("slack: read user lookup body: %w", err)
	}
	var env struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		User  struct {
			Name     string `json:"name"`
			RealName string `json:"real_name"`
			Profile  struct {
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return "", fmt.Errorf("slack: decode user lookup: %w", err)
	}
	if !env.OK {
		if env.Error == "" {
			env.Error = "unknown"
		}
		// Typed, so `missing_scope` (users:read was never granted) is distinguishable from `user_not_found`
		// (a deactivated account) without substring matching.
		return "", &APIError{Code: env.Error}
	}
	for _, candidate := range []string{
		env.User.Profile.DisplayName,
		env.User.Profile.RealName,
		env.User.RealName,
		env.User.Name,
	} {
		if name := flattenName(candidate); name != "" {
			return name, nil
		}
	}
	return "", fmt.Errorf("slack: user %s has no readable name", userID)
}

// flattenName turns a profile field into something safe to use as a label: every control character and
// newline becomes a space (so a name cannot open a line of its own), runs of whitespace collapse, and the
// result is bounded. It does NOT try to make the name trustworthy — nothing can, it is text a stranger typed
// — it only stops the name from being read as STRUCTURE by whatever renders it.
func flattenName(raw string) string {
	flat := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, raw)
	flat = strings.Join(strings.Fields(flat), " ")
	// The ellipsis is INSIDE the bound, not added to it: MaxDisplayNameRunes is what the caller budgets for.
	if runes := []rune(flat); len(runes) > MaxDisplayNameRunes {
		flat = strings.TrimSpace(string(runes[:MaxDisplayNameRunes-1])) + "…"
	}
	return flat
}
