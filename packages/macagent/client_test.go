package macagent

import "testing"

// TestTheAccountVERBSGetTheirOwnBound — A DAEMON DOING WHAT IT WAS ASKED IS NOT A WEDGED DAEMON.
//
// `sysadminctl -addUser` writes a user record and a home directory; on a busy Mac that is seconds. The
// five-second exchange bound was chosen to catch a daemon that ACCEPTS and then says nothing, and it
// cannot tell that apart from one creating an account — so a session's first workspace failed with
// `create slot 01: … i/o timeout` while the account was being created successfully underneath.
//
// The two bounds stay separate rather than the short one being raised: version and list answer from
// memory, and a slow answer from either IS the wedge the short bound exists to catch.
func TestTheAccountVERBSGetTheirOwnBound(t *testing.T) {
	for _, v := range []Verb{VerbCreate, VerbDelete} {
		if got := exchangeTimeoutFor(v); got != AccountExchangeTimeout {
			t.Errorf("verb %v gets %s, want the account bound %s — it does real work in the OS", v, got, AccountExchangeTimeout)
		}
	}
	for _, v := range []Verb{VerbVersion, VerbList, VerbSpawn} {
		if got := exchangeTimeoutFor(v); got != ExchangeTimeout {
			t.Errorf("verb %v gets %s, want the short bound %s — it answers from memory, so a slow reply "+
				"is the wedge that bound exists to catch", v, got, ExchangeTimeout)
		}
	}
	if AccountExchangeTimeout <= ExchangeTimeout {
		t.Fatal("the account bound must be longer than the memory bound, or separating them bought nothing")
	}
}
