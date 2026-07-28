package compose

import "embed"

// Files are the deploy files a `palai` binary carries so it can drive compose with NO source tree
// behind it. Until they were embedded, every compose-driving command resolved
// deploy/compose/compose.yaml relative to the caller's cwd, which meant Palai could only be run
// from inside a clone of this repo.
//
// The set is exactly what a bring-up and `palai config validate` READ at run time:
// compose.yaml and production.yml are the two files compose is pointed at; production-entrypoint.sh
// is read (not executed) by config-validate, which extracts the guard's dev-default literals from it
// so the two can never fork; the two .env.example files are what an operator copies to write their
// own env file; compose.observability.yml is the optional third overlay.
//
// The Dockerfiles and the bridge entrypoints are deliberately NOT here. They are BUILD inputs, and a
// packaged binary has no build context to hand them (compose.yaml's `build.context: ../..` is the
// repo root) — so the packaged path never builds, it requires images that already exist. Embedding
// them would advertise a capability that cannot work.
//
// The CLI materialises these under ${PALAI_HOME}/compose (stack.ensureComposeFiles); a checkout
// still uses its own committed copies, so these bytes and those never disagree about which file an
// operator is debugging.
//
//go:embed compose.yaml production.yml production-entrypoint.sh compose.env.example production.env.example compose.observability.yml
var Files embed.FS
