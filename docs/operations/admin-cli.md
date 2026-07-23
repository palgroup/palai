# Admin CLI — `palai org | project | apikey | secret`

The admin CLI is a **thin authenticated HTTP client** over the E13 provisioning and secret-ref APIs
(`POST/GET /v1/organizations`, `/v1/projects`, `/v1/api-keys`, `/v1/secret-refs`). It adds no server
surface — every subcommand maps to exactly one endpoint. Until the E17 web console, it is the only human
interface for tenancy administration (spec §47.6).

## Connecting

Each command resolves its base URL and admin API key over a **flag → env → `.palai`** chain:

| What | Flag | Env | Fallback |
|---|---|---|---|
| Base URL | `--base-url <url>` | `PALAI_BASE_URL` | `.palai/config.json` `base_url` |
| API key | `--api-key-file <path>` | `PALAI_API_KEY` | `.palai/api-key` (the bootstrap key) |

The key is a full-capability admin/bootstrap key (empty scope set) or any key holding the `provision`
capability. On a freshly `palai init`-ed stack the `.palai/api-key` bootstrap key already qualifies, so a
local operator needs no flags.

**Credential hygiene (hard rule):** the API key and any secret VALUE never appear in `argv`. The key comes
only from a file or env; a secret value comes only from stdin. There is deliberately no `--value` flag.

## Output

- Default: a human-readable render. Success prints the response as indented JSON; a non-2xx renders the
  RFC 9457 problem as a one-line error (`<title>: <detail> (<code>, request <id>)`) to stderr with exit 1.
- `--json`: prints the raw response (or the raw problem document) to stdout verbatim, for scripting.

## Organizations

```sh
palai org create --display-name "Acme"     # 201 → org + default project + one-time admin key (see below)
palai org list
palai org get <org_id>
```

`org create` opens a NEW tenant with no restart. Its response carries a one-time `admin_api_key.key`
plaintext — **capture it now; it is never shown again.** Pipe `--json` to a file with `umask 077` if you
need to store it.

## Projects

```sh
palai project create --display-name "checkout"
palai project list
palai project get <prj_id>
# §14 config_policy write-path (only the flags you pass are sent):
palai project set-policy <prj_id> --allowed-models m1,m2 --allowed-tools web_search --default-tools web_search
```

## API keys

```sh
palai apikey create --project <prj_id>                 # full-capability key for that project
palai apikey create --project <prj_id> --scope run     # least-privilege; repeat --scope to add more
palai apikey create --project <prj_id> --expires-at 2027-01-01T00:00:00Z
palai apikey list
palai apikey get <key_id>
palai apikey revoke <key_id>                            # idempotent
```

`apikey create` is the **one** command that prints a secret to stdout: the response's one-time `key` field
(the API's create-only disclosure). Every read (`list`/`get`) returns metadata only — the key is never
shown again. Store it immediately; the CLI does not retain it.

## Secrets (write-only)

A secret VALUE is written only — reads return metadata (name/version/updated_at), never the value. The
value is read from **stdin**, so it never lands in `argv`, shell history, or a log.

```sh
printf %s "$DATABASE_URL" | palai secret create --name db-url
palai secret list
palai secret get db-url                                 # metadata only
printf %s "$NEW_DATABASE_URL" | palai secret rotate db-url   # next resolve reads it, no restart
```

A rotation takes effect with no restart (the resolver reads the latest version fresh). `rotate` of a name
that was never created is a 404.

## Errors

Every non-2xx is an RFC 9457 problem. Common cases:

| Code | Meaning | Fix |
|---|---|---|
| `authentication_required` | no/blank bearer | set `--api-key-file` / `PALAI_API_KEY` |
| `invalid_token` | the bearer is present but not recognized (wrong/revoked/stale key) | use the key for THIS stack (`.palai/api-key` or one from `apikey create`) |
| `insufficient_scope` | the key lacks `provision` | use an admin key or a key with the capability |
| `invalid_request` | missing/unknown body field | check the flags for that subcommand |
| `not_found` | absent or foreign id/name | the resource is outside this key's organization |
