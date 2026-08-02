package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// recordingArtifacts stands in for the object store. It records what was written, because the whole
// value of this tool is WHAT reached the store — a test that only checked the tool returned an id would
// pass against a tool that stored the wrong bytes under the right name.
type recordingArtifacts struct {
	content   []byte
	mediaType string
	logical   string
	prov      map[string]any
	err       error
}

func (r *recordingArtifacts) WriteArtifact(_ context.Context, _, _, _ string, content []byte, mediaType, logicalType string, provenance map[string]any) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	r.content, r.mediaType, r.logical, r.prov = content, mediaType, logicalType, provenance
	return "art_media_1", nil
}

func mediaEnv(t *testing.T, store toolbroker.ArtifactWriter) (toolbroker.ExecEnv, string) {
	t.Helper()
	root := t.TempDir()
	return toolbroker.ExecEnv{WorkspaceRoot: root, Artifacts: store}, root
}

func TestMediaToolShowsWhatTheAgentChose(t *testing.T) {
	ctx := context.Background()

	t.Run("a screenshot reaches the store with its bytes and its caption", func(t *testing.T) {
		store := &recordingArtifacts{}
		env, root := mediaEnv(t, store)
		png := []byte("\x89PNG\r\n\x1a\nnot-really-but-bytes-are-bytes")
		if err := os.WriteFile(filepath.Join(root, "shot.png"), png, 0o600); err != nil {
			t.Fatal(err)
		}

		out, err := mediaExec(ctx, env, map[string]any{"path": "shot.png", "caption": "the new empty state"})
		if err != nil {
			t.Fatalf("show_media: %v", err)
		}
		if out["artifact_id"] != "art_media_1" || out["media_type"] != "image/png" {
			t.Fatalf("answer = %v, want the artifact id and image/png", out)
		}
		// THE BYTES, NOT JUST THE ID. A tool that answered with an id while storing something else would
		// pass every assertion above and show the user the wrong picture.
		if string(store.content) != string(png) {
			t.Fatalf("stored %d bytes, want the file's %d — the id is right and the picture is not",
				len(store.content), len(png))
		}
		if store.logical != mediaLogicalType {
			t.Errorf("logical type = %q, want %q so an auditor can tell 'shown to the user' from 'downloaded'",
				store.logical, mediaLogicalType)
		}
		// The caption travels with the object rather than only in the model's text: the chat renders it
		// beside the image, and an artifact found later still says what it was.
		if store.prov["caption"] != "the new empty state" {
			t.Errorf("provenance = %v, want the caption carried with the object", store.prov)
		}
	})

	t.Run("a video is the same path", func(t *testing.T) {
		store := &recordingArtifacts{}
		env, root := mediaEnv(t, store)
		if err := os.WriteFile(filepath.Join(root, "run.mp4"), []byte("moov"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := mediaExec(ctx, env, map[string]any{"path": "run.mp4"}); err != nil {
			t.Fatalf("show_media(video): %v", err)
		}
		if store.mediaType != "video/mp4" {
			t.Fatalf("media type = %q, want video/mp4 — a chat that gets image/* for a video renders a broken image",
				store.mediaType)
		}
	})

	// EVERY REFUSAL BELOW IS AN ANSWER, NOT A FAILURE. A tool error ends the attempt; an answer goes back
	// to the model, which can name a different path and carry on. This tree already paid for the
	// difference once — `read "README"` instead of `read "repo/README"` used to end a run permanently.
	for _, tc := range []struct {
		name string
		args map[string]any
		why  string
	}{
		{"a path outside the workspace", map[string]any{"path": "../escape.png"}, "traversal is refused by the shared confinement, not by a check written here"},
		{"an absolute path", map[string]any{"path": "/etc/hosts.png"}, "an absolute path is not workspace-relative"},
		{"a file that is not media", map[string]any{"path": "build.log"}, "showing a log as an image renders a broken picture instead of saying the path was wrong"},
		{"a missing file", map[string]any{"path": "nope.png"}, "the model named a path that is not there and can try another"},
		{"no path", map[string]any{}, "a show with no subject"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &recordingArtifacts{}
			env, _ := mediaEnv(t, store)
			_, err := mediaExec(ctx, env, tc.args)
			if err == nil {
				t.Fatalf("accepted %v — %s", tc.args, tc.why)
			}
			if !errors.Is(err, toolbroker.ErrToolAnswer) {
				t.Fatalf("refused %v with a FAILURE rather than an answer (%v). A failure ends the attempt; "+
					"the model can recover from this one by naming a different path", tc.args, err)
			}
			if store.content != nil {
				t.Errorf("a refused call still wrote %d bytes to the store", len(store.content))
			}
		})
	}

	// HALF A VIDEO IS NOT A SHORTER VIDEO. The file tool truncates because its bytes become model text and
	// a prefix is still readable; these bytes become a file the browser decodes, and a truncated one is
	// corrupt. So the bound refuses and names the size rather than storing a broken object.
	t.Run("an oversized file is refused rather than truncated", func(t *testing.T) {
		store := &recordingArtifacts{}
		env, root := mediaEnv(t, store)
		if err := os.WriteFile(filepath.Join(root, "big.mp4"), make([]byte, maxMediaBytes+1), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := mediaExec(ctx, env, map[string]any{"path": "big.mp4"})
		if err == nil {
			t.Fatal("an oversized file was accepted; a truncated video is a corrupt one")
		}
		if !strings.Contains(err.Error(), "larger than") {
			t.Errorf("the refusal does not say it was a size problem: %v", err)
		}
		if store.content != nil {
			t.Errorf("a refused oversized call still stored %d bytes", len(store.content))
		}
	})

	// A DEPLOYMENT WITH NO STORE MUST NOT SUCCEED QUIETLY. Answering with no id would have the model tell
	// the user "here is the screenshot" with nothing behind it — the exact failure this tool removes,
	// reintroduced one layer up.
	t.Run("no artifact store is refused, not silently skipped", func(t *testing.T) {
		env, root := mediaEnv(t, nil)
		if err := os.WriteFile(filepath.Join(root, "shot.png"), []byte("png"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := mediaExec(ctx, env, map[string]any{"path": "shot.png"})
		if err == nil {
			t.Fatal("succeeded with no store — the model would claim to have shown something that went nowhere")
		}
	})
}
