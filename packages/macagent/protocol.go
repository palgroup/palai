// Package macagent is the wire protocol between a control plane and palai-agentd, the root daemon that
// owns session accounts on a Mac.
//
// THE POINT OF THIS PACKAGE IS WHAT IT CANNOT SAY.
//
// palai-agentd runs as root and is reachable from a process the tenant's model can influence. If a
// caller supplied the account name, a compromised control plane could say `delete salih`. So the caller
// does not supply one: a request carries a SLOT, the slot is an integer, and the daemon derives
// `palai-sNN` from it. The boundary is not a check that could be forgotten — it is the shape of
// [Request], which has no string-kinded field anywhere in it. `delete salih` is not refused; it is
// unwriteable.
//
// TestTheProtocolCannotExpressAnAccountNameAtAll in cmd/palai-agentd reads that shape back out of the
// AST, because an absence has no behaviour to observe and the only way to regress it is to change a
// declaration.
package macagent

import (
	"fmt"
	"strings"
)

// MaxSlot is the highest session index, mirroring MAX_SESSIONS in scripts/ops/mac-sessions.sh. The two
// must agree: a slot this daemon creates is one that tooling has to be able to name and delete.
const MaxSlot = 99

// UIDBase is the uid this namespace allocates from, mirroring UID_BASE in mac-sessions.sh. Slot N gets
// uid UIDBase+N, and that arithmetic is load-bearing rather than cosmetic — the deletion guard refuses
// any record whose uid is not the one this formula would have produced, which is how a renamed or
// hand-edited account fails to look like ours.
const UIDBase = 700

// namePrefix is the account-name prefix this daemon owns. Unlike mac-sessions.sh's PREFIX it is NOT
// overridable. That script needed a variable so its own test suite could scan a namespace no real
// machine uses; a root daemon has no such need, and an environment-settable prefix on a privileged
// process is a way to aim it at a namespace somebody else owns.
const namePrefix = "palai-s"

// MaxRequestBytes caps one request line. A root daemon that reads an unbounded line from a socket lets
// anyone in the `palai` group spend its memory, and no legitimate request is longer than `delete 07`.
const MaxRequestBytes = 256

// Verb is the operation a request asks for.
//
// IT IS AN INTEGER, NOT A STRING, and that is deliberate: see the package comment. Together with Slot
// it makes [Request] a struct with no string-kinded field, so there is nowhere in a request to put an
// account name, a path, or a shell fragment.
type Verb uint8

// The verb set is closed and it is small. Every entry is one thing the daemon can be asked to do; there
// is no pass-through, no flag, and no escape hatch, because the sudoers lesson in
// scripts/ops/palai-session-account applies unchanged here — the shape of the privilege IS the
// privilege.
const (
	VerbUnknown Verb = iota
	VerbCreate
	VerbDelete
	VerbList
)

// Word is the token this verb is spelled with on the wire. An unknown verb has no word, and that is
// what makes a malformed request unencodable as well as unparseable.
func (v Verb) Word() string {
	switch v {
	case VerbCreate:
		return "create"
	case VerbDelete:
		return "delete"
	case VerbList:
		return "list"
	default:
		return ""
	}
}

func (v Verb) String() string {
	if w := v.Word(); w != "" {
		return w
	}
	return "unknown"
}

// Request is everything a caller can ask palai-agentd for.
//
// ‼️ NEITHER FIELD IS A STRING, AND THAT IS THE SECURITY PROPERTY. Adding a string field to this struct
// — a name, a path, a "reason" — reopens exactly the hole the type closes, which is why a test asserts
// the declaration rather than trusting this comment.
type Request struct {
	Verb Verb
	// Slot is 1..MaxSlot for create and delete, and ignored for list. It is the ONLY thing a caller
	// chooses, and [AccountName] is the only thing that turns it into a name.
	Slot int
}

// Class is the machine-readable half of an error response.
//
// CALLERS BRANCH ON THE CLASS AND NEVER ON THE MESSAGE. The message is prose for an operator and is
// free to change; a caller that pattern-matched it would break the next time somebody improved the
// wording. This repository has already paid that bill once, when code branching on an error string
// turned a sentinel into plain text and broke three arms at once.
type Class string

