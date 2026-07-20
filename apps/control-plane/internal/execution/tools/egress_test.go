package tools

import "testing"

// TestClassifyEgressDeniesPrivateAndMetadata proves the SAN-004 decision logic: the cloud metadata
// address and every private/loopback/link-local range are denied with an audit finding, while a
// public destination is allowed with none. This is the security-load-bearing classifier the shell
// tool consults; a parse slip that reads 169.254.169.254 as public would leak instance credentials.
func TestClassifyEgressDeniesPrivateAndMetadata(t *testing.T) {
	denied := map[string]string{
		"169.254.169.254":    "metadata",
		"169.254.169.254:80": "metadata",
		"169.254.10.10":      "link_local",
		"10.0.0.5":           "private",
		"172.16.4.4":         "private",
		"192.168.1.1":        "private",
		"127.0.0.1":          "loopback",
		"::1":                "loopback",
		"fd00::1":            "private",
		"0.0.0.0":            "unspecified",
		"[fe80::1]:22":       "link_local",
	}
	for host, wantReason := range denied {
		allowed, finding := ClassifyEgress(host)
		if allowed || finding == nil {
			t.Fatalf("ClassifyEgress(%q) allowed=%v finding=%v, want denied with a finding", host, allowed, finding)
		}
		if finding.Reason != wantReason {
			t.Fatalf("ClassifyEgress(%q) reason = %q, want %q", host, finding.Reason, wantReason)
		}
	}

	allowed := []string{"93.184.216.34", "8.8.8.8", "example.com:443", "[2606:2800:220:1::1]:443"}
	for _, host := range allowed {
		ok, finding := ClassifyEgress(host)
		if !ok || finding != nil {
			t.Fatalf("ClassifyEgress(%q) allowed=%v finding=%v, want allowed with no finding", host, ok, finding)
		}
	}

	// An empty or unparseable host fails closed — denied, never silently allowed.
	if ok, finding := ClassifyEgress(""); ok || finding == nil {
		t.Fatalf("ClassifyEgress(\"\") allowed=%v, want denied (fail closed)", ok)
	}
}
