// THE MCP SERVER CATALOGUE — a directory of known public MCP servers, and an HONEST statement of which of
// them this deployment can actually dial.
//
// WHY A CATALOGUE AT ALL. /tools asks an operator to type a URL. Every reference console instead opens a
// directory: pick "Linear", get the endpoint. Measured in this tree before this file existed (2026-08-01):
//
//   grep -rn 'mcp\.slack\.com' --include='*.ts' --include='*.tsx' --include='*.go' . | wc -l   → 4
//
// and all four are prose inside evidence/releases/runner-fleet-0.1.0/manifest.json — shipped words ABOUT the
// Slack MCP server, in a release bundle, with no catalogue anywhere that an operator could see.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────────────────
// THE CEILING, MEASURED IN OUR OWN TRANSPORT — AND IT IS NARROWER THAN "OAUTH IS UNSUPPORTED"
// ─────────────────────────────────────────────────────────────────────────────────────────────────────────
//
// A catalogue that offers a "Connect" button which fails at the end is worse than no catalogue. So every
// entry below carries the reason it can or cannot be connected, and the reason is derived from THREE facts
// in this repository rather than from a vendor's marketing:
//
//   1. adapters/integrations/mcp/http.go:70
//        var authSchemes = []string{"Bearer ", "Basic "}
//      A connection's secret may name its OWN scheme, and exactly two are recognised. authorizationHeader()
//      (http.go:72-95) prefixes anything else with "Bearer ". So a credential that must ride a THIRD scheme
//      cannot be rendered: stored as `Sentry-Bearer abc`, it goes out as `Bearer Sentry-Bearer abc`.
//
//   2. adapters/integrations/mcp/http.go:213-224
//      The transport sets exactly five headers — Content-Type, Accept, Origin, Authorization, Mcp-Session-Id.
//      There is NO per-connection header map anywhere in the config allow-list
//      (apps/control-plane/internal/extensions/mcp.go:200 allows url, audience, oauth, sampling,
//      sampling_max_tokens). A server authenticating on `X-API-Key` is therefore unreachable, and no amount
//      of operator diligence changes that.
//
//   3. THERE IS NO OAUTH FLOW, AND THE `oauth` CONFIG BLOCK IS NOT EVIDENCE OF ONE. This is the trap this
//      file exists to not fall into. `config.oauth` IS accepted and validated at registration
//      (extensions/mcp.go:191-199 → mcp.ValidateOAuthMetadata). Nothing ever reads it again — measured:
//
//        grep -rn '"oauth"\|authorization_endpoint\|token_endpoint\|code_verifier\|grant_type' \
//          --include='*.go' . | grep -v '_test.go' | wc -l   → 4   (2026-08-01)
//
//      and all four hits are the validator and its one caller. No token exchange, no redirect, no
//      code_verifier, anywhere. The validator says so itself at adapters/integrations/mcp/auth.go:86-88:
//      "HONEST CEILING: this validates SHAPE only. Palai runs NO interactive OAuth flow (no
//      authorization-code redirect, no token exchange) — that is E17." E17 shipped and did not build it;
//      that comment is a stale forward-reference, and the grep above is what settles it.
//
// So the question "can we connect to X" is NOT "does X support OAuth". It is: CAN A STATIC STRING, SENT AS
// `Authorization: Bearer …` OR `Authorization: Basic …`, AUTHENTICATE THIS SERVER? Three of the buckets
// below answer no, and they fail for three different reasons.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────────────────
// HOW THE CLASSIFICATION WAS ESTABLISHED, AND WHY PROBING COULD NOT DO IT
// ─────────────────────────────────────────────────────────────────────────────────────────────────────────
//
// Each entry cites vendor documentation, because a live probe CANNOT answer this question. Measured
// (2026-08-01) — an unauthenticated `initialize` against five endpoints returned an indistinguishable
// challenge:
//
//   github     401  www-authenticate: Bearer error="invalid_request", resource_metadata="…"
//   atlassian  401  www-authenticate: Bearer realm="OAuth", error="invalid_token"
//   slack      401  www-authenticate: Bearer resource_metadata="…"
//   linear     401  www-authenticate: Bearer realm="OAuth", error="invalid_token"
//   notion     401  www-authenticate: Bearer realm="OAuth", error="invalid_token"
//
// GitHub, Atlassian and Linear accept a pasted token; Slack and Notion do not. The challenge is identical.
// A probe therefore proves connectability only in ONE direction — a 200 to an unauthenticated initialize
// proves a server needs no credential, which is how the two `none` entries below were confirmed:
//
//   deepwiki   200  {"result":{"protocolVersion":"2025-06-18", …}}
//   context7   200  {"result":{"protocolVersion":"2025-06-18", …}}
//
// ─────────────────────────────────────────────────────────────────────────────────────────────────────────
// WHY THIS CATALOGUE IS HTTP-ONLY
// ─────────────────────────────────────────────────────────────────────────────────────────────────────────
//
// A stdio connection requires a sha256-pinned image digest — validateConnectionConfig demands one
// (extensions/mcp.go:166-169, "stdio needs a sha256 image_digest"). A checked-in catalogue cannot carry
// pinned digests without them rotting into a lie the first time a vendor pushes a tag. So servers that ship
// only as a local npx/uvx process (MongoDB's `mongodb-mcp-server`, Chroma's `chroma-mcp`) are ABSENT rather
// than listed-and-broken: there is no URL for an operator to register.

