package harness

import "fmt"

// Run executes scenario sequentially n times (run indices 1..n), appending
// each returned Row to out. Sequential, not concurrent: scenarios mutate
// shared cluster/repo state and interleaving them would make results
// meaningless.
func Run(out *CSVWriter, n int, scenario func(run int) Row) error {
	for run := 1; run <= n; run++ {
		row := scenario(run)
		if err := out.Write(row); err != nil {
			return fmt.Errorf("run %d: write row: %w", run, err)
		}
	}
	return nil
}
