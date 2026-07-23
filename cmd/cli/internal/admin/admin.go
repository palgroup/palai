// Package admin is the `palai org|project|apikey|secret` admin CLI: a thin authenticated HTTP client over
// the E13 provisioning + secret-ref APIs (router.go:152-187). It adds NO server surface — every subcommand
// maps to one existing endpoint. Until the E17 console it is the only human interface for tenancy admin
// (spec §47.6).
//
// CREDENTIAL HYGIENE (the load-bearing rule): the admin API key and any secret VALUE never ride argv. The
// key comes from --api-key-file (a path, not the value), $PALAI_API_KEY, or the initialised .palai stack;
// a secret value comes only from stdin. The one place a secret is written to stdout is an `apikey create`
// response's one-time plaintext key — the API's create-only field — which the CLI prints once and never
// retains.
package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/palgroup/palai/cmd/cli/internal/stack"
)

// Client speaks the admin API over one resolved base URL + bearer key. Out/In are the IO streams (os.Stdout
// and os.Stdin in main, buffers in tests); JSON toggles the raw-vs-human rendering.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
	Out     io.Writer
	In      io.Reader
	JSON    bool
}

// Run parses a resource's subcommand + flags, resolves the connection over the flag → env → .palai chain,
// and executes the request against Out/In. It is the entry point main dispatches the four admin cases to.
func Run(resource string, args []string, out io.Writer, in io.Reader) error {
	sub, f, pos, err := parse(resource, args)
	if err != nil {
		return err
	}
	baseURL, apiKey, err := resolve(f.baseURL, f.apiKeyFile)
	if err != nil {
		return err
	}
	c := &Client{
		BaseURL: baseURL,
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		Out:     out,
		In:      in,
		JSON:    f.json,
	}
	return c.execute(resource, sub, pos, f)
}

// flags holds every flag the four resources may register. Registration is per-resource, so an irrelevant
// flag on the wrong resource is a parse error (a typo guard), while the value fields stay in one struct.
type flags struct {
	baseURL       string
	apiKeyFile    string
	json          bool
	displayName   string
	project       string
	scopes        multiFlag
	expiresAt     string
	allowedModels string
	allowedTools  string
	defaultTools  string
	name          string
}

// multiFlag collects a repeatable string flag (--scope run --scope provision).
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// register defines the global flags plus the resource-specific ones. NOTE: there is deliberately no flag
// carrying a secret value — a secret value only ever comes from stdin.
func (f *flags) register(fs *flag.FlagSet, resource string) {
	fs.StringVar(&f.baseURL, "base-url", "", "control-plane base URL (else $PALAI_BASE_URL, then .palai)")
	fs.StringVar(&f.apiKeyFile, "api-key-file", "", "file holding the admin API key (else $PALAI_API_KEY, then .palai)")
	fs.BoolVar(&f.json, "json", false, "emit the raw JSON response instead of a human render")
	switch resource {
	case "org":
		fs.StringVar(&f.displayName, "display-name", "", "organization display name (create)")
	case "project":
		fs.StringVar(&f.displayName, "display-name", "", "project display name (create)")
		fs.StringVar(&f.allowedModels, "allowed-models", "", "comma-separated allowed models (set-policy)")
		fs.StringVar(&f.allowedTools, "allowed-tools", "", "comma-separated allowed tools (set-policy)")
		fs.StringVar(&f.defaultTools, "default-tools", "", "comma-separated default tools (set-policy)")
	case "apikey":
		fs.StringVar(&f.project, "project", "", "project id the key belongs to (create)")
		fs.Var(&f.scopes, "scope", "capability scope (repeatable; omit for a full-capability admin key)")
		fs.StringVar(&f.expiresAt, "expires-at", "", "RFC3339 expiry, optional (create)")
	case "secret":
		fs.StringVar(&f.name, "name", "", "secret name (create) — the VALUE is read from stdin")
	}
}

// parse builds the FlagSet for a resource and returns the subcommand, the parsed flags, and the positionals.
func parse(resource string, args []string) (sub string, f *flags, pos []string, err error) {
	if len(args) == 0 {
		return "", nil, nil, usageErr(resource)
	}
	sub = args[0]
	f = &flags{}
	fs := flag.NewFlagSet("palai "+resource+" "+sub, flag.ContinueOnError)
	f.register(fs, resource)
	pos, err = parseInterleaved(fs, args[1:])
	return sub, f, pos, err
}

