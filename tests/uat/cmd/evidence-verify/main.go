// Command evidence-verify checks a local-live evidence bundle against the manifest
// contract (required receipts present, exactly-one terminal per case, checksums well
// formed, no leaked credential) and prints the operator summary. It backs
// `make evidence-verify RELEASE=<name>` and exits non-zero on any finding.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/palgroup/palai/tests/uat"
)

func main() {
	release := flag.String("release", "", "release name under evidence/releases/")
	flag.Parse()
	if *release == "" {
		fmt.Fprintln(os.Stderr, "usage: evidence-verify --release <name>")
		os.Exit(2)
	}
	dir := filepath.Join("evidence", "releases", *release)

	// When the live credential is in the environment (right after a live UAT), pass it as a
	// redaction needle so a leak fails verification even if it is not sk- shaped. The value
	// is never printed — only used as an opaque needle.
	var secrets []string
	if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		secrets = append(secrets, v)
	}

	summary, err := uat.VerifyRelease(dir, secrets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify %s: %v\n", *release, err)
		os.Exit(1)
	}
	fmt.Println(summary.String())
	for _, f := range summary.Findings {
		fmt.Fprintln(os.Stderr, "  - "+f.String())
	}
	if !summary.OK() {
		os.Exit(1)
	}
}
