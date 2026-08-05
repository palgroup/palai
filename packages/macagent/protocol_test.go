package macagent_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/palgroup/palai/packages/macagent"
)

func classOf(t *testing.T, err error) macagent.Class {
	t.Helper()
	var e *macagent.Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v carries no class, so a caller has nothing to branch on", err)
	}
	return e.Class
}

// TestARequestIsRefusedOnShapeNotOnArithmetic is the protocol half of the slot rule.
//
// It refuses on SHAPE for the same reason validate_index in scripts/ops/palai-session-account does:
// `1`, `+1`, ` 1`, `1e1` and `0x11` are all things some parser will happily turn into a number, none of
// them is what this tooling formats, and a value that took a different path to the same integer took a
// path nobody tested.
func TestARequestIsRefusedOnShapeNotOnArithmetic(t *testing.T) {
	refused := []struct {
		line  string
		class macagent.Class
	}{
		{"create 7", macagent.ClassBadSlot},
		{"create 007", macagent.ClassBadSlot},
		{"create +7", macagent.ClassBadSlot},
		{"create  7", macagent.ClassBadRequest},
		{"create 1e1", macagent.ClassBadSlot},
		{"create 0x11", macagent.ClassBadSlot},
		{"create 00", macagent.ClassBadSlot},
		{"create -1", macagent.ClassBadSlot},
		{"create 100", macagent.ClassBadSlot},
		{"delete salih", macagent.ClassBadSlot},
		{"delete /Users/salih", macagent.ClassBadSlot},
		{"create 07; rm -rf /", macagent.ClassBadRequest},
		{"create 07 && id", macagent.ClassBadRequest},
		// A tab is not the separator, and it is also not printable ASCII — so it is refused as a
		// malformed REQUEST before anything looks at a slot. That is the stronger answer of the two:
		// the byte never gets as far as being interpreted.
		{"create\t07", macagent.ClassBadRequest},
		{"list 07", macagent.ClassBadRequest},
		// An empty line is an empty token, not an unknown verb. The distinction matters to a caller:
		// bad_request means "you sent nothing I can frame", unknown_verb means "I framed it and the
		// verb is not mine".
		{"", macagent.ClassBadRequest},
		{"CREATE 07", macagent.ClassUnknownVerb},
		{"sudo 07", macagent.ClassUnknownVerb},
		{"create " + strings.Repeat("0", 300), macagent.ClassBadRequest},
		{"create 0\x007", macagent.ClassBadRequest},
	}
	for _, tc := range refused {
		req, err := macagent.ParseRequest(tc.line)
		if err == nil {
			t.Errorf("%q parsed as %+v", tc.line, req)
			continue
		}
		if got := classOf(t, err); got != tc.class {
			t.Errorf("%q refused with class %q, want %q", tc.line, got, tc.class)
		}
	}

	// THE CONTROL. Refusing everything would make every line above meaningless.
	accepted := map[string]macagent.Request{
		"create 01":   {Verb: macagent.VerbCreate, Slot: 1},
		"create 07\n": {Verb: macagent.VerbCreate, Slot: 7},
		"delete 99":   {Verb: macagent.VerbDelete, Slot: 99},
		"list":        {Verb: macagent.VerbList},
		"list\n":      {Verb: macagent.VerbList},
	}
	for line, want := range accepted {
		got, err := macagent.ParseRequest(line)
		if err != nil {
			t.Errorf("%q was refused: %v", line, err)
			continue
		}
		if got != want {
			t.Errorf("%q parsed as %+v, want %+v", line, got, want)
		}
	}
}

// TestAccountNamesComeOnlyFromSlots pins the derivation. This is the function that makes the whole
// design work: there is one producer of account names in this system and it takes an integer.
func TestAccountNamesComeOnlyFromSlots(t *testing.T) {
	for slot, want := range map[int]string{1: "palai-s01", 7: "palai-s07", 42: "palai-s42", 99: "palai-s99"} {
		got, err := macagent.AccountName(slot)
		if err != nil || got != want {
			t.Errorf("AccountName(%d) = %q, %v; want %q", slot, got, err, want)
		}
		uid, err := macagent.AccountUID(slot)
		if err != nil || uid != macagent.UIDBase+slot {
			t.Errorf("AccountUID(%d) = %d, %v; want %d", slot, uid, err, macagent.UIDBase+slot)
		}
		home, err := macagent.HomeDir(slot)
		if err != nil || home != "/Users/"+want {
			t.Errorf("HomeDir(%d) = %q, %v; want /Users/%s", slot, home, err, want)
		}
	}
	for _, slot := range []int{0, -1, 100, 1 << 20} {
		if name, err := macagent.AccountName(slot); err == nil {
			t.Errorf("AccountName(%d) produced %q", slot, name)
		}
		if uid, err := macagent.AccountUID(slot); err == nil {
			t.Errorf("AccountUID(%d) produced %d", slot, uid)
		}
		if home, err := macagent.HomeDir(slot); err == nil {
			t.Errorf("HomeDir(%d) produced %q", slot, home)
		}
	}
}

