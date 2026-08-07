package macagent

import (
	"os/user"
	"testing"
)

// TestTheGroupAuTHORISESTheHumanAndNotTheRootRunningTheInstall — GROUP MEMBERSHIP IS THE ENTIRE
// CREDENTIAL, SO AUTHORISING THE WRONG ACCOUNT AUTHORISES NOBODY.
//
// The install runs as root. The control plane on a native Mac runs as the human. `user.Current()` under
// sudo answers root, so the daemon came up with root in its group and told the control plane
// "connect: permission denied" — while the machine advertised `accounts` isolation it could not
// deliver. Measured on a real Mac on 2026-08-07.
//
// This is the third place in one enrolment where "who am I" and "who is this for" were the same
// question with two answers, after the LaunchAgent's GUI domain and its bootstrap namespace.
func TestTheGroupAuTHORISESTheHumanAndNotTheRootRunningTheInstall(t *testing.T) {
	asRoot := func() (*user.User, error) { return &user.User{Username: "root"}, nil }
	for _, tc := range []struct {
		name, sudoUser, want string
	}{
		{"under sudo the human is authorised", "salih", "salih"},
		{"no sudo means the caller is the human", "", "root"},
		{"SUDO_USER=root is not a human either", "root", "root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := authorisedClient(func(string) string { return tc.sudoUser }, asRoot)
			if got != tc.want {
				t.Fatalf("authorisedClient(SUDO_USER=%q) = %q, want %q", tc.sudoUser, got, tc.want)
			}
		})
	}
}
