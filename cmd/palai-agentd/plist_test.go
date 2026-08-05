package main

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// plistPath is the deployment descriptor Task 2 will install. It is asserted against the daemon's own
// declarations here because the two drift silently otherwise: a plist naming a flag this binary does
// not have, or a socket path it does not default to, fails on a machine at install time — which is the
// worst place to find out, and the one place nobody in this repository can run.
const plistPath = "../../deploy/macos/net.pallasite.palai-agentd.plist"

func readPlist(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("the plist this daemon ships with is missing: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("the plist is empty, so everything below would be vacuously true")
	}
	return string(b)
}

// TestThePlistIsWellFormedXML catches the failure a comment cannot: an unescaped character or an
// unclosed tag makes launchd refuse the job with a message about the file, not about the daemon.
func TestThePlistIsWellFormedXML(t *testing.T) {
	dec := xml.NewDecoder(strings.NewReader(readPlist(t)))
	for {
		_, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				return
			}
			t.Fatalf("the plist is not well-formed XML: %v", err)
		}
	}
}

// TestThePlistAgreesWithTheDaemonItStarts pins every value that has to match something in this package.
func TestThePlistAgreesWithTheDaemonItStarts(t *testing.T) {
	body := readPlist(t)

	for _, want := range []string{
		"<string>" + defaultSocketPath + "</string>",
		"<string>" + defaultGroup + "</string>",
		"<string>-socket</string>",
		"<string>-group</string>",
		// Root, because creating an account needs it. A plist that quietly ran this as anybody else
		// would produce a daemon that starts and then fails every verb.
		"<key>UserName</key>",
		"<string>root</string>",
		// Restarted rather than left down: a machine whose daemon died cannot isolate a session.
		"<key>KeepAlive</key>",
		"<key>RunAtLoad</key>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the plist does not contain %s", want)
		}
	}

	// The flags it passes must be flags this binary declares. A plist naming `--socket` or `-sock`
	// would make launchd start a daemon that exits on flag.Parse.
	for _, flagName := range []string{"-socket", "-group"} {
		if !strings.Contains(body, "<string>"+flagName+"</string>") {
			continue
		}
		if !flagIsDeclared(t, strings.TrimPrefix(flagName, "-")) {
			t.Errorf("the plist passes %s but main.go declares no such flag", flagName)
		}
	}

	// SOCKET ACTIVATION IS NOT CLAIMED. This tree has no cgo and launch_activate_socket has no
	// non-cgo wrapper, so a Sockets dict here would be a promise the daemon cannot keep: launchd would
	// hold the listener and the daemon would bind a second socket at the same path, or fail to. The
	// ceiling is named in the plist's comment; this asserts the file does not quietly contradict it.
	if strings.Contains(body, "<key>Sockets</key>") {
		t.Error("the plist declares Sockets, but this daemon binds its own socket and never calls launch_activate_socket")
	}
}

// flagIsDeclared looks for a flag definition in the daemon's own source.
func flagIsDeclared(t *testing.T, name string) bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(".", "main.go"))
	if err != nil {
		t.Fatalf("reading main.go: %v", err)
	}
	return strings.Contains(string(b), `"`+name+`"`)
}
