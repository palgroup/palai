package main

import (
	"context"
	"fmt"

	palai "github.com/palgroup/palai/sdks/go"
)

// slackCredentials is what this process must hold to be a Slack bot: the app-level token it presents at
// connect, and the bot token every outbound call presents. Bytes rather than strings because they come
// from a redemption rather than a literal, and a []byte does not land in a %v by accident.
type slackCredentials struct {
	appToken []byte
	botToken []byte
}

// redeemSlackCredentials turns the bot row's secret HANDLES into the token bytes. It cannot, today, and
// this function exists to say so exactly once, at the one place it matters, rather than let the failure
// arrive as a 404 somewhere downstream.
//
// THE PLATFORM HAS NO READ-BACK PATH FOR A SEALED SECRET. Measured 2026-08-03 against the running control
// plane, not cited:
//
//	$ curl -XPOST …/v1/secret-refs -d '{"name":"probe-readback","value":"the-secret-value-xyz"}'  → 201
//	$ curl …/v1/secret-refs/probe-readback
//	  {"name":"probe-readback","object":"secret_ref","version":1,"updated_at":"2026-08-03T21:03:11+03:00"}
//
// Metadata only, and no parameter adds the value. The control plane says the same in its own words —
// apps/control-plane/api/secret_refs.go: "the value is envelope-encrypted at rest and has NO read-back
// path"; apps/control-plane/internal/identity/environments.go on the sibling surface: "THE VALUES ARE NOT
// HERE AND THERE IS NO PARAMETER THAT WOULD ADD THEM." Every resolver that DOES redeem one
// (slackSecretResolver, remoteToolSecretResolver, the MCP and A2A resolvers) runs INSIDE the control-plane
// process, holding the master key. This process holds no master key and is not that process.
//
// The one out-of-process redemption in the tree is apps/control-plane/internal/workers — a capability
// worker redeems a job-scoped handle over the worker gateway (Store.RedeemSecretHandle), guarded by a
// fence, a per-job scope and an expiry that rides the job's deadline. A long-lived bot has no job, no
// fence and no deadline, so that path is not reusable as it stands.
//
// SO THE BOT REFUSES, AND THE REFUSAL IS THE POINT. The alternative — reading SLACK_BOT_TOKEN out of this
// process's environment — is precisely the arrangement the bot registry exists to end: "an operator
// configures a bot in the admin panel, not in a file, and a Slack token is never this process's
// environment variable" (main.go's own opening). A bot that quietly fell back to the environment would
// make the console's sealing ceremony decorative, and nothing downstream would ever notice.
//
// WHAT CLOSES IT: a narrow control-plane route that resolves ONLY the handles named in the caller's own
// bot row — the shape of GET /v1/bots/{bot_id}/credentials — plus the SDK method for it. That is a
// deliberate hole in a stated security property and therefore an owner's decision, not one to make on the
// way past. It was not written here for a second reason as well: it is org-scoped
// (identity.SecretStore.Resolve takes an org, storage.WithTenant scopes it), and the tenancy surface it
// would have to be written against is being rewritten in this tree right now.
func redeemSlackCredentials(_ context.Context, _ *palai.Client, cfg slackConfig) (slackCredentials, error) {
	return slackCredentials{}, fmt.Errorf(
		"this bot's Slack tokens are sealed under %s and %s, and the platform has no way to hand them back: "+
			"POST /v1/secret-refs is write-only and GET /v1/secret-refs/{name} answers metadata only "+
			"(measured 2026-08-03; apps/control-plane/api/secret_refs.go: \"the value is envelope-encrypted at rest and has NO read-back path\"). "+
			"A relay process outside the control plane therefore cannot redeem its own credentials. "+
			"What is missing is a bot-scoped resolution route — GET /v1/bots/{bot_id}/credentials, resolving only the handles that bot's own config names — and the SDK method for it. "+
			"Setting SLACK_BOT_TOKEN in this process's environment is NOT the workaround: it is the arrangement the bot registry exists to end",
		cfg.AppTokenRef, cfg.BotTokenRef)
}
