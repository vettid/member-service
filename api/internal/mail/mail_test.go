package mail

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValid(t *testing.T) {
	for addr, want := range map[string]bool{
		"a@example.org": true, "first.last+tag@sub.example.org": true,
		"": false, "a": false, "@example.org": false, "a@": false, "a@example": false, "a@@example.org": false,
		"a b@example.org": false, "a\n@example.org": false, "<a@example.org>": false, `"a"@example.org`: false, "Ada <a@example.org>": false,
		strings.Repeat("a", 250) + "@x.org": false,
	} {
		if Valid(addr) != want {
			t.Errorf("Valid(%q) = %v", addr, !want)
		}
	}
	if Normalize("  Ada@Example.ORG ") != "ada@example.org" {
		t.Fatal("Normalize")
	}
}

// The code is in the subject and the body; Dir writes 0600 files.
func TestDir(t *testing.T) {
	d := &Dir{Path: t.TempDir()}
	if err := d.SendCode(context.Background(), "a@example.org", "123456", Connect); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(d.Path, "*.eml"))
	if len(files) != 1 {
		t.Fatalf("%d files", len(files))
	}
	b, _ := os.ReadFile(files[0])
	fi, _ := os.Stat(files[0])
	if !strings.Contains(string(b), "Subject: 123456 is your") || !strings.Contains(string(b), "is 123456.") || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%s %v", b, fi.Mode())
	}
}
