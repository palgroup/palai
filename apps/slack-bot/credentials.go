package main

import (
	"context"
	"fmt"
	"strings"

	palai "github.com/palgroup/palai/sdks/go"
)

// slackCredentials is what this process must hold to be a Slack bot: the app-level token it presents at
// connect, and the bot token every outbound call presents. Bytes rather than strings because they come
// from a redemption rather than a literal, and a []byte does not land in a %v by accident.
type slackCredentials struct {
	appToken []byte
	botToken []byte
}

// redeemSlackCredentials turns the bot row's secret HANDLES into the token bytes, over
// GET /v1/bots/{bot_id}/credentials.
//
// WHY A ROUTE AND NOT A READ OF THE SECRET STORE. A sealed secret has no read-back path — POST
// /v1/secret-refs is write-only and GET /v1/secret-refs/{name} answers metadata with no parameter that
// adds the value — and every in-tree resolver that opens one runs INSIDE the control-plane process,
// holding the master key. This process holds no master key. So the redemption happens over a route whose
// only input is a BOT ID: the control plane resolves the `_ref` keys in THAT row's own config and nothing
// else, which is why the route can hand back a value at all without becoming an arbitrary secret reader.
//
// The map comes back keyed by CONFIG KEY, so this function asks for exactly the two keys parseSlackConfig
// already refused to start without — it does not need to know, and is never told, what those handles are
// named.
//
// IT REFUSES ON A MISSING VALUE RATHER THAN CARRYING AN EMPTY ONE, and reads `Unresolved` to do it. A key
// absent from the map and a key holding "" are the same thing to a map lookup, and an empty xapp- token
// would surface much later as an opaque Slack auth failure. The row is the thing an operator repairs, so
// the refusal names the config key, and names the seal step if the control plane said the handle resolves
// to no sealed secret.
func redeemSlackCredentials(ctx context.Context, client *palai.Client, botID string, cfg slackConfig) (slackCredentials, error) {
	out, err := client.Bots.Credentials(ctx, botID)
	if err != nil {
		return slackCredentials{}, fmt.Errorf(
			"redeem this bot's sealed credentials (GET /v1/bots/%s/credentials): %w — "+
				"a 403 means this process's API key lacks the bots.credentials capability; "+
				"a 404 means the control plane serving this URL has no such bot, or is old enough not to have the route at all",
			botID, err)
	}
	unresolved := make(map[string]bool, len(out.Unresolved))
	for _, key := range out.Unresolved {
		unresolved[key] = true
	}

	var creds slackCredentials
	var missing []string
	for _, want := range []struct {
		key  string
		name string
		into *[]byte
	}{
		{key: "app_token_ref", name: cfg.AppTokenRef, into: &creds.appToken},
		{key: "bot_token_ref", name: cfg.BotTokenRef, into: &creds.botToken},
	} {
		value := out.Credentials[want.key]
		switch {
		case unresolved[want.key]:
			missing = append(missing, fmt.Sprintf("%s names %q and nothing is sealed under that name — seal it in the admin panel (Bots → this bot → Credentials)", want.key, want.name))
		case value == "":
			missing = append(missing, fmt.Sprintf("%s (%q) came back empty", want.key, want.name))
		default:
			*want.into = []byte(value)
		}
	}
	if len(missing) > 0 {
		return slackCredentials{}, fmt.Errorf(
			"bot %s cannot start: %s. It stops here rather than connect half-configured — a bot with no bot token dials Slack successfully and then cannot say a word, which looks like it is working",
			botID, strings.Join(missing, "; "))
	}
	return creds, nil
}