const (
	// ClassBadRequest: the line is not a request — wrong arity, empty, over-long, or carrying bytes a
	// request never carries.
	ClassBadRequest Class = "bad_request"
	// ClassUnknownVerb: the first token is not one of the three verbs.
	ClassUnknownVerb Class = "unknown_verb"
	// ClassBadSlot: the slot token is not exactly two digits naming 01..99.
	ClassBadSlot Class = "bad_slot"
	// ClassExists: create was asked for a slot that already has an account.
	ClassExists Class = "exists"
	// ClassNotFound: delete was asked for a slot that has no account.
	ClassNotFound Class = "not_found"
	// ClassRefused: an account with that name exists but is not one this daemon created on this Mac,
	// so it will not be touched. This is the interesting refusal — see the deletion guard.
	ClassRefused Class = "refused"
	// ClassUnsupported: this is not a Mac, so there are no session accounts to manage.
	ClassUnsupported Class = "unsupported"
	// ClassInternal: a privileged step failed. The message names which.
	ClassInternal Class = "internal"
)

// Error carries a class and a human message together, so a handler returns one value and the serving
// loop does not have to guess which class a bare error deserves.
type Error struct {
	Class   Class
	Message string
}

func (e *Error) Error() string { return string(e.Class) + ": " + e.Message }

// Errorf builds an [Error] with a formatted message.
func Errorf(class Class, format string, args ...any) *Error {
	return &Error{Class: class, Message: fmt.Sprintf(format, args...)}
}

// ValidSlot is the ONE range check in this package, and every other function that takes a slot calls
// it. Keeping it single means a perturbation that removes it removes it everywhere — a check duplicated
// across three call sites is a check whose removal no test can observe.
func ValidSlot(slot int) error {
	if slot < 1 || slot > MaxSlot {
		return Errorf(ClassBadSlot, "slot %d is outside 01..%02d", slot, MaxSlot)
	}
	return nil
}

// AccountName is the ONLY way an account name comes into existence in this system, and it takes an int.
// There is deliberately no inverse that a caller can reach: names go one way, out of a slot.
func AccountName(slot int) (string, error) {
	if err := ValidSlot(slot); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%02d", namePrefix, slot), nil
}

// AccountUID is the uid slot N is allocated, mirroring session_uid in mac-sessions.sh.
func AccountUID(slot int) (int, error) {
	if err := ValidSlot(slot); err != nil {
		return 0, err
	}
	return UIDBase + slot, nil
}

// HomeDir is where slot N's home lives.
func HomeDir(slot int) (string, error) {
	name, err := AccountName(slot)
	if err != nil {
		return "", err
	}
	return "/Users/" + name, nil
}

