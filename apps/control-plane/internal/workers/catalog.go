package workers

// Operation is one typed operation of a capability. ReadOnly marks whether a failure may be retried on
// another compatible worker (§31.6): a read-only operation (a compile check produces an artifact but touches
// no external side effect) is freely retried; a side-effecting one relies on destination idempotency instead.
type Operation struct {
	Name     string
	ReadOnly bool
}

// Catalog is the WHOLE of what a capability worker may run — the no-tunnel allowlist (§31.5). A dispatch,
// claim, or submit for an operation NOT in its capability's map is refused (ErrUntypedOperation): there is no
// generic connect/proxy/exec operation anywhere, so an ordinary sandbox worker cannot be used as a general
// tunnel. Enrollment for a capability absent from Catalog is refused too, so the surface can only ever run
// operations the control plane has explicitly typed.
//
// apple-build is DELIBERATELY ABSENT: it is DISABLED in discovery (no real Xcode + signing proof), so NO
// apple-build operation exists to dispatch — there is no signing/build/store operation in the system at all.
// The macOS fixture leg proves only the swift-toolchain 'swift.build-check' typed operation (a real compile
// check where swiftc is present, an honest toy build otherwise); a real signed Apple build is a separate
// capability and the §6 leg 3 operator work.
var Catalog = map[string]map[string]Operation{
	"swift-toolchain": {
		"swift.build-check": {Name: "swift.build-check", ReadOnly: true},
	},
}

// LookupOperation returns the typed operation and whether it is a typed operation of the capability. A false
// second return is the no-tunnel refusal condition.
func LookupOperation(capability, operation string) (Operation, bool) {
	ops, ok := Catalog[capability]
	if !ok {
		return Operation{}, false
	}
	op, ok := ops[operation]
	return op, ok
}

// KnownCapability reports whether the control plane types (and therefore can enroll a worker for) a
// capability. apple-build returns false — it has no Catalog entry.
func KnownCapability(capability string) bool {
	_, ok := Catalog[capability]
	return ok
}
