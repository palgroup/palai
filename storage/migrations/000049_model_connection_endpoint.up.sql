-- 000049 (E29 provider wiring): the ENDPOINT a model connection dials.
--
-- WHAT WAS MISSING. `model_connections` (000001) carries a provider family and a secret reference, which is
-- everything an OpenAI or an Anthropic connection needs — those families have one endpoint and it is
-- theirs. It is not everything a CUSTOM OpenAI-compatible connection needs, and that is the third shape a
-- self-hosted operator asks for by name: their own vLLM, their own Ollama, their own gateway. Until this
-- column the only way to move that endpoint was PALAI_OPENAI_COMPATIBLE_BASE_URL, read once at boot into a
-- single adapter value — so the endpoint was a property of the DEPLOYMENT. Two projects on one stack could
-- not reach two endpoints, an operator could not name one from the console at all, and changing it meant
-- editing a dotenv file and restarting the control plane.
--
-- NOT NULL DEFAULT '' rather than NULLable, for the reason 000046 and 000048 give: every existing row on
-- every deployment has no endpoint, and '' is ONE kind of nothing where NULL plus '' would be two. Every
-- reader would then need to handle both, and this tree has shipped a LEFT JOIN whose NULL arm decided a
-- security outcome — one spelling of absent is worth a column default.
--
-- IT IS AN ADDRESS THE CONTROL PLANE WILL DIAL, so it is vetted BEFORE it is stored, in the store's write
-- path, through packages/egress (the shared SSRF layer the webhook sender, the web-research tool and the
-- MCP/A2A transports all vet through). Private and loopback destinations are ALLOWED here and that is a
-- decision rather than an oversight: a self-hosted deployment's own Ollama lives on localhost or on the
-- LAN, and refusing those would refuse the single most common custom endpoint there is. What stays denied
-- under that flag is what is never a legitimate destination for anybody — the cloud metadata address
-- (169.254.169.254 and the special-use ranges around it), unspecified, multicast — so the flag opens the
-- operator's own network and not the instance's credentials. There is no CHECK constraint here to say so:
-- a URL policy is not expressible in one and a half-expressed one would be read as the whole rule.
--
-- NO NEW TABLE, therefore no palai_apply_tenant_policy call: model_connections already carries
-- organization_id and 000029's catalogue loop has secured it since that migration ran. An ALTER that adds
-- a non-tenant column changes nothing about that policy (000043 is written the same way, and says so).
ALTER TABLE model_connections ADD COLUMN IF NOT EXISTS base_url TEXT NOT NULL DEFAULT '';

-- The verification stamp. It is a CACHE OF AN OBSERVATION, never a gate: nothing reads it to decide
-- whether a run may dispatch, and a connection that was never verified routes exactly like one that was.
-- The value of writing it down is that an operator looking at a list of connections can see which ones
-- have been shown to work and when — the question they actually have when a run fails at 3am.
--
-- THE OUTCOME IS STORED, NOT A BOOLEAN, because "not_probed" and "rejected" are different facts and a
-- boolean would flatten them into the same red. `verified_at` NULL means no probe has ever run.
ALTER TABLE model_connections ADD COLUMN IF NOT EXISTS verified_at TIMESTAMPTZ;
ALTER TABLE model_connections ADD COLUMN IF NOT EXISTS verification_outcome TEXT NOT NULL DEFAULT '';

INSERT INTO schema_migrations (version) VALUES (49) ON CONFLICT DO NOTHING;
