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
// ‼️ AND THAT SHAPE IS WHAT LETS THIS DAEMON START A PROCESS AT ALL. [VerbSpawn] runs a session worker
// as `palai-sNN`, which is the only way a tenant's work is ever the tenant's own uid — the execers
// cannot drop privilege themselves, because only uid 0 may become another uid and no execer is uid 0.
// A root daemon with a `run` verb would ordinarily be the worst thing in a system; this one is
// tolerable for one reason and it is structural: the caller chooses an integer and the daemon chooses
// the program, from [InstalledWorkerPath]. Adding a string field to [Request] would not weaken this
// verb, it would DELETE the argument for having it.
//
// TestTheProtocolCannotExpressAnAccountNameAtAll in cmd/palai-agentd reads that shape back out of the
// AST, because an absence has no behaviour to observe and the only way to regress it is to change a
// declaration.
package macagent

import (
	"fmt"
	"strconv"
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

// AccountGroupName is the group every session account is created in, and AccountGID is its id.
//
// ‼️ IT IS NOT `staff` (20), AND IT USED TO BE, AND THAT WAS THE WHOLE ISOLATION MISSING ITS POINT.
// Measured on this machine on 2026-08-06: `/Users/salih` is `drwxr-x---` owned by `salih:staff`, and
// every local macOS account is created with PrimaryGroupID 20 unless told otherwise. So a session
// account in `staff` traverses the operator's home by the GROUP bit and reads every project under it —
// `ls /Users/salih/workspace` from inside a session listed sixteen unrelated checkouts. The uid drop
// would have isolated sessions from EACH OTHER and left the machine's owner completely exposed, which
// is worse than no isolation, because a deployment that wired it would believe it had a boundary.
//
// A DEDICATED GROUP IS THE BOUNDARY, and it is only a boundary while nothing else joins it. Nothing
// does: the two creators below put session accounts in it and no other code adds a member, so the
// operator's home admits these accounts by neither owner, group, nor other.
//
// 700 mirrors [UIDBase] deliberately — slot N is uid 700+N and the group they share is 700, so an
// operator reading `ls -l` sees one namespace rather than two unrelated numbers. Both were free on a
// stock macOS (701/702 are Apple's sharepoint groups; 700 is not allocated).
//
// IT IS A CONSTANT HERE BECAUSE IT HAS TWO WRITERS AND ONE READER THAT MUST AGREE WITH BOTH. The
// writers are the creators — cmd/palai-agentd/accounts.go's `-GID` argument and mac-sessions.sh's —
// and the reader is the control plane, which hands a uid/gid pair to whatever drops privilege to it
// (execution.SessionAccount). A gid the account was not created with is not a smaller boundary, it is
// a command that cannot open its own home directory, so the number lives in one place and both
// creators spend it. The shell one spent a LITERAL `20` until 2026-08-06 while this comment already
// claimed otherwise; TestBothAccountCreatorsSpendTheSameGroup now measures the claim.
const (
	AccountGroupName = "palai-sessions"
	AccountGID       = 700
)

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
//
// VerbVersion grants NOTHING: it takes no argument, touches no account, and answers a build stamp
// packages/version already calls a build identifier and never a secret. It exists because the
// alternative was worse — see [Prober.Probe] for why the RUNNING daemon's answer, and not the binary
// sitting on disk, is what an upgrade decision has to be made on.
//
// ‼️ VerbSpawn IS THE ONE VERB THAT STARTS A PROCESS, AND IT TAKES THE SAME SINGLE INTEGER THE OTHERS
// DO. That is not a convenience, it is the whole reason a root daemon may have this verb at all: the
// caller says WHICH SLOT and the daemon decides WHAT TO RUN, from [InstalledWorkerPath], which is a
// constant here and settable by nobody. The opposite spelling — a caller that names a program, an argv
// or an environment — would turn this socket into `sudo ANY`, which is the sentence the whole package
// was shaped to make unwriteable. See the package comment, and TestSpawnCannotBeToldWhatToRun.
const (
	VerbUnknown Verb = iota
	VerbCreate
	VerbDelete
	VerbList
	VerbVersion
	VerbSpawn
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
	case VerbVersion:
		return "version"
	case VerbSpawn:
		return "spawn"
	default:
		return ""
	}
}

