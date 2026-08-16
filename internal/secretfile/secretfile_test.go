package secretfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckPrivateAcceptsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	writeFile(t, path, 0o600)

	if err := CheckPrivate(path); err != nil {
		t.Fatalf("CheckPrivate: %v", err)
	}
}

func TestCheckPrivateRejectsGroupOrWorldReadable(t *testing.T) {
	dir := t.TempDir()
	for _, mode := range []uint32{0o640, 0o644, 0o604, 0o660} {
		path := filepath.Join(dir, "key")
		writeFile(t, path, mode)

		err := CheckPrivate(path)
		if err == nil {
			t.Fatalf("mode %04o: CheckPrivate() = nil, want an error", mode)
		}
		if !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("mode %04o: error %q does not mention the fix", mode, err)
		}
	}
}

func TestCheckPrivatePropagatesStatError(t *testing.T) {
	if err := CheckPrivate(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("CheckPrivate() = nil, want an error for a nonexistent file")
	}
}

func writeFile(t *testing.T, path string, mode uint32) {
	t.Helper()
	if err := os.WriteFile(path, []byte("secret"), os.FileMode(mode)); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	// os.WriteFile only applies mode to a newly created file; force it in
	// case the test reuses a path across table entries with a stricter
	// umask already narrowing the requested bits.
	if err := os.Chmod(path, os.FileMode(mode)); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}
