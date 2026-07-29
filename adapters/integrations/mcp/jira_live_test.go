//go:build live

// The credential-gated live leg for a REAL Atlassian Rovo MCP server (docs/jira-mcp-connection.md).
//
// It is READ-ONLY by construction: it performs the MCP handshake and tools/list, and calls nothing. It never
// touches an issue, so it is safe to run against a production Atlassian site.
//
// HONEST CEILING: this proves that Palai's own transport reaches, authenticates to, and enumerates the real
// server. It does NOT drive a model run — that is the component chain in
// apps/control-plane/internal/extensions/mcp_jira_component_test.go, which covers register → discover →
// approve → pin → grant → advertise → call against a fake server built to the published protocol.

package mcp

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	jiraCredentialEnv = "PALAI_JIRA_MCP_CREDENTIAL"
	jiraURLEnv        = "PALAI_JIRA_MCP_URL"
	// defaultJiraMCPURL is Atlassian's documented Rovo MCP endpoint
	// (support.atlassian.com/atlassian-rovo-mcp-server/docs/configuring-authentication-via-api-token/,
	// fetched 2026-07-27).
	defaultJiraMCPURL = "https://mcp.atlassian.com/v1/mcp"
)

// jiraSetupInstructions is the skip message. It IS the documentation: someone who hits this skip must be able
// to go from nothing to a running leg without opening another file.
const jiraSetupInstructions = `
  PALAI_JIRA_MCP_CREDENTIAL is unset, so the real Atlassian Rovo MCP server was not contacted.

  To run this leg:

  1. Create an Atlassian API token:
       https://id.atlassian.com/manage-profile/security/api-tokens  ("Create API token")

  2. Build the credential. A PERSONAL API token authenticates with Basic auth over
     base64(email:token) — NOT Bearer. Bearer is only for a service-account API key.

       printf 'you@example.com:YOUR_API_TOKEN' | base64

     The credential is the whole header value, scheme included:

       PALAI_JIRA_MCP_CREDENTIAL='Basic <the base64 from above>'

     (A service-account API key instead: PALAI_JIRA_MCP_CREDENTIAL='Bearer <key>'.)

  3. Ask an Atlassian org admin to enable API-token authentication for the Rovo MCP server.
     Without it the endpoint accepts only interactive OAuth 2.1 and every request 401s.

  4. Run it (keep the credential out of your shell history — note the leading space):

        PALAI_JIRA_MCP_CREDENTIAL='Basic ...' go test -tags=live -run TestLiveJiraMCP -v ./adapters/integrations/mcp/

  Optional: PALAI_JIRA_MCP_URL overrides the endpoint (default ` + defaultJiraMCPURL + `).`