// IsAccountName reports whether a name is inside this daemon's namespace, and returns the slot it
// belongs to.
//
// THIS IS THE SECOND DEFENCE, NOT THE FIRST. The first is that a caller cannot express a name at all.
// This one guards the other direction: the daemon reads account names back OUT of directory services
// when it lists and when it deletes, and a record it did not create must not be acted on because it
// happened to turn up in an enumeration. It refuses on SHAPE — exactly two digits after the prefix —
// for the same reason validate_index in palai-session-account does: `1`, `+1`, ` 1` and `0x11` are all
// things an integer parse will happily accept, and a value that took a different path to the same
// number took a path nobody tested.
func IsAccountName(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, namePrefix)
	if !ok || len(rest) != 2 || !isDigit(rest[0]) || !isDigit(rest[1]) {
		return 0, false
	}
	slot := int(rest[0]-'0')*10 + int(rest[1]-'0')
	if ValidSlot(slot) != nil {
		return 0, false
	}
	return slot, true
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// ParseRequest turns one wire line into a [Request], or refuses with a class.
//
// It refuses on SHAPE rather than on whatever arithmetic can be made of the token, which is why
// `07; rm -rf /` fails on arity before anything looks at `07`, and why `7`, `007`, `+7` and `0x11` fail
// even though each of them names a number some parser would accept.
func ParseRequest(line string) (Request, error) {
	if len(line) > MaxRequestBytes {
		return Request{}, Errorf(ClassBadRequest, "request is %d bytes, the limit is %d", len(line), MaxRequestBytes)
	}
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	// Fields would fold `create   07` and `create\t07` into a well-formed request. A protocol with one
	// spelling per request is a protocol whose tests cover it, so the separator is a single space.
	fields := strings.Split(line, " ")
	for _, f := range fields {
		if f == "" {
			return Request{}, Errorf(ClassBadRequest, "request has an empty token; the separator is exactly one space")
		}
		if strings.IndexFunc(f, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 {
			return Request{}, Errorf(ClassBadRequest, "request carries a byte outside printable ASCII")
		}
	}

	var verb Verb
	switch fields[0] {
	case "create":
		verb = VerbCreate
	case "delete":
		verb = VerbDelete
	case "list":
		verb = VerbList
	default:
		return Request{}, Errorf(ClassUnknownVerb, "%q is not one of create, delete, list", fields[0])
	}

	if verb == VerbList {
		if len(fields) != 1 {
			return Request{}, Errorf(ClassBadRequest, "list takes no argument, got %d", len(fields)-1)
		}
		return Request{Verb: verb}, nil
	}
	if len(fields) != 2 {
		return Request{}, Errorf(ClassBadRequest, "%s takes exactly one slot, got %d tokens", verb, len(fields)-1)
	}
	slot, err := parseSlot(fields[1])
	if err != nil {
		return Request{}, err
	}
	return Request{Verb: verb, Slot: slot}, nil
}

// parseSlot accepts exactly two ASCII digits naming 01..99 and nothing else.
func parseSlot(tok string) (int, error) {
	if len(tok) != 2 || !isDigit(tok[0]) || !isDigit(tok[1]) {
		return 0, Errorf(ClassBadSlot, "slot must be exactly two digits 01..%02d, got %q", MaxSlot, tok)
	}
	slot := int(tok[0]-'0')*10 + int(tok[1]-'0')
	if err := ValidSlot(slot); err != nil {
		return 0, err
	}
	return slot, nil
}

// Line encodes a request for the wire. It refuses to encode one it could not parse back, so a caller
// cannot put a malformed request on the socket by constructing the struct directly.
func (r Request) Line() (string, error) {
	word := r.Verb.Word()
	if word == "" {
		return "", Errorf(ClassUnknownVerb, "verb %d has no wire spelling", r.Verb)
	}
	if r.Verb == VerbList {
		return "list\n", nil
	}
	if err := ValidSlot(r.Slot); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s %02d\n", word, r.Slot), nil
}

// Response is what the daemon writes back. Unlike [Request] it does carry strings — a name and a home
// path — because this direction is the daemon TELLING the caller what it derived. Nothing here was
// chosen by the caller.
type Response struct {
	OK    bool
	Verb  Verb
	Name  string
	Home  string
	Slots []int

	Class   Class
	Message string
}

// OKAccount is the reply to a create or a delete.
func OKAccount(verb Verb, name, home string) Response {
	return Response{OK: true, Verb: verb, Name: name, Home: home}
}

// OKList is the reply to a list.
func OKList(slots []int) Response { return Response{OK: true, Verb: VerbList, Slots: slots} }

// Err is an error reply.
func Err(class Class, message string) Response {
	return Response{Class: class, Message: message}
}

// Line encodes a response for the wire, always exactly one line.
//
// The message is flattened onto one line because a newline inside it would otherwise turn one response
// into two and desynchronise a caller that reads line by line.
func (r Response) Line() string {
	if !r.OK {
		msg := strings.NewReplacer("\n", " ", "\r", " ").Replace(r.Message)
		return fmt.Sprintf("err %s %s\n", r.Class, msg)
	}
	if r.Verb == VerbList {
		out := "ok list"
		for _, s := range r.Slots {
			out += fmt.Sprintf(" %02d", s)
		}
		return out + "\n"
	}
	return fmt.Sprintf("ok %s %s %s\n", r.Verb.Word(), r.Name, r.Home)
}

// ParseResponse reads one reply. It is the caller's half of the protocol, and it exists here rather
// than in the caller so both halves are pinned by the same tests.
func ParseResponse(line string) (Response, error) {
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	fields := strings.Split(line, " ")
	switch fields[0] {
	case "err":
		if len(fields) < 3 {
			return Response{}, fmt.Errorf("malformed err response %q", line)
		}
		return Response{Class: Class(fields[1]), Message: strings.Join(fields[2:], " ")}, nil
	case "ok":
		if len(fields) < 2 {
			return Response{}, fmt.Errorf("malformed ok response %q", line)
		}
		switch fields[1] {
		case "list":
			slots := make([]int, 0, len(fields)-2)
			for _, tok := range fields[2:] {
				slot, err := parseSlot(tok)
				if err != nil {
					return Response{}, fmt.Errorf("malformed slot %q in %q", tok, line)
				}
				slots = append(slots, slot)
			}
			return OKList(slots), nil
		case "create", "delete":
			if len(fields) != 4 {
				return Response{}, fmt.Errorf("malformed %s response %q", fields[1], line)
			}
			verb := VerbCreate
			if fields[1] == "delete" {
				verb = VerbDelete
			}
			return OKAccount(verb, fields[2], fields[3]), nil
		}
	}
	return Response{}, fmt.Errorf("unrecognised response %q", line)
}

// SlotFromUID inverts [AccountUID]. The deletion guard uses it to check that a record's uid is the one
// this namespace would have allocated for its name; a record where the two disagree was edited after
// creation and we do not know by whom.
func SlotFromUID(uid int) (int, bool) {
	slot := uid - UIDBase
	if ValidSlot(slot) != nil {
		return 0, false
	}
	return slot, true
}
