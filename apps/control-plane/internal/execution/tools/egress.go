// Package tools defines the built-in model-facing tool surface (file, shell) that runs behind the
// sandbox-backed execution seam (spec §28.7-28.8). Each tool resolves and confines every effect to
// the workspace, bounds and redacts its output, and records the audit findings the changeset and
// approval layers consume.
package tools

import (
	"net"
	"strings"
)

// metadataIP is the cloud instance-metadata address. It is link-local, but denials name it
// "metadata" specifically because reaching it is the canonical credential-exfiltration path
// (spec §28.8, SAN-004).
var metadataIP = net.ParseIP("169.254.169.254")

// metadataHostnames are the well-known metadata endpoints reached by name rather than by the raw
// address. ponytail: a small static set — the address form (any link-local) is the real guard; DNS
// rebinding to a private A record is a documented ceiling until egress runs through a resolving proxy.
var metadataHostnames = map[string]bool{
	"metadata.google.internal": true,
	"metadata":                 true,
}

// EgressFinding records a denied egress target for the audit and changeset trail (spec §28.8).
type EgressFinding struct {
	Host   string
	Reason string
}

// ClassifyEgress decides whether a shell egress target is allowed and, when denied, returns the
// audit finding (spec §28.8, SAN-004). The cloud metadata address, loopback, link-local, and every
// private range (RFC1918 + ULA) are denied; a public destination is allowed with no finding. It
// fails closed: an empty or unresolvable host is denied. A bare hostname that is not a known
// metadata name is treated as public — the sandbox's own no-network enforcement is the backstop for
// a name that resolves into a private range.
func ClassifyEgress(target string) (bool, *EgressFinding) {
	host := target
	if h, _, err := net.SplitHostPort(target); err == nil {
		host = h
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false, &EgressFinding{Host: target, Reason: "unresolved"}
	}

	ip := net.ParseIP(host)
	if ip == nil {
		if metadataHostnames[strings.ToLower(host)] {
			return false, &EgressFinding{Host: host, Reason: "metadata"}
		}
		return true, nil
	}

	switch {
	case ip.Equal(metadataIP):
		return false, &EgressFinding{Host: host, Reason: "metadata"}
	case ip.IsLoopback():
		return false, &EgressFinding{Host: host, Reason: "loopback"}
	case ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast():
		return false, &EgressFinding{Host: host, Reason: "link_local"}
	case ip.IsPrivate():
		return false, &EgressFinding{Host: host, Reason: "private"}
	case ip.IsUnspecified():
		return false, &EgressFinding{Host: host, Reason: "unspecified"}
	}
	return true, nil
}