/**
 * How a server authenticates, in the only terms that matter here — what our transport would have to send.
 *
 * The three refusing buckets are kept SEPARATE rather than folded into one "unsupported", because they are
 * three different facts about the world and an operator asking "why not" deserves the real one. Only
 * `oauth` would be fixed by building an authorization-code flow; `customScheme` and `customHeader` would
 * still fail the morning after that epic shipped.
 */
export type CatalogueAuth =
  /** No credential at all. The easiest thing this deployment can connect, and confirmed by probe. */
  | "none"
  /** A pasteable long-lived token, sent `Authorization: Bearer <token>`. */
  | "bearer"
  /** A pasteable static credential, sent `Authorization: Basic <base64>`. */
  | "basic"
  /** Requires an interactive authorization-code redirect and/or Dynamic Client Registration. */
  | "oauth"
  /** Wants an `Authorization` scheme that is neither Bearer nor Basic — see http.go:70. */
  | "customScheme"
  /** Authenticates on a header the transport never sends — see http.go:213-224. */
  | "customHeader";

/** The buckets a static secret_ref can satisfy. Everything else is shown, and shown as unconnectable. */
const CONNECTABLE: readonly CatalogueAuth[] = ["none", "bearer", "basic"];

export interface CatalogueEntry {
  /** Stable id — the registration form's suggested connection name, and this row's test id suffix. */
  id: string;
  /** The vendor's name for the server, as the vendor writes it. */
  name: string;
  /** One line: what a model gets by calling this server. */
  blurb: string;
  /** The Streamable-HTTP endpoint, verbatim from vendor documentation. */
  url: string;
  auth: CatalogueAuth;
  /**
   * What the operator must paste, for a connectable entry — the SHAPE of the secret, never a value.
   *
   * For `basic` this matters more than it looks: our transport sends the secret VERBATIM when it already
   * names a scheme (http.go:89-93), so the stored secret must be the whole `Basic base64(...)` string. An
   * operator who stores the bare base64 gets `Bearer <base64>` on the wire and a 401 they cannot explain.
   */
  credential?: string;
  /** For a refused entry: the specific reason, in one sentence an operator can act on. */
  refusal?: string;
  /** The documentation URL the classification was read from. */
  evidence: string;
  /** A verbatim sentence from `evidence` that establishes the bucket. */
  quote: string;
  /** Anything true that the bucket alone does not convey. Rendered under the entry when present. */
  caveat?: string;
}

/** Whether this deployment can dial the entry today. Derived — never stored, so it cannot drift. */
export const isConnectable = (entry: CatalogueEntry): boolean => CONNECTABLE.includes(entry.auth);

/**
 * THE CATALOGUE. Sorted connectable-first inside each group by MCP_CATALOGUE_GROUPS below, so the screen
 * opens on what works.
 *
 * Every `quote` is verbatim from its `evidence` URL, read 2026-08-01.
 */
