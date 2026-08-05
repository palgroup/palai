// Package macos carries the macOS deployment descriptors a `palai` binary needs on a machine that has
// no source tree behind it.
//
// IT IS EMBEDDED FOR THE REASON deploy/compose IS. The zero-touch path is a cloud Mac running a
// first-boot hook with a downloaded binary and nothing else; a `palai up --native` that resolved this
// plist relative to the caller's cwd would work only inside a clone of this repository, which is the
// one place the hundred machines are not.
//
// THIS FILE IS THE ONE WRITING OF THE JOB DESCRIPTION. The installer does not render a plist of its own
// — a second producer of the same XML is two things that drift, and the drift lands as a launchd job
// naming a binary the installer put somewhere else. cmd/palai-agentd/plist_test.go asserts these bytes
// agree with the daemon's own flags and paths.
package macos

import "embed"

//go:embed net.pallasite.palai-agentd.plist
var Files embed.FS

// LaunchDaemonPlist is the job description the installer writes to
// /Library/LaunchDaemons/net.pallasite.palai-agentd.plist.
func LaunchDaemonPlist() ([]byte, error) {
	return Files.ReadFile("net.pallasite.palai-agentd.plist")
}
