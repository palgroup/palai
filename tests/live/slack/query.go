// Package live holds the credential-gated Slack smokes. This file is DELIBERATELY UNTAGGED while every other
// file in the package carries //go:build live, and the reason is the whole point of it: a live test cannot be
// run without a real workspace, so the SQL it depends on would otherwise ship unexecuted. Keeping the query
// here lets a component test drive it against a real Postgres holding real Slack-born rows.
package live

// BornRunsByTeam finds the runs a Slack workspace's events gave birth to, since a moment, grouped by the
// SOURCE EVENT they came from. $1 is the team id, $2 the "since" timestamp; it returns (event_id, response_id).
//
// IT IS KEYED ON THE IDEMPOTENCY RESERVATION, and that is a correction rather than a preference. The E19
// live legs identified a Slack-born run by its stored input projection:
//
//	WHERE input->>'source' = 'slack' AND input->>'team_id' = $1
//
// That stopped being true the moment the run's input became THE PROMPT — a bare JSON string, because a map
// arrives at the provider as compact JSON and the model answers the JSON instead of the human (see
// slackRunInput). `->>` on a JSON string is NULL, so the predicate matches nothing, forever: both live
// mention tests would report "no Slack-born run appeared" and blame the operator's Request URL, their app
// subscription or their app_token_ref — none of which would be wrong.
//
// The reservation is the durable identity that did NOT move and cannot: SlackAdmitter.Admit reserves every
// Slack event under route '/v1/slack/events' with the key team_id + ':' + event_id, and that pair IS
// SLK-001/002's "one effect per source event". Grouping by it is therefore the same grouping the claim is
// about, rather than a proxy for it.
//
// The join to responses goes through the reservation's stored projection (response_body->>'id'), because
// idempotency_records carries no response FK — the projection is what the bridge wrote and what a replay
// hands back, so it names exactly the one response the reservation settled on.
const BornRunsByTeam = `
	SELECT split_part(i.idempotency_key, ':', 2), r.id
	  FROM responses r
	  JOIN idempotency_records i
	    ON i.organization_id = r.organization_id
	   AND i.project_id = r.project_id
	   AND i.response_body->>'id' = r.id
	 WHERE i.route = '/v1/slack/events'
	   AND i.idempotency_key LIKE $1 || ':%'
	   AND r.created_at > $2`