// TestLiveJiraMCPServerReachableAndEnumerable dials the real Atlassian Rovo MCP server with the owner's
// credential, completes the MCP handshake, and lists its tools. A green run means Jira is reachable from
// Palai's transport with that credential — the only thing a connection row then adds is the secret_ref.
func TestLiveJiraMCPServerReachableAndEnumerable(t *testing.T) {
	credential := os.Getenv(jiraCredentialEnv)
	if credential == "" {
		t.Skip(jiraSetupInstructions)
	}
	url := os.Getenv(jiraURLEnv)
	if url == "" {
		url = defaultJiraMCPURL
	}

	transport, err := NewHTTPTransport(HTTPOptions{URL: url, Bearer: credential, Timeout: 30 * time.Second})
	if err != nil {
		// The credential is never echoed — NewHTTPTransport's own errors are credential-free by construction.
		t.Fatalf("build transport for %s: %v", url, err)
	}
	defer func() { _ = transport.Close(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client := NewClient(transport)
	if err := client.Initialize(ctx); err != nil {
		// The two failures worth naming, because they are the ones an owner will actually hit.
		switch {
		case strings.Contains(err.Error(), "http status 401"):
			t.Fatalf("initialize against %s: %v\n\n"+
				"401 means the credential was rejected. Check, in order:\n"+
				"  - the scheme: a PERSONAL API token is Basic base64(email:token), not Bearer;\n"+
				"  - that an org admin has enabled API-token auth for the Rovo MCP server;\n"+
				"  - that the token has not expired or been revoked.", url, err)
		case strings.Contains(err.Error(), "server protocol"):
			t.Fatalf("initialize against %s: %v\n\n"+
				"This is a PROTOCOL VERSION mismatch, not an auth failure. Our client negotiates exactly %q and\n"+
				"disconnects on anything else. That version was VERIFIED against this endpoint on 2026-07-27 —\n"+
				"initialize succeeded — so seeing this means Atlassian has since moved. The error above names the\n"+
				"version the server offered; the fix is to widen the accepted set in client.go, as the tools\n"+
				"subset this client uses (initialize/tools-list/tools-call) is stable across the 2025 revisions.",
				url, err, ProtocolVersion)
		default:
			t.Fatalf("initialize against %s: %v", url, err)
		}
	}

	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("tools/list against %s: %v", url, err)
	}
	if len(tools) == 0 {
		t.Fatalf("%s advertised no tools; expected the Jira/Confluence tool set", url)
	}

	// Assert a Jira tool is actually present, so a server that answers but exposes nothing Jira-shaped is a
	// failure rather than a hollow pass.
	var names []string
	jiraFound := false
	for _, tool := range tools {
		names = append(names, tool.Name)
		if strings.Contains(strings.ToLower(tool.Name), "jira") {
			jiraFound = true
		}
	}
	if !jiraFound {
		// OBSERVED 2026-07-27 against the real endpoint: a credential the server does not accept does NOT
		// produce a 401 here. initialize and tools/list both succeed and the server degrades to a small
		// ANONYMOUS tool set — [getTeamworkGraphContext getTeamworkGraphObject addTeamworkGraphContext] —
		// with no Jira tools in it. (A request carrying NO Authorization header at all does 401.) So the
		// authentication failure surfaces HERE, as a missing tool set, not as a transport error; without this
		// assertion the leg would pass hollowly against an unauthenticated session.
		t.Fatalf("connected to %s but no Jira tool is present among the %d advertised: %v\n\n"+
			"This is what an UNACCEPTED credential looks like on this endpoint — it does not 401, it drops you\n"+
			"to the anonymous tool set. Check, in order:\n"+
			"  - the scheme: a PERSONAL API token is Basic base64(email:token), not Bearer;\n"+
			"  - that an org admin has enabled API-token auth for the Rovo MCP server;\n"+
			"  - that the token has not expired or been revoked;\n"+
			"  - that the account can actually see a Jira project.", url, len(tools), names)
	}
	// The tool names are the ones to register a connection against; the descriptions are UNTRUSTED and are
	// deliberately not printed.
	t.Logf("%s advertised %d tools, Jira among them: %v", url, len(tools), names)

	// E23 T5: THE WRITE TOOLS, BY NAME. Palai can now publish these behind a human's button (§3b of
	// docs/operations/jira-mcp-connection.md), and whether this tenant's credential can even SEE them is a
	// question only the real server answers. It is asserted by NAME rather than by a successful call for
	// the J5 reason above — an unaccepted credential does not 401, it silently drops to the anonymous set,
	// so "the call succeeded" and "the tool exists" are different measurements and only the second one is
	// available read-only.
	//
	// This leg STOPS HERE by construction. Actually transitioning an issue is an irreversible change to
	// somebody's tracker, and this file is documented as safe to run against a production Atlassian site;
	// the post-approval write is proven against a fake peer built to the published protocol, in
	// apps/control-plane/internal/execution/mcp_write_approval_component_test.go, which drives the REAL
	// orchestrator and counts the requests that reach the server.
	var writeTools []string
	for _, tool := range tools {
		switch tool.Name {
		case "transitionJiraIssue", "transitionIssue", "addCommentToJiraIssue":
			writeTools = append(writeTools, tool.Name)
		}
	}
	if len(writeTools) == 0 {
		t.Logf("NOTE: no Jira WRITE tool is advertised to this credential (looked for transitionJiraIssue, "+
			"transitionIssue, addCommentToJiraIssue among %v). Read tools work; publishing a gated write tool "+
			"would have nothing to bind to. That is an Atlassian permission question, not a Palai one.", names)
	} else {
		t.Logf("WRITE TOOLS AVAILABLE: %v — publish each with {\"approval_required\":true} and an operator "+
			"label; see docs/operations/jira-mcp-connection.md §3b", writeTools)
	}
}
