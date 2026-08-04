// Package toolset is the CANONICAL list of tools a palai bring-up grants.
//
// IT LIVES UNDER packages/ FOR A REASON THAT IS NOT STYLE: Go's internal rule makes
// apps/control-plane/internal/... unreachable from cmd/cli, and three binaries need this list — the
// CLI that writes the project baseline, the control plane's guard that proves every name resolves,
// and the Slack relay that renders a human title per name. A list copied into any of those three is a
// list that will drift; commit 1e5fc63e is the proof, having deleted the CLI's copy and left the
// guard hunting for it.
//
// THE NAMES ARE GENERIC, NOT SLACK'S. A bring-up binds a platform surface — a workspace, a shell, a
// publication path — and Slack is one consumer of that surface, not its shape.
package toolset

// defaultTools is what every bring-up binds, with or without a repository: read and write files, run
// a command, stop what that command started, show a human what happened, and reach for outside
// knowledge. Each name is registered in toolbroker.New (apps/control-plane/cmd/palai-control-plane/
// main.go:656-672) — a name that is not is a tool the model is offered and can never be given.
var defaultTools = []string{
	"palai.workspace.file",
	"palai.workspace.shell",
	"palai.workspace.background_kill",
	"palai.workspace.show_media",
	"palai.research.fetch",
	"palai.knowledge.retrieve",
}

// repositoryTools is the coding half of the canonical surface: committing without a repository is not
// a weaker capability, it is a meaningless one. NO GRANT PATH EXISTS TODAY (dated 2026-08-04) — `palai
// up` writes only Default() (cmd/cli/internal/stack/up.go's bootstrapDefaultTools), and nothing in this
// tree calls Repository(). The list still earns its place: it is what a future conditional grant
// (bound-a-repository) will reach for, and it is what the guards below bind names ↔ broker
// registrations ↔ Slack titles against ahead of that wiring.
var repositoryTools = []string{
	"palai.workspace.commit",
}

// publishTools is the publication path: the three operations that move work OUT of the workspace.
// They are their own list because they are the ones gated by a human decision (E23 T6).
var publishTools = []string{
	"palai.publish.push",
	"palai.publish.pull_request",
	"palai.publish.merge_pull_request",
}

// Default returns the tools every bring-up binds.
func Default() []string { return clone(defaultTools) }

// Repository returns the tools a bring-up adds when it bound a repository.
func Repository() []string { return clone(repositoryTools) }

// Publish returns the publication tools.
func Publish() []string { return clone(publishTools) }

// All returns every canonical name, in list order: Default, then Repository, then Publish. It is what
// a guard ranges over when it must prove something about the whole surface.
func All() []string {
	out := make([]string, 0, len(defaultTools)+len(repositoryTools)+len(publishTools))
	out = append(out, defaultTools...)
	out = append(out, repositoryTools...)
	return append(out, publishTools...)
}

// clone hands back a copy. Returning the package's own slice would let one caller's append or
// assignment rewrite the canonical list for every later caller in the process.
func clone(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}