export const MCP_CATALOGUE: readonly CatalogueEntry[] = [
  // ── No credential at all ────────────────────────────────────────────────────────────────────────────
  {
    id: "deepwiki",
    name: "DeepWiki",
    blurb: "Ask questions about any public GitHub repository's documentation.",
    url: "https://mcp.deepwiki.com/mcp",
    auth: "none",
    evidence: "https://docs.devin.ai/work-with-devin/deepwiki-mcp",
    quote:
      "The DeepWiki MCP server is a free, remote, no-authentication-required service that provides access to public repositories.",
    caveat:
      "Confirmed by probe: an unauthenticated initialize returned HTTP 200 and a protocol handshake (2026-08-01). Private repositories are a different server needing a Devin API key, not a credential on this one.",
  },
  {
    id: "context7",
    name: "Context7",
    blurb: "Up-to-date API documentation and code examples for thousands of libraries.",
    url: "https://mcp.context7.com/mcp",
    auth: "none",
    credential: "Optional. A `ctx7sk…` key raises the rate limit; paste it bare and it is sent as Bearer.",
    evidence: "https://context7.com/docs/api-guide.md",
    quote: "Without API key: Low rate limits and no custom configuration",
    caveat:
      "Confirmed by probe: an unauthenticated initialize returned HTTP 200 (2026-08-01). This is the one entry that works both with and without a secret_ref.",
  },
  {
    id: "exa",
    name: "Exa Search",
    blurb: "Neural web search and content retrieval built for models.",
    url: "https://mcp.exa.ai/mcp",
    auth: "none",
    evidence: "https://exa.ai/docs/reference/exa-mcp",
    quote: "Exa MCP has a generous free plan",
    caveat:
      "ANONYMOUS TIER ONLY. Exa's keyed path is an `x-api-key` header, which this transport cannot send (http.go:213-224); its documented alternative puts the key in the query string, which would store a live credential as plaintext in the connection's config JSONB. Register this one keyless or not at all.",
  },
  {
    id: "shopify-storefront",
    name: "Shopify Storefront",
    blurb: "Search a storefront's catalogue and build a cart.",
    url: "https://{shop}.myshopify.com/api/mcp",
    auth: "none",
    evidence: "https://shopify.dev/docs/apps/build/storefront-mcp/servers/storefront",
    quote: "Storefront MCP servers don't require authentication.",
    caveat:
      "PER-STORE. Replace {shop} with your own myshopify subdomain before registering — the URL above is a template and will not resolve as written.",
  },

  // ── A pasteable static token ────────────────────────────────────────────────────────────────────────
  {
    id: "github",
    name: "GitHub",
    blurb: "Repositories, issues, pull requests and code search.",
    url: "https://api.githubcopilot.com/mcp/",
    auth: "bearer",
    credential: "A GitHub Personal Access Token.",
    evidence: "https://github.com/github/github-mcp-server",
    quote: '"headers": { "Authorization": "Bearer ${input:github_mcp_pat}" }',
    caveat: "GitHub Enterprise deployments use https://<host>/mcp with the same Bearer PAT.",
  },
  {
    id: "linear",
    name: "Linear",
    blurb: "Issues, projects and cycles.",
    url: "https://mcp.linear.app/mcp",
    auth: "bearer",
    credential: "A Linear API key.",
    evidence: "https://linear.app/docs/mcp",
    quote:
      "The MCP server supports passing OAuth token and API keys directly in the Authorization: Bearer <yourtoken> header instead of using the interactive authentication flow.",
    caveat: "https://mcp.linear.app/mcp/readonly is the same server with write tools withheld.",
  },
  {
    id: "atlassian",
    name: "Atlassian (Jira & Confluence)",
    blurb: "Jira issues and Confluence pages through the Rovo server.",
    url: "https://mcp.atlassian.com/v1/mcp",
    auth: "basic",
    credential:
      "The WHOLE header value: `Basic ` followed by base64(email:api_token). Store the scheme with it — a bare base64 string would be re-wrapped as Bearer and refused.",
    evidence:
      "https://support.atlassian.com/atlassian-rovo-mcp-server/docs/configuring-authentication-via-api-token/",
    quote: "Personal API tokens using Basic auth: Authorization: Basic <base64(email:api_token)>",
    caveat:
      "An org admin must enable API-token auth first — OAuth allowlists do not apply to it, so it is off until switched on. A service-account API key works too, as `Bearer <api_key>`. This is the one entry with a shipped in-tree proof: apps/control-plane/internal/extensions/mcp_jira_component_test.go drives a real Basic credential end to end.",
  },
  {
    id: "cloudflare",
    name: "Cloudflare",
    blurb: "Workers, DNS, and account configuration.",
    url: "https://mcp.cloudflare.com/mcp",
    auth: "bearer",
    credential: "A Cloudflare API token.",
    evidence:
      "https://developers.cloudflare.com/agents/model-context-protocol/cloudflare/servers-for-cloudflare/",
    quote:
      "For CI/CD or automation, you can create a Cloudflare API token with the permissions you need and pass it as a bearer token in the Authorization header.",
    caveat:
      "Cloudflare ships a family of servers. https://docs.mcp.cloudflare.com/mcp is documentation-only and needs no credential.",
  },
  {
    id: "supabase",
    name: "Supabase",
    blurb: "Project schema, migrations and edge functions.",
    url: "https://mcp.supabase.com/mcp",
    auth: "bearer",
    credential: "A Supabase personal access token.",
    evidence: "https://supabase.com/docs/guides/getting-started/mcp",
    quote: '"headers": { "Authorization": "Bearer ${SUPABASE_ACCESS_TOKEN}" }',
    caveat: "Append ?project_ref=<ref> to scope the connection to one project.",
  },
  {
    id: "neon",
    name: "Neon",
    blurb: "Postgres branches, queries and migrations.",
    url: "https://mcp.neon.tech/mcp",
    auth: "bearer",
    credential: "A Neon API key.",
    evidence: "https://neon.com/docs/ai/connect-mcp-clients-to-neon",
    quote: 'For API key authentication, add --header "Authorization: Bearer $NEON_API_KEY".',
  },
  {
    id: "stripe",
    name: "Stripe",
    blurb: "Customers, payments, subscriptions and invoices.",
    url: "https://mcp.stripe.com",
    auth: "bearer",
    credential: "A Stripe restricted API key.",
    evidence: "https://docs.stripe.com/mcp",
    quote:
      "We strongly recommend using restricted API keys to limit your agent's access to exactly the functionality it requires.",
    caveat: "A live secret key here grants a model your whole Stripe account. Restrict the key first.",
  },
  {
    id: "airtable",
    name: "Airtable",
    blurb: "Bases, tables and records.",
    url: "https://mcp.airtable.com/mcp",
    auth: "bearer",
    credential: "An Airtable personal access token.",
    evidence: "https://support.airtable.com/docs/using-the-airtable-mcp-server",
    quote:
      "Alternatively, you may also authenticate with a personal access token if your AI tool supports custom request headers or authentication tokens.",
  },
  {
    id: "monday",
    name: "monday.com",
    blurb: "Boards, items and updates.",
    url: "https://mcp.monday.com/mcp",
    auth: "bearer",
    credential: "A monday.com personal API token.",
    evidence: "https://developer.monday.com/api-reference/docs/integrate-with-monday-mcp",
    quote:
      "Use a personal API token when your client isn't a compatible MCP client or doesn't support the MCP OAuth flow",
  },
  {
    id: "intercom",
    name: "Intercom",
    blurb: "Conversations, contacts and help-centre articles.",
    url: "https://mcp.intercom.com/mcp",
    auth: "bearer",
    credential: "An Intercom private-app access token.",
    evidence: "https://developers.intercom.com/docs/guides/mcp",
    quote: '"AUTH_HEADER": "Bearer YOUR_INTERCOM_API_TOKEN"',
    caveat: "EU and AU workspaces use https://mcp.eu.intercom.com/mcp and https://mcp.au.intercom.com/mcp.",
  },
  {
    id: "zapier",
    name: "Zapier",
    blurb: "Whichever Zapier actions you expose to this server.",
    url: "https://mcp.zapier.com/api/v1/connect",
    auth: "bearer",
    credential: "The token minted in the Zapier MCP Connect tab.",
    evidence: "https://help.zapier.com/hc/en-us/articles/36265392843917-Use-Zapier-MCP-with-your-client",
    quote:
      "Add Authorization: Bearer as a header to your client configuration and connect to https://mcp.zapier.com/api/v1/connect.",
    caveat: "Zapier shows the token once, at creation. It is rotatable but not re-readable.",
  },
  {
    id: "huggingface",
    name: "Hugging Face",
    blurb: "Models, datasets and Spaces.",
    url: "https://huggingface.co/mcp",
    auth: "bearer",
    credential: "A Hugging Face access token.",
    evidence: "https://github.com/huggingface/hf-mcp-server",
    quote: "HTTP clients must send a Hugging Face token in the Authorization: Bearer header",
  },
  {
    id: "firecrawl",
    name: "Firecrawl",
    blurb: "Scrape, crawl and parse web pages into model-ready text.",
    url: "https://mcp.firecrawl.dev/v2/mcp",
    auth: "bearer",
    credential: "A Firecrawl API key. Optional — the endpoint also serves a reduced keyless tier.",
    evidence: "https://docs.firecrawl.dev/mcp-server/connect",
    quote:
      "Keep the API key in an environment variable or secret store and send it as an Authorization header.",
  },
  {
    id: "sanity",
    name: "Sanity",
    blurb: "Content documents, schemas and datasets.",
    url: "https://mcp.sanity.io",
    auth: "bearer",
    credential: "A Sanity API token (`sk…`).",
    evidence: "https://www.sanity.io/docs/ai/mcp-server",
    quote:
      "You may instead provide an API token by setting the Authorization header in your MCP config. When configured with the header, the server will not use OAuth.",
  },
  {
    id: "paypal",
    name: "PayPal",
    blurb: "Invoices, orders, subscriptions and disputes.",
    url: "https://mcp.paypal.com/http",
    auth: "bearer",
    credential: "An access token minted from your PayPal client credentials.",
    evidence: "https://docs.paypal.ai/developer/tools/ai/mcp-quickstart",
    quote: "PayPal supports using your PayPal client credentials to connect to the remote MCP server.",
    caveat:
      "THE TOKEN EXPIRES IN HOURS, and nothing here refreshes it — a connection registered today stops working overnight and the failure surfaces as a 401 at dispatch. Sandbox is https://mcp.sandbox.paypal.com/http.",
  },
  {
    id: "netlify",
    name: "Netlify",
    blurb: "Sites, deploys and environment variables.",
    url: "https://netlify-mcp.netlify.app/mcp",
    auth: "bearer",
    credential: "A Netlify personal access token (`nfp_…`).",
    evidence: "https://github.com/netlify/netlify-mcp/blob/main/src/utils/api-networking.ts",
    quote:
      "if (bearerToken.startsWith('nfu') || bearerToken.startsWith('nfp') || bearerToken.startsWith('nfo')) { token = bearerToken; }",
    caveat:
      "SOURCE-VERIFIED, NOT DOCUMENTED. No Netlify docs page states that the hosted endpoint accepts a PAT; the classification comes from the server's own request handler. Treat it as the least certain row here.",
  },

  // ── Refused: an interactive OAuth flow is the only documented way in ─────────────────────────────────
  {
    id: "slack",
    name: "Slack",
    blurb: "Channels, messages and search.",
    url: "https://mcp.slack.com/mcp",
    auth: "oauth",
    refusal:
      "Slack's MCP server takes only a USER token minted through a confidential OAuth redirect, and it does not support Dynamic Client Registration. There is no token you can paste.",
    evidence: "https://docs.slack.dev/ai/slack-mcp-server/",
    quote:
      "Slack supports confidential OAuth for MCP clients. You'll need to use your app's client_id and client_secret for Slack OAuth.",
    caveat:
      "THIS DEPLOYMENT ALREADY TALKS TO SLACK ANOTHER WAY. E21 built an internal Slack tool against the bot token rather than opening an OAuth epic to reach a method it could already call — see evidence/releases/runner-fleet-0.1.0/manifest.json. You do not need this row to use Slack here.",
  },
  {
    id: "notion",
    name: "Notion",
    blurb: "Pages, databases and comments.",
    url: "https://mcp.notion.com/mcp",
    auth: "oauth",
    refusal: "Notion states outright that its MCP server refuses bearer tokens.",
    evidence: "https://developers.notion.com/docs/get-started-with-mcp",
    quote:
      "Notion MCP requires user-based OAuth authentication and does not support bearer token authentication.",
  },
  {
    id: "figma",
    name: "Figma",
    blurb: "Files, components and design context.",
    url: "https://mcp.figma.com/mcp",
    auth: "oauth",
    refusal:
      "Figma documents only an OAuth sign-in for the remote server, and the mcp:connect scope is not granted to general third-party apps — so this may stay unreachable even after an OAuth flow exists.",
    evidence: "https://developers.figma.com/docs/figma-mcp-server/remote-server-installation/",
    quote: "To use this server, you'll need to sign in through Figma's OAuth authentication flow.",
  },
  {
    id: "vercel",
    name: "Vercel",
    blurb: "Projects, deployments and logs.",
    url: "https://mcp.vercel.com",
    auth: "oauth",
    refusal:
      "OAuth is the only documented path, and Vercel additionally admits only clients it has reviewed — two gates, not one.",
    evidence: "https://vercel.com/docs/agent-resources/vercel-mcp",
    quote: "Vercel MCP only supports AI clients that have been reviewed and approved by Vercel",
  },
  {
    id: "gitlab",
    name: "GitLab",
    blurb: "Projects, merge requests and pipelines.",
    url: "https://gitlab.com/api/v4/mcp",
    auth: "oauth",
    refusal:
      "GitLab's MCP server authenticates by Dynamic Client Registration only; PAT support is an open, unimplemented feature request.",
    evidence: "https://docs.gitlab.com/user/model_context_protocol/mcp_server/",
    quote: "The GitLab MCP server supports OAuth 2.0 Dynamic Client Registration",
  },
  {
    id: "asana",
    name: "Asana",
    blurb: "Tasks, projects and portfolios.",
    url: "https://mcp.asana.com/v2/mcp",
    auth: "oauth",
    refusal:
      "Asana requires a registered client id and secret, and tokens minted for MCP expire in an hour and work with no other Asana API.",
    evidence: "https://developers.asana.com/docs/connecting-mcp-clients-to-asanas-v2-server",
    quote:
      "The V2 MCP server (https://mcp.asana.com/v2/mcp) requires OAuth 2.0 authentication with a registered client ID and client secret.",
  },
  {
    id: "clickup",
    name: "ClickUp",
    blurb: "Tasks, lists and docs.",
    url: "https://mcp.clickup.com/mcp",
    auth: "oauth",
    refusal: "ClickUp refuses API keys for MCP explicitly.",
    evidence: "https://developer.clickup.com/docs/connect-an-ai-assistant-to-clickups-mcp-server",
    quote:
      "No, you cannot authenticate using your own API keys or Auth access tokens. We only support OAuth for authentication.",
  },
  {
    id: "canva",
    name: "Canva",
    blurb: "Designs, brand templates and exports.",
    url: "https://mcp.canva.com/mcp",
    auth: "oauth",
    refusal: "Canva's server authenticates by authorization-code with PKCE; there is no static-token path.",
    evidence: "https://www.canva.dev/docs/mcp/",
    quote: "Client ID Metadata Documents (CIMD) is the recommended authentication method for MCP.",
  },
  {
    id: "box",
    name: "Box",
    blurb: "Files, folders and metadata.",
    url: "https://mcp.box.com",
    auth: "oauth",
    refusal:
      "Box does take a Bearer token, but every Box token expires after 60 minutes and developer tokens cannot be refreshed programmatically — so there is no credential an operator can paste that survives the hour.",
    evidence: "https://developer.box.com/guides/box-mcp/setup",
    quote: "Authorization: Bearer token (via OAuth 2.0)",
    caveat:
      "THE ONE REFUSAL THAT COULD FLIP ON OUR SIDE RATHER THAN THEIRS. If this deployment ever grows a credential-refresh loop, Box's client-credentials grant becomes viable with no OAuth redirect at all.",
  },
  {
    id: "webflow",
    name: "Webflow",
    blurb: "Sites, collections and CMS items.",
    url: "https://mcp.webflow.com/mcp",
    auth: "oauth",
    refusal: "Webflow documents OAuth as the mechanism its remote endpoint exists to enable.",
    evidence: "https://developers.webflow.com/mcp/reference/how-it-works",
    quote: "The server runs remotely at https://mcp.webflow.com/mcp to enable OAuth authentication.",
    caveat:
      "UNTESTED IN THE OTHER DIRECTION: no Webflow document says a Site Token would be rejected, only that OAuth is what is documented. This row is a docs-based refusal, not a measured one.",
  },
  {
    id: "square",
    name: "Square",
    blurb: "Payments, catalogue and orders.",
    url: "https://mcp.squareup.com/sse",
    auth: "oauth",
    refusal:
      "The remote Square server is OAuth-only; the static ACCESS_TOKEN belongs to the local npm package, not this endpoint.",
    evidence: "https://github.com/square/square-mcp-server",
    quote:
      "The remote MCP is recommended as it uses OAuth authentication, allowing you to log in with your Square account directly without having to create or manage access tokens manually.",
  },
  {
    id: "prisma",
    name: "Prisma",
    blurb: "Databases, schemas and Prisma Postgres projects.",
    url: "https://mcp.prisma.io/mcp",
    auth: "oauth",
    refusal:
      "Every documented Prisma config is a bare URL plus a browser sign-in; neither the docs nor the repo mention an Authorization header.",
    evidence: "https://www.prisma.io/docs/ai/tools/mcp-server",
    quote:
      "The server authenticates with Prisma Console on first use so your AI tool can access the workspace you choose.",
  },
  {
    id: "grafana",
    name: "Grafana Cloud",
    blurb: "Dashboards, datasources and incidents.",
    url: "https://mcp.grafana.com/mcp",
    auth: "oauth",
    refusal:
      "The hosted Grafana Cloud server is OAuth 2.1 only. Service-account tokens are documented for the self-hosted server, which is a different endpoint.",
    evidence: "https://grafana.com/docs/grafana-cloud/machine-learning/assistant/configure/cloud-mcp/",
    quote: "the Grafana Cloud MCP server is fully hosted and uses OAuth 2.1 authorization",
    caveat:
      "A DIFFERENT GRAFANA ENDPOINT IS CONNECTABLE: the Tempo traces server at https://<stack>.grafana.net/tempo/api/mcp takes `Authorization: Basic base64(userID:token)`. It is not listed as its own row because its URL is per-stack and cannot be written down here.",
  },

  // ── Refused: an Authorization scheme this transport cannot render ────────────────────────────────────
  {
    id: "sentry",
    name: "Sentry",
    blurb: "Issues, events, releases and Seer root-cause analysis.",
    url: "https://mcp.sentry.dev/mcp",
    auth: "customScheme",
    refusal:
      "Sentry's static-token path uses a `Sentry-Bearer` scheme, and reserves plain `Bearer` for its own OAuth tokens. This transport recognises only `Bearer ` and `Basic ` (http.go:70), so a Sentry token would go out as `Bearer Sentry-Bearer <token>` and be refused.",
    evidence: "https://github.com/getsentry/sentry-mcp",
    quote:
      "Sentry-Bearer is intentionally separate from Bearer: Bearer is reserved for MCP OAuth access tokens.",
    caveat:
      "THIS ONE LOOKS CONNECTABLE AND IS NOT. Sentry documents a headless token, which reads like every other row in the connectable group above — the incompatibility is one word in a scheme name. Adding \"Sentry-Bearer \" to authSchemes in adapters/integrations/mcp/http.go:70 is a one-line change that would move this row, and it is the cheapest catalogue expansion available.",
  },
];

/** The screen's section order. `refused` is last because the screen should open on what works. */
export const MCP_CATALOGUE_GROUPS = [
  {
    id: "ready",
    heading: "Connect these today",
    blurb:
      "A static credential — or none at all — is enough. Register one below, then run discovery against it.",
    match: (e: CatalogueEntry) => isConnectable(e),
  },
  {
    id: "refused",
    heading: "Cannot be connected from this deployment",
    blurb:
      "Listed so the gap is visible rather than discovered at the end of a registration. Each row says what would have to change.",
    match: (e: CatalogueEntry) => !isConnectable(e),
  },
] as const;

/** One line naming the mechanism, for the entry's badge. Kept short — the reason lives in `refusal`. */
export const AUTH_LABEL: Record<CatalogueAuth, string> = {
  none: "No credential",
  bearer: "Static token",
  basic: "Static token (Basic)",
  oauth: "Needs OAuth",
  customScheme: "Unsupported scheme",
  customHeader: "Unsupported header",
};
