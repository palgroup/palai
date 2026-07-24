//go:build component

package workers

// SetAfterFenceGuardHook installs (or clears, with nil) the deterministic-interleave seam the fence-TOCTOU
// component tests use: the hook fires in the WINDOW between the fence guard read and the guarded journal
// append (SubmitResult and ClaimNext), so a test can commit a concurrent re-dispatch / health change into
// exactly that window and assert the guarded append closes it. Test-only; the hook is nil in every non-test
// build.
func SetAfterFenceGuardHook(fn func()) { afterFenceGuardHook = fn }
