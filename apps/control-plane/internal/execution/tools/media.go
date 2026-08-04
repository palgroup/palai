package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// SHOWING SOMETHING TO A HUMAN IS AN ACT, WHICH IS WHY IT IS A TOOL.
//
// A coding agent on a Mac can already take a screenshot and record a video: `xcrun simctl io <udid>
// screenshot out.png` and `… recordVideo out.mp4` are argv, and argv is what the shell tool runs. The
// bytes land in the workspace and stop there — nothing carries them to the person watching.
//
// AND NOTHING SHOULD, AUTOMATICALLY. A run that streamed every file it wrote would flood a chat with
// build intermediates. What a reader wants is the ONE frame the agent chose, at the moment it chose it,
// with a sentence about what changed — "look, this is how it turned out; is that right?" That decision
// belongs to the model, so it is a tool call: deliberate, journalled, and visible in the ledger as a
// thing the agent did rather than a side effect of something else.
//
// THE MEASUREMENT THAT MADE THIS THE ONLY SHAPE (2026-08-02, against the live control plane): the event
// stream's tool payload is `{run_id, replay_class, tool_call_id}` on executing and `{run_id,
// tool_call_id}` on completed — no name, no arguments, no result, no bytes. The durable tool_calls
// ledger holds the rest and no HTTP route reads it. So a browser cannot pull a screenshot out of a shell
// tool's stdout no matter how the shell call is written; the bytes need a store and an id. The artifact
// store is that store, `GET /v1/artifacts/{id}/content` is that route, and both already existed —
// what was missing was anything that PUT a screenshot in one.

const mediaToolName = "palai.workspace.show_media"

// mediaLogicalType is the artifact's logical kind, distinct from research bodies and changeset patches
// so a retention policy or an auditor can tell "the agent showed the user this" from "the agent
// downloaded this".
const mediaLogicalType = "shown_media"

// maxMediaBytes bounds one shown file at 24 MiB.
//
// IT IS LARGER THAN THE FILE TOOL'S 1 MiB ON PURPOSE and the difference is the point: that bound exists
// because a file read becomes MODEL TEXT, and a megabyte of text is already an expensive turn. These
// bytes never enter the model's context — the tool answers with an id, and the browser fetches the
// object. So the bound here is about what a chat can reasonably render and a store reasonably hold, not
// about a context window.
//
// A simulator screenshot measures in the low hundreds of KiB; a short screen recording is the case this
// number is actually sized for. A file past it is REFUSED with its size named rather than truncated —
// half a video is not a shorter video, it is a corrupt one.
const maxMediaBytes = 24 << 20

// mediaTypes maps the extensions a simulator actually produces. It is an ALLOW-LIST rather than a
// sniffer: this tool exists to show a screenshot or a recording, and a file whose extension is not one
// of these is far more likely to be a mistake than a media file in disguise. Guessing from content
// would turn "the agent pointed at the wrong path" into "the chat rendered a build log as an image".
var mediaTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".mp4":  "video/mp4",
	".mov":  "video/quicktime",
	".m4v":  "video/x-m4v",
}

// MediaTool lets the agent show the human a screenshot or a recording it produced.
func MediaTool() toolbroker.Tool {
	return toolbroker.Tool{
		Name: mediaToolName,
		Description: "Show the user an image or video from the workspace — a simulator screenshot or screen " +
			"recording. Give the workspace-relative path and a short caption saying what they are looking at. " +
			"Use this after a visible change so the user can see the result and tell you whether it is right.",
		// PURE, and the word is load-bearing rather than convenient: this reads a file and writes an
		// artifact, so re-running it changes nothing a later call can observe — the workspace is untouched
		// and the second artifact is a second copy of identical bytes. A recovery that replays it produces
		// the same answer, which is exactly what this class promises.
		ReplayClass: toolbroker.ClassPure,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "workspace-relative path to the image or video"},
				"caption": map[string]any{"type": "string", "description": "one line telling the user what they are looking at"},
			},
			"required":             []any{"path"},
			"additionalProperties": false,
		},
		OutputSchema: map[string]any{"type": "object"},
		Exec:         mediaExec,
	}
}

