package toolbroker

import "context"

// WorkspaceOps is the confined workspace surface a workspace-touching tool acts through, and it is
// the A.3 T5 sibling of ShellRunner: the same substitution, one layer over.
//
// WHY IT EXISTS AT ALL. Before it, five of the six coding tools reached the workspace by calling the
// operating system directly — workspace.NewWorkspaceFS(env.WorkspaceRoot) in the file and media
// tools, filepath.Join(env.WorkspaceRoot, "repo") plus git in commit/push/pull_request. Every one of
// those reads the CONTROL PLANE's filesystem. That was correct while the bytes lived there and became
// wrong the moment a lease could place a run on another machine: the shell tool would edit files on a
// Mac and the file tool would read a directory in a container, and neither would say so. Measured
// 2026-08-04 before this file existed:
//
//	grep -n "WorkspaceRoot" apps/control-plane/internal/execution/tools/{file,commit,push,media}.go
//	# -> five call sites, all of them a path in this process's own filesystem
//
// SO THE SEAM IS THE WHOLE POINT: a tool states WHAT it wants done to the workspace, and the thing
// that does it is chosen per attempt — the machine holding the lease when the allocation lives there,
// this host when it does not.
//
// IT TAKES A CONTEXT AND *WorkspaceFS DOES NOT, which is why there is an adapter rather than a
// direct satisfaction. That is not ceremony: a remote implementation blocks on a network answer, and
// an operation nobody can cancel is the shape this tree has already paid for once, when every tool
// error wedged its run forever.
type WorkspaceOps interface {
	// Read returns at most maxBytes of a workspace-relative path, reporting whether there was more.
	Read(ctx context.Context, rel string, maxBytes int64) (data []byte, truncated bool, err error)
	// Write replaces a workspace-relative path atomically and reports the before/after hashes the
	// changeset consumes.
	Write(ctx context.Context, rel string, data []byte) (WriteReport, error)
	// List returns the entries of a workspace-relative directory.
	List(ctx context.Context, rel string) ([]DirEntry, error)
	// Stat returns the metadata of a workspace-relative path.
	Stat(ctx context.Context, rel string) (FileStat, error)
	// Checksum returns the content digest of a workspace-relative regular file.
	Checksum(ctx context.Context, rel string) (string, error)
	// Glob returns workspace-relative paths of regular files matching a glob pattern, NEWEST
	// modification first, capped at limit (0 means uncapped). The bool reports whether the cap
	// dropped matches — a caller that cannot tell a complete answer from a clipped one concludes the
	// missing files do not exist. `**` crosses directories; a bare `*` does not.
	Glob(ctx context.Context, pattern string, limit int) ([]string, bool, error)
	// Grep searches file CONTENTS. The pattern is RE2 syntax, the same dialect ripgrep implements.
	Grep(ctx context.Context, req GrepRequest) (GrepResult, error)
	// Head reports the workspace repository's current commit and tree.
	Head(ctx context.Context) (commit, tree string, err error)
	// Commit records every tracked change in the workspace repository under the platform's fixed
	// author identity and returns the new commit. It needs no credential and grants no push.
	Commit(ctx context.Context, message string) (string, error)
}

// WriteReport is the before/after summary of one workspace write: the workspace-relative path, the
// content hash before and after (before is empty for a new file), and whether the file was created.
//
// It is declared here rather than imported from the filesystem package because BOTH ends of the wire
// need it and only one of them owns a filesystem. The fields are the same four
// adapters/sandboxes/oci/workspace.WriteReport carries; the adapters convert between them, which is
// one four-line function per direction and keeps the broker free of sandbox mechanics — the reason
// this package holds ShellCommand rather than importing the executor's.
type WriteReport struct {
	Path       string `json:"path"`
	BeforeHash string `json:"before_hash"`
	AfterHash  string `json:"after_hash"`
	Created    bool   `json:"created"`
}

// FileStat is the confined metadata of a workspace path. It is named FileStat rather than Stat
// because this package is dot-free at its call sites (`toolbroker.Stat` would read as a verb).
type FileStat struct {
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

// DirEntry is one entry of a confined directory listing.
// GrepRequest is one content search. It mirrors adapters/sandboxes/oci/workspace.GrepQuery, which the
// adapters convert to — the same split the DirEntry and WriteReport pairs below already carry, so the
// broker's surface does not depend on a sandbox package.
type GrepRequest struct {
	Pattern    string // RE2 regex syntax: `interface{}` must be written `interface\{\}`
	Path       string // workspace-relative subtree; "" searches everything
	Glob       string // filename filter, e.g. `**/*.go`
	OutputMode string // "content" | "files_with_matches" | "count"; empty means files_with_matches
	Before     int    // leading context lines, content mode only
	After      int    // trailing context lines, content mode only
	Multiline  bool   // let `.` cross line boundaries
	Limit      int    // maximum entries LISTED; Total still counts every match
}

// GrepMatch is one matching line with any context the request asked for.
type GrepMatch struct {
	Path   string
	Line   int
	Text   string
	Before []string
	After  []string
}

// GrepResult carries whichever shape the mode asked for. Total is every match found, including
// matches in entries the limit did not list — a total that summed only the listed rows would read as
// authoritative while under-reporting.
type GrepResult struct {
	Mode      string
	Matches   []GrepMatch
	Files     []string
	Counts    map[string]int
	Total     int
	Truncated bool
}

type DirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}
