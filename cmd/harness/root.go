package main

import (
	"flag"
	"fmt"
	"os"
	"slices"

	"github.com/alexmchughdev/swarmgate/internal/harness"
)

// sharedFlags are common to every scenario subcommand.
type sharedFlags struct {
	n          int
	out        string
	eventsFile string
	label      string
}

func bindSharedFlags(fs *flag.FlagSet) *sharedFlags {
	sf := &sharedFlags{}
	fs.IntVar(&sf.n, "n", 30, "number of runs")
	fs.StringVar(&sf.out, "out", "results.csv", "results CSV path (appended; header written once)")
	fs.StringVar(&sf.eventsFile, "events-file", "", "path to swarmgate telemetry JSONL (required by most scenarios; not read by t8, which polls ArgoCD's own status instead)")
	fs.StringVar(&sf.label, "label", "", "freeform condition suffix, e.g. events=on")
	return sf
}

// scenarioCmd is one harness subcommand. bind registers the scenario's own
// flags on fs (in addition to the shared ones already bound) and returns
// the function to run once fs.Parse has populated every flag variable —
// splitting registration from execution is what lets one FlagSet cover
// both shared and scenario-specific flags in a single Parse call.
type scenarioCmd struct {
	name string
	bind func(fs *flag.FlagSet, sf *sharedFlags) (run func() error)
	// noEventsFile opts out of the shared --events-file requirement, for
	// scenarios with no swarmgate telemetry to read at all (t8, which
	// polls ArgoCD's own Application status instead of swarmgate JSONL).
	noEventsFile bool
}

var scenarios = map[string]scenarioCmd{}

// register adds a scenario subcommand. Scenario packages call this from an
// init() function so cmd/harness/root.go never needs to change as
// scenarios are added.
func register(c scenarioCmd) {
	if _, dup := scenarios[c.name]; dup {
		panic(fmt.Sprintf("harness: scenario %q registered twice", c.name))
	}
	scenarios[c.name] = c
}

func dispatch(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: harness <scenario> [flags]")
		printScenarios()
		return 2
	}
	name := args[0]
	cmd, ok := scenarios[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "harness: unknown scenario %q\n", name)
		printScenarios()
		return 2
	}

	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	sf := bindSharedFlags(fs)
	run := cmd.bind(fs, sf)
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if sf.eventsFile == "" && !cmd.noEventsFile {
		fmt.Fprintln(os.Stderr, "harness: --events-file is required")
		return 2
	}
	if err := harness.CheckNoStraySwarmgate(); err != nil {
		fmt.Fprintln(os.Stderr, "harness:", err)
		return 2
	}

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "harness:", err)
		return 1
	}
	return 0
}

func printScenarios() {
	names := make([]string, 0, len(scenarios))
	for name := range scenarios {
		names = append(names, name)
	}
	slices.Sort(names)
	fmt.Fprintln(os.Stderr, "available scenarios:")
	for _, name := range names {
		fmt.Fprintln(os.Stderr, " ", name)
	}
}