// parseInterleaved parses flags and positionals in any order — the stdlib flag package stops at the first
// non-flag token, so this loop resumes past each positional to keep flags after an id working
// (`palai apikey revoke <id> --json`).
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// execute maps (resource, subcommand) to the one E13 endpoint it fronts and dispatches it.
func (c *Client) execute(resource, sub string, pos []string, f *flags) error {
	switch resource {
	case "org":
		switch sub {
		case "create":
			return c.do(http.MethodPost, "/v1/organizations", body(map[string]any{"display_name": f.displayName}))
		case "list":
			return c.do(http.MethodGet, "/v1/organizations", nil)
		case "get":
			id, err := arg(pos, "organization id")
			if err != nil {
				return err
			}
			return c.do(http.MethodGet, "/v1/organizations/"+esc(id), nil)
		}
	case "project":
		switch sub {
		case "create":
			return c.do(http.MethodPost, "/v1/projects", body(map[string]any{"display_name": f.displayName}))
		case "list":
			return c.do(http.MethodGet, "/v1/projects", nil)
		case "get":
			id, err := arg(pos, "project id")
			if err != nil {
				return err
			}
			return c.do(http.MethodGet, "/v1/projects/"+esc(id), nil)
		case "set-policy":
			id, err := arg(pos, "project id")
			if err != nil {
				return err
			}
			policy := map[string]any{}
			if f.allowedModels != "" {
				policy["allowed_models"] = csv(f.allowedModels)
			}
			if f.allowedTools != "" {
				policy["allowed_tools"] = csv(f.allowedTools)
			}
			if f.defaultTools != "" {
				policy["default_tools"] = csv(f.defaultTools)
			}
			if len(policy) == 0 {
				return errors.New("set-policy requires at least one of --allowed-models/--allowed-tools/--default-tools")
			}
			return c.do(http.MethodPatch, "/v1/projects/"+esc(id), body(map[string]any{"config_policy": policy}))
		}
	case "apikey":
		switch sub {
		case "create":
			b := map[string]any{"project_id": f.project}
			if len(f.scopes) > 0 {
				b["scopes"] = []string(f.scopes)
			}
			if f.expiresAt != "" {
				b["expires_at"] = f.expiresAt
			}
			return c.do(http.MethodPost, "/v1/api-keys", body(b))
		case "list":
			return c.do(http.MethodGet, "/v1/api-keys", nil)
		case "get":
			id, err := arg(pos, "key id")
			if err != nil {
				return err
			}
			return c.do(http.MethodGet, "/v1/api-keys/"+esc(id), nil)
		case "revoke":
			id, err := arg(pos, "key id")
			if err != nil {
				return err
			}
			return c.do(http.MethodPost, "/v1/api-keys/"+esc(id)+"/revoke", nil)
		}
	case "secret":
		switch sub {
		case "create":
			if f.name == "" {
				return errors.New("secret create requires --name (the VALUE is read from stdin)")
			}
			value, err := readSecret(c.In)
			if err != nil {
				return err
			}
			return c.do(http.MethodPost, "/v1/secret-refs", body(map[string]any{"name": f.name, "value": value}))
		case "list":
			return c.do(http.MethodGet, "/v1/secret-refs", nil)
		case "get":
			name, err := arg(pos, "secret name")
			if err != nil {
				return err
			}
			return c.do(http.MethodGet, "/v1/secret-refs/"+esc(name), nil)
		case "rotate":
			name, err := arg(pos, "secret name")
			if err != nil {
				return err
			}
			value, err := readSecret(c.In)
			if err != nil {
				return err
			}
			return c.do(http.MethodPost, "/v1/secret-refs/"+esc(name)+"/rotate", body(map[string]any{"value": value}))
		}
	}
	return usageErr(resource)
}

// do issues the authenticated request and renders the outcome. A body of nil sends no payload.
func (c *Client) do(method, path string, payload []byte) error {
	var r io.Reader
	if payload != nil {
		r = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return c.renderProblem(resp.StatusCode, raw)
	}
	return c.renderSuccess(raw)
}

// renderSuccess writes the response to Out: raw bytes in --json mode, indented JSON otherwise (a non-JSON
// body is passed through as-is).
func (c *Client) renderSuccess(raw []byte) error {
	if c.JSON || !json.Valid(raw) {
		return writeLine(c.Out, raw)
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return writeLine(c.Out, raw)
	}
	return writeLine(c.Out, buf.Bytes())
}

