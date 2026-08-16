package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// StrayProcessPIDs returns the PIDs of every process under procDir whose
// comm is name, excluding self, sorted ascending. procDir is normally
// "/proc"; a caller-supplied path lets tests point this at a fake
// directory tree without touching the real one.
func StrayProcessPIDs(procDir, name string, self int) ([]int, error) {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		comm, err := os.ReadFile(filepath.Join(procDir, e.Name(), "comm"))
		if err != nil {
			// Process exited between the directory listing and this read,
			// or comm isn't readable for another reason; not this check's
			// concern either way.
			continue
		}
		if strings.TrimSpace(string(comm)) == name {
			pids = append(pids, pid)
		}
	}
	slices.Sort(pids)
	return pids, nil
}

// CheckNoStraySwarmgate refuses to proceed if more than one swarmgate
// process is running on this host. A campaign drives a single running
// instance (the one under test); anything beyond that is leftover debris
// from an earlier manual run or demo, and every extra instance polls and
// reconciles the same Docker daemon concurrently — corrupting
// measurements with unrelated apply/converge/API activity, and in
// extreme cases starving the Docker API enough to make the campaign's
// own AwaitConverged calls time out. This is what actually happened
// during this project's own pre-public review: seven abandoned swarmgate
// processes left running from unrelated demos caused exactly this
// failure mode against a real cluster.
//
// Linux-only: procDir is read via the /proc filesystem interface, which
// has no portable equivalent. If procDir can't be read at all (a
// non-Linux host, or /proc genuinely unavailable), the check is skipped
// rather than blocking the campaign outright — this is a safety net, not
// a hard platform requirement.
func CheckNoStraySwarmgate() error {
	pids, err := StrayProcessPIDs("/proc", "swarmgate", os.Getpid())
	if err != nil {
		return nil
	}
	if len(pids) > 1 {
		return fmt.Errorf("refusing to start: %d swarmgate processes are running on this host (pids %v), expected at most 1 (the instance under test) — stop the extras before running a campaign", len(pids), pids)
	}
	return nil
}