func mediaExec(ctx context.Context, env toolbroker.ExecEnv, args map[string]any) (map[string]any, error) {
	if env.WorkspaceRoot == "" {
		// The same answer the file tool gives, and for the same reason: a run offered a workspace tool with
		// no workspace bound used to HANG. Nothing ran, so it is an answer the model can act on.
		return nil, toolbroker.Answer("no_workspace", fmt.Errorf("%s: no workspace bound for this run", mediaToolName))
	}
	// NO STORE MEANS NO SHOWING, AND IT SAYS SO. Returning an id-less success would have the model tell
	// the user "here is the screenshot" with nothing behind it — the failure mode this whole tool exists
	// to remove, reintroduced one layer up.
	if env.Artifacts == nil {
		return nil, toolbroker.Answer("no_artifact_store", fmt.Errorf(
			"%s: this deployment has no artifact store, so there is nowhere to put the media and no way to show it", mediaToolName))
	}

	path, _ := args["path"].(string)
	if strings.TrimSpace(path) == "" {
		return nil, toolbroker.Answer("bad_path", fmt.Errorf("%s: path is required", mediaToolName))
	}
	caption, _ := args["caption"].(string)

	mediaType, ok := mediaTypes[strings.ToLower(filepath.Ext(path))]
	if !ok {
		return nil, toolbroker.Answer("unsupported_media", fmt.Errorf(
			"%s: %q is not a media file this can show (png, jpg, gif, webp, mp4, mov, m4v)", mediaToolName, path))
	}

	// THE PATH IS RESOLVED BY THE SAME CONFINEMENT THE FILE TOOL USES, not by a check written here.
	// Every path-equality and prefix comparison this repository has written by hand has shipped
	// defeated at least once; WorkspaceFS.resolve carries the traversal, absolute-path and
	// escaping-symlink refusals, and reusing it is the only version that stays correct when one of them
	// is tightened.
	fs, err := workspace.NewWorkspaceFS(env.WorkspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("%s: open workspace: %w", mediaToolName, err)
	}
	// maxMediaBytes+1 so an oversized file comes back TRUNCATED rather than silently at the limit — the
	// difference between "exactly the cap" and "past the cap" is what decides whether this refuses.
	data, truncated, err := fs.Read(path, maxMediaBytes+1)
	if err != nil {
		// A read that failed changed nothing, so it is an answer: the model named a path that is not
		// there, or not readable, or not inside the workspace, and it can try again.
		return nil, toolbroker.Answer("read_failed", fmt.Errorf("%s: %w", mediaToolName, err))
	}
	// BOTH CONDITIONS, AND THE SECOND IS THE ONE THAT WORKS. Measured 2026-08-02 by removing it: a file of
	// exactly maxMediaBytes+1 comes back from Read FULLY and UNTRUNCATED — `truncated` reports that there
	// was more than the limit asked for, and asking for cap+1 means a cap+1-byte file fits. So the length
	// check is not belt-and-braces on the flag; it is the check, and the flag covers the larger files.
	if truncated || int64(len(data)) > maxMediaBytes {
		return nil, toolbroker.Answer("too_large", fmt.Errorf(
			"%s: %q is larger than the %d MiB this can show — half a video is not a shorter video, so it is "+
				"refused rather than cut; record a shorter clip or downscale it", mediaToolName, path, maxMediaBytes>>20))
	}

	artifactID, err := env.Artifacts.WriteArtifact(ctx, env.Scope.Project, env.Scope.RunID,
		data, mediaType, mediaLogicalType,
		map[string]any{"tool": mediaToolName, "path": path, "caption": caption})
	if err != nil {
		return nil, fmt.Errorf("%s: persist media: %w", mediaToolName, err)
	}

	// THE ANSWER IS AN ID, NOT BYTES. The model gets a handle it can refer to and the browser fetches
	// the object; putting base64 here would spend the turn's context on pixels the model cannot see.
	return map[string]any{
		"artifact_id": artifactID,
		"media_type":  mediaType,
		"bytes":       len(data),
		"path":        path,
		"caption":     caption,
		"shown":       true,
	}, nil
}
