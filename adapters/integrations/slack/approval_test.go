package slack

import (
	"errors"
	"testing"
)

func TestMapInteractiveApprovalBindsHashUserWorkspace(t *testing.T) {
	body := []byte(`{
		"type":"block_actions",
		"user":{"id":"U7","team_id":"T1"},
		"team":{"id":"T1"},
		"actions":[{"action_id":"palai_approve","value":"reqhash-abc","type":"button"}]
	}`)
	intent, err := MapInteractiveApproval(body)
	if err != nil {
		t.Fatalf("MapInteractiveApproval error = %v", err)
	}
	if intent.Decision != "approve" || intent.RequestHash != "reqhash-abc" {
		t.Fatalf("intent decision/hash = %q/%q, want approve/reqhash-abc", intent.Decision, intent.RequestHash)
	}
	if intent.UserID != "U7" || intent.TeamID != "T1" {
		t.Fatalf("intent user/team = %q/%q, want U7/T1", intent.UserID, intent.TeamID)
	}
}

func TestMapInteractiveApprovalDenyIsMapped(t *testing.T) {
	body := []byte(`{"type":"block_actions","user":{"id":"U7"},"team":{"id":"T1"},"actions":[{"action_id":"palai_deny","value":"reqhash-xyz","type":"button"}]}`)
	intent, err := MapInteractiveApproval(body)
	if err != nil {
		t.Fatalf("deny MapInteractiveApproval error = %v", err)
	}
	if intent.Decision != "deny" || intent.RequestHash != "reqhash-xyz" {
		t.Fatalf("intent = %+v, want deny/reqhash-xyz", intent)
	}
}

// The security core of SLK-007: an approval decision can ONLY come from a minted, hash-bearing button. A
// message that says "yes", a foreign/other block action, a bare button with no hash, a shortcut, or a
// view_submission all authorize NOTHING.
func TestMapInteractiveApprovalRejectsEverythingElse(t *testing.T) {
	cases := map[string]string{
		"a plain message (not even an interaction)": `{"type":"event_callback","event":{"type":"message","text":"yes"}}`,
		"a foreign block action":                    `{"type":"block_actions","user":{"id":"U7"},"actions":[{"action_id":"some_other_button","value":"reqhash","type":"button"}]}`,
		"an approve button with no request hash":    `{"type":"block_actions","user":{"id":"U7"},"actions":[{"action_id":"palai_approve","value":"","type":"button"}]}`,
		"a shortcut":                                `{"type":"shortcut","callback_id":"hello"}`,
		"a view_submission":                         `{"type":"view_submission","view":{"callback_id":"m"}}`,
		"non-json":                                  `}{`,
	}
	for name, body := range cases {
		if _, err := MapInteractiveApproval([]byte(body)); !errors.Is(err, ErrNotApproval) {
			t.Errorf("%s: err = %v, want ErrNotApproval (authorizes nothing)", name, err)
		}
	}
}
