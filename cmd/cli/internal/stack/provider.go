package stack

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// AddProvider stores a provider credential into the .palai file-secret compose mounts.
// The value is read from stdin — never an argument — so it never lands in the process
// table, shell history, or the CLI argv the credential-hygiene proof scans. The file is
// 0600; the next `local up` carries it into the control-plane as a native Docker
// file-secret (Option B), so the raw value never rides a compose environment value.
func AddProvider(ref string) error {
	if strings.TrimSpace(ref) == "" {
		return errors.New("usage: palai provider add <ref>")
	}
	p, err := resolvePaths()
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 64*1024))
	if err != nil {
		return fmt.Errorf("read secret from stdin: %w", err)
	}
	value := strings.TrimRight(string(raw), "\r\n")
	if value == "" {
		return errors.New("no secret on stdin (pipe the value, e.g. `printf %s $KEY | palai provider add provider-one`)")
	}
	if err := os.MkdirAll(p.secretsDir, 0o700); err != nil {
		return fmt.Errorf("create secrets dir: %w", err)
	}
	if err := os.WriteFile(p.secretPath(ref), []byte(value), 0o600); err != nil {
		return fmt.Errorf("write secret: %w", err)
	}
	fmt.Fprintf(os.Stderr, "stored provider secret %q\n", ref)
	return nil
}