// TakesSlot reports whether this verb carries the one integer a caller chooses. It is a method rather
// than a comparison repeated at each site because a verb added without a slot — VerbVersion was — must
// join the zero-argument branch of the parser, the encoder AND the dispatcher, and three separate
// comparisons is three places to only update two of.
func (v Verb) TakesSlot() bool { return v == VerbCreate || v == VerbDelete || v == VerbSpawn }

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
	// ClassUnknownVerb: the first token is not one of the verbs.
	ClassUnknownVerb Class = "unknown_verb"
	// ClassBadSlot: the slot token is not exactly two digits naming 01..99.
	ClassBadSlot Class = "bad_slot"
	// ClassExists: create was asked for a slot that already has an account.
	ClassExists Class = "exists"
	// ClassNotFound: delete or spawn was asked for a slot that has no account. Spawn shares the class
	// rather than getting its own because the caller's next step is the same one: create it first.
	ClassNotFound Class = "not_found"
	// ClassRefused: an account with that name exists but is not one this daemon created on this Mac,
	// so it will not be touched. This is the interesting refusal — see [notOurAccountBecause] in
	// cmd/palai-agentd, which both delete and spawn go through. The two directions are worth naming
	// together: refusing to DELETE a record we did not create keeps this daemon from erasing somebody
	// else's account, and refusing to SPAWN as one keeps it from starting a process as somebody else's
	// uid. It is the same fact — a name is not authority — spent twice.
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
	case "version":
		verb = VerbVersion
	case "spawn":
		verb = VerbSpawn
	default:
		return Request{}, Errorf(ClassUnknownVerb, "%q is not one of create, delete, list, version, spawn", fields[0])
	}

	if !verb.TakesSlot() {
		if len(fields) != 1 {
			return Request{}, Errorf(ClassBadRequest, "%s takes no argument, got %d", verb, len(fields)-1)
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
	if !r.Verb.TakesSlot() {
		return word + "\n", nil
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
	// Version is the build stamp of the daemon that ANSWERED, and it is deliberately not read from the
	// binary on disk. See [Prober.Probe].
	Version string
	// PID is the session worker a spawn started. It is here because a caller needs something it can
	// OBSERVE: "the daemon says it started one" is a claim, and a pid is the handle that turns it into
	// a measurement anybody with `ps` can take. It is not a capability — the caller may not signal a
	// process it does not own — and it is the daemon telling the caller what it derived, which is what
	// this whole direction of the protocol is for.
	PID int

	Class   Class
	Message string
}

// OKAccount is the reply to a create or a delete.
func OKAccount(verb Verb, name, home string) Response {
	return Response{OK: true, Verb: verb, Name: name, Home: home}
}

// OKList is the reply to a list.
func OKList(slots []int) Response { return Response{OK: true, Verb: VerbList, Slots: slots} }

// OKVersion is the reply to a version.
func OKVersion(stamp string) Response { return Response{OK: true, Verb: VerbVersion, Version: stamp} }

// OKSpawn is the reply to a spawn: the account the worker was started as, and its pid.
//
// IT ANSWERS THE NAME AS WELL AS THE PID because the name is the thing the caller could not say. A
// reply carrying only a pid would leave the caller trusting that the slot it asked for is the account
// the process got; this way the daemon states which uid it spent, and a caller comparing that against
// the slot it sent is comparing two derivations of the same integer.
func OKSpawn(name string, pid int) Response {
	return Response{OK: true, Verb: VerbSpawn, Name: name, PID: pid}
}

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
	if r.Verb == VerbVersion {
		// Flattened for the reason a message is: PALAI_VERSION lets an operator pin the reported stamp
		// to any string, and one carrying a newline would turn one response into two and desynchronise a
		// caller reading line by line.
		return "ok version " + flattenLine(r.Version) + "\n"
	}
	if r.Verb == VerbSpawn {
		return fmt.Sprintf("ok spawn %s %d\n", r.Name, r.PID)
	}
	return fmt.Sprintf("ok %s %s %s\n", r.Verb.Word(), r.Name, r.Home)
}

func flattenLine(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
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
	// A daemon too old to know the verb answers `err unknown_verb ...`, which lands on the branch above
	// and is a perfectly good answer: it says a daemon is THERE and cannot name itself, which is a
	// reason to upgrade rather than a reason to be unreachable.
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
		case "version":
			if len(fields) < 3 {
				return Response{}, fmt.Errorf("malformed version response %q", line)
			}
			return OKVersion(strings.Join(fields[2:], " ")), nil
		case "spawn":
			if len(fields) != 4 {
				return Response{}, fmt.Errorf("malformed spawn response %q", line)
			}
			// The pid is parsed rather than carried as text, so a reply this side cannot make sense of is
			// a protocol error here instead of a zero the caller reports as a running worker.
			pid, err := strconv.Atoi(fields[3])
			if err != nil || pid <= 0 {
				return Response{}, fmt.Errorf("malformed pid %q in %q", fields[3], line)
			}
			return OKSpawn(fields[2], pid), nil
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