// TestIsAccountNameRefusesEveryNameOutsideTheNamespace covers the second defence: the daemon reads
// names back out of directory services when it lists and when it deletes, and a record it did not
// create must not be acted on merely because it turned up in an enumeration.
func TestIsAccountNameRefusesEveryNameOutsideTheNamespace(t *testing.T) {
	for _, name := range []string{
		"salih", "root", "", "palai-s", "palai-s0", "palai-s1", "palai-s001", "palai-s100",
		"palai-s00", "palai-sxx", "palai-s07 ", " palai-s07", "Palai-s07", "palai-s07x",
		"../palai-s07", "palai-s-1",
	} {
		if slot, ok := macagent.IsAccountName(name); ok {
			t.Errorf("IsAccountName(%q) accepted it as slot %d", name, slot)
		}
	}
	for name, want := range map[string]int{"palai-s01": 1, "palai-s07": 7, "palai-s99": 99} {
		if slot, ok := macagent.IsAccountName(name); !ok || slot != want {
			t.Errorf("IsAccountName(%q) = %d, %v; want %d, true", name, slot, ok, want)
		}
	}
}

// TestSlotFromUIDGuardsTheCacheSweep: this is what stops removeDarwinBuckets being pointed at anybody
// else's caches, so its edges are worth pinning rather than inferring.
func TestSlotFromUIDGuardsTheCacheSweep(t *testing.T) {
	for _, uid := range []int{0, 1, 501, 504, macagent.UIDBase, macagent.UIDBase + macagent.MaxSlot + 1, -1} {
		if slot, ok := macagent.SlotFromUID(uid); ok {
			t.Errorf("SlotFromUID(%d) accepted it as slot %d", uid, slot)
		}
	}
	if slot, ok := macagent.SlotFromUID(701); !ok || slot != 1 {
		t.Errorf("SlotFromUID(701) = %d, %v; want 1, true", slot, ok)
	}
	if slot, ok := macagent.SlotFromUID(799); !ok || slot != 99 {
		t.Errorf("SlotFromUID(799) = %d, %v; want 99, true", slot, ok)
	}
}

// TestTheWireRoundTripsBothWays pins the two halves against each other, because a daemon and a caller
// that disagree about the format fail at the least convenient moment.
func TestTheWireRoundTripsBothWays(t *testing.T) {
	for _, req := range []macagent.Request{
		{Verb: macagent.VerbCreate, Slot: 1},
		{Verb: macagent.VerbDelete, Slot: 99},
		{Verb: macagent.VerbList},
	} {
		line, err := req.Line()
		if err != nil {
			t.Fatalf("%+v does not encode: %v", req, err)
		}
		if !strings.HasSuffix(line, "\n") {
			t.Errorf("%q has no terminator, so a reader cannot frame it", line)
		}
		back, err := macagent.ParseRequest(line)
		if err != nil || back != req {
			t.Errorf("%+v encoded to %q and came back as %+v (%v)", req, line, back, err)
		}
	}

	// A struct built by hand cannot put a malformed request on the wire either.
	for _, req := range []macagent.Request{
		{Verb: macagent.VerbCreate, Slot: 0},
		{Verb: macagent.VerbDelete, Slot: 100},
		{Verb: macagent.VerbUnknown, Slot: 7},
		{Verb: macagent.Verb(200), Slot: 7},
	} {
		if line, err := req.Line(); err == nil {
			t.Errorf("%+v encoded to %q", req, line)
		}
	}

	responses := []macagent.Response{
		macagent.OKAccount(macagent.VerbCreate, "palai-s07", "/Users/palai-s07"),
		macagent.OKAccount(macagent.VerbDelete, "palai-s01", "/Users/palai-s01"),
		macagent.OKList([]int{1, 7, 99}),
		macagent.OKList(nil),
		macagent.Err(macagent.ClassRefused, "the marker was minted on another host"),
	}
	for _, want := range responses {
		line := want.Line()
		if strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
			t.Errorf("%+v encoded to %q, which is not exactly one line", want, line)
		}
		got, err := macagent.ParseResponse(line)
		if err != nil {
			t.Errorf("%q did not parse: %v", line, err)
			continue
		}
		if got.OK != want.OK || got.Name != want.Name || got.Home != want.Home ||
			got.Class != want.Class || got.Message != want.Message || len(got.Slots) != len(want.Slots) {
			t.Errorf("%q came back as %+v, want %+v", line, got, want)
		}
	}
}

// TestAMultiLineMessageStaysOneLine: a newline inside a message would turn one response into two and
// desynchronise a caller reading line by line — which is how a caller ends up matching the NEXT
// request's reply against this one's request.
func TestAMultiLineMessageStaysOneLine(t *testing.T) {
	line := macagent.Err(macagent.ClassInternal, "first\nsecond\r\nthird").Line()
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("%q spans more than one line", line)
	}
	got, err := macagent.ParseResponse(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Class != macagent.ClassInternal {
		t.Errorf("class came back as %q", got.Class)
	}
	if strings.Contains(got.Message, "\n") {
		t.Errorf("message %q still carries a newline", got.Message)
	}
}