// renderProblem turns a >=400 response into a returned error (main prints it to stderr, exit 1). In --json
// mode the raw RFC9457 problem is written to stdout and the error stays terse; otherwise the error carries a
// human line built from the problem's title/detail/code/request id.
func (c *Client) renderProblem(status int, raw []byte) error {
	if c.JSON {
		if err := writeLine(c.Out, raw); err != nil {
			return err
		}
		return fmt.Errorf("request failed (HTTP %d)", status)
	}
	var p struct {
		Title     string `json:"title"`
		Detail    string `json:"detail"`
		Code      string `json:"code"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.Code == "" {
		return fmt.Errorf("request failed (HTTP %d): %s", status, strings.TrimSpace(string(raw)))
	}
	msg := p.Title
	if msg == "" {
		msg = p.Code
	}
	if p.Detail != "" {
		msg += ": " + p.Detail
	}
	msg += " (" + p.Code
	if p.RequestID != "" {
		msg += ", request " + p.RequestID
	}
	msg += ")"
	return errors.New(msg)
}

// resolve applies the flag → env → .palai chain for the base URL and the API key. The key comes from a file
// (the --api-key-file path or the .palai file) or the env — never a flag value.
func resolve(baseURLFlag, apiKeyFileFlag string) (baseURL, apiKey string, err error) {
	baseURL = firstNonEmpty(baseURLFlag, os.Getenv("PALAI_BASE_URL"))
	if apiKeyFileFlag != "" {
		raw, err := os.ReadFile(apiKeyFileFlag)
		if err != nil {
			return "", "", fmt.Errorf("read --api-key-file: %w", err)
		}
		apiKey = strings.TrimSpace(string(raw))
	} else {
		apiKey = os.Getenv("PALAI_API_KEY")
	}
	if baseURL == "" || apiKey == "" {
		dURL, dKey, derr := stack.AdminDefaults()
		if derr != nil {
			return "", "", fmt.Errorf("no base URL / API key from flags or env, and no initialised .palai: %w", derr)
		}
		baseURL = firstNonEmpty(baseURL, dURL)
		apiKey = firstNonEmpty(apiKey, dKey)
	}
	if baseURL == "" {
		return "", "", errors.New("no base URL: pass --base-url, set $PALAI_BASE_URL, or run `palai init`")
	}
	if apiKey == "" {
		return "", "", errors.New("no API key: pass --api-key-file, set $PALAI_API_KEY, or run `palai init`")
	}
	return baseURL, apiKey, nil
}

// readSecret reads a secret value from stdin (never argv). The trailing newline a shell pipe adds is
// trimmed; an empty stdin is an actionable error.
func readSecret(in io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(in, 64*1024))
	if err != nil {
		return "", fmt.Errorf("read secret value from stdin: %w", err)
	}
	v := strings.TrimRight(string(raw), "\r\n")
	if v == "" {
		return "", errors.New("no secret value on stdin (pipe it, e.g. `printf %s \"$VALUE\" | palai secret create --name db-url`)")
	}
	return v, nil
}

// body marshals a request body. The maps here are string/[]string only, so marshalling cannot fail.
func body(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// esc percent-escapes a user-chosen path segment (a secret name may carry reserved characters; ids are
// minted but escaping is uniformly correct).
func esc(s string) string { return url.PathEscape(s) }

// csv splits a comma-separated flag into a trimmed, non-empty slice.
func csv(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// arg returns the first positional or an actionable "missing X" error.
func arg(pos []string, what string) (string, error) {
	if len(pos) == 0 {
		return "", fmt.Errorf("missing %s", what)
	}
	return pos[0], nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// writeLine writes b to w, ensuring exactly one trailing newline.
func writeLine(w io.Writer, b []byte) error {
	_, err := w.Write(append(bytes.TrimRight(b, "\n"), '\n'))
	return err
}

// usageErr names the subcommands a resource accepts.
func usageErr(resource string) error {
	subs := map[string]string{
		"org":     "create --display-name <n> | list | get <org_id>",
		"project": "create --display-name <n> | list | get <prj_id> | set-policy <prj_id> --allowed-models <a,b>",
		"apikey":  "create --project <prj_id> [--scope <s>]... | list | get <key_id> | revoke <key_id>",
		"secret":  "create --name <n> (value on stdin) | list | get <name> | rotate <name> (value on stdin)",
	}
	return fmt.Errorf("usage: palai %s <%s>", resource, subs[resource])
}
