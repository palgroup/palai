package extensions

import (
	"context"
	"errors"
	"testing"
)

// TestCreateSkillRejectsUnsafeName is the SEC-1 root-cause guard: a skill NAME flows into a filesystem
// WRITE path at materialization (<alloc>/.palai/skills/<name>/), so a name carrying a path separator, a
// `..` segment, a control char, a leading dot, or emptiness must be REJECTED at create — before any row
// exists. Validation returns before the DB call, so a nil pool never panics here (proving no write).
func TestCreateSkillRejectsUnsafeName(t *testing.T) {
	s := New(nil) // CreateSkill validates the name BEFORE touching the pool
	bad := []string{
		"", "   ", "..", ".", ".hidden",
		"../../etc/passwd", "../../../../tmp/pwned", "a/b", "a\\b", "foo/../bar",
		"a\x00b", "bad\nname",
	}
	for _, name := range bad {
		if _, err := s.CreateSkill(context.Background(), "org", "prj", name); !errors.Is(err, ErrInvalidSkillName) {
			t.Errorf("CreateSkill(%q) err = %v, want ErrInvalidSkillName", name, err)
		}
	}
}
