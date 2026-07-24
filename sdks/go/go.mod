// The Palai Go SDK is its OWN module (E16 T4): it never imports the monorepo's
// internal packages, so the public SDK stays independently movable. It is stdlib-only
// — net/http + bufio SSE, encoding/json forward-compat — with NO third-party or
// provider dependency (plan §2 retry-owner + dependency-ceiling invariants).
module github.com/palgroup/palai/sdks/go

go 1.26.0
