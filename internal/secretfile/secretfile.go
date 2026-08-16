// Package secretfile validates the permission bits on files that hold
// credential material (SSH private keys, registry auth config) before
// swarmgate reads them.
package secretfile

import (
	"fmt"
	"os"
)

// CheckPrivate rejects path if its mode grants group or other any
// permission. A secret file left group/world-readable is most often an
// oversight (created via a shell redirect, copied without preserving mode)
// rather than a deliberate sharing decision, and is worth failing loudly on
// at startup rather than trusting silently.
func CheckPrivate(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s: mode %04o is readable or writable by group/other; chmod 600 it", path, perm)
	}
	return nil
}
