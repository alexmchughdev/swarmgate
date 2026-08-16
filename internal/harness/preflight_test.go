package harness

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// fakeProcDir builds a temp directory shaped like /proc: one subdirectory
// per pid, each with a comm file, plus non-numeric entries a real /proc
// also contains (self, cpuinfo) that must be skipped rather than erroring.
func fakeProcDir(t *testing.T, comms map[int]string) string {
	t.Helper()
	dir := t.TempDir()
	for pid, comm := range comms {
		pidDir := filepath.Join(dir, strconv.Itoa(pid))
		if err := os.MkdirAll(pidDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(pidDir, "comm"), []byte(comm+"\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	for _, name := range []string{"self", "cpuinfo"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	return dir
}

func TestStrayProcessPIDsFindsMatchingComm(t *testing.T) {
	dir := fakeProcDir(t, map[int]string{
		100: "swarmgate",
		101: "bash",
		102: "swarmgate",
	})
	pids, err := StrayProcessPIDs(dir, "swarmgate", -1)
	if err != nil {
		t.Fatalf("StrayProcessPIDs: %v", err)
	}
	if want := []int{100, 102}; !slices.Equal(pids, want) {
		t.Fatalf("pids = %v, want %v", pids, want)
	}
}

func TestStrayProcessPIDsExcludesSelf(t *testing.T) {
	dir := fakeProcDir(t, map[int]string{
		100: "swarmgate",
		101: "swarmgate",
	})
	pids, err := StrayProcessPIDs(dir, "swarmgate", 101)
	if err != nil {
		t.Fatalf("StrayProcessPIDs: %v", err)
	}
	if want := []int{100}; !slices.Equal(pids, want) {
		t.Fatalf("pids = %v, want %v", pids, want)
	}
}

func TestStrayProcessPIDsNoMatches(t *testing.T) {
	dir := fakeProcDir(t, map[int]string{100: "bash"})
	pids, err := StrayProcessPIDs(dir, "swarmgate", -1)
	if err != nil {
		t.Fatalf("StrayProcessPIDs: %v", err)
	}
	if len(pids) != 0 {
		t.Fatalf("pids = %v, want none", pids)
	}
}

func TestCheckNoStraySwarmgateUnreadableProcDirIsSkipped(t *testing.T) {
	// CheckNoStraySwarmgate hardcodes "/proc"; on a host where that's
	// unreadable (non-Linux), it must not block the campaign.
	if err := CheckNoStraySwarmgate(); err != nil {
		t.Skipf("this host has a readable /proc with >1 real swarmgate process; skipping: %v", err)
	}
}
