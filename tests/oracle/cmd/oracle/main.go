// Command oracle records and compares the selected v1.6.5 parity fixtures.
//
// Recording is loopback-only and requires --fake-upstream. Comparing never
// contacts the real network unless the target is an explicit loopback URL.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	harness "github.com/ayeaaaa/mix2api-v2/tests/oracle/harness"
)

func main() {
	if code := run(os.Args[1:], os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "oracle: missing command (record or compare)")
		return 2
	}
	switch args[0] {
	case "record":
		return runRecord(args[1:], stdout, stderr)
	case "compare":
		return runCompare(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "oracle: unknown command %q\n", args[0])
		return 2
	}
}

func runRecord(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("oracle record", flag.ContinueOnError)
	flags.SetOutput(stderr)
	base := flags.String("base", "", "loopback base URL of the running v1.6.5 gateway")
	out := flags.String("out", "", "empty output directory")
	version := flags.String("version", "", "recorded v1 version")
	commit := flags.String("commit", "", "recorded v1 commit")
	fake := flags.Bool("fake-upstream", false, "confirm the gateway uses a local fake upstream")
	panel := flags.String("panel-token", "", "panel token")
	apiKey := flags.String("api-key", "", "API key")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *base == "" || *out == "" || *version == "" || *commit == "" {
		fmt.Fprintln(stderr, "oracle: --base, --out, --version, and --commit are required")
		return 2
	}
	recorder := &harness.Recorder{
		BaseURL: *base, Creds: harness.Credentials{PanelToken: *panel, APIKey: *apiKey},
		Provenance: harness.Provenance{
			TargetVersion: *version, TargetCommit: *commit, FakeUpstream: *fake,
			Note: "selected v1.6.5 fake-upstream responses; loopback only",
		},
		Now: time.Now,
	}
	if err := recorder.Record(context.Background(), *out); err != nil {
		fmt.Fprintln(stderr, "oracle: "+err.Error())
		return 1
	}
	fmt.Fprintf(stdout, "recorded %d oracle cases to %s\n", len(harness.DefaultCases()), *out)
	return 0
}

func runCompare(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("oracle compare", flag.ContinueOnError)
	flags.SetOutput(stderr)
	base := flags.String("base", "", "loopback base URL of the running v2 candidate")
	dir := flags.String("oracle", "", "oracle fixture directory")
	panel := flags.String("panel-token", "", "panel token")
	apiKey := flags.String("api-key", "", "API key")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *base == "" || *dir == "" {
		fmt.Fprintln(stderr, "oracle: --base and --oracle are required")
		return 2
	}
	comparer := &harness.Comparer{
		BaseURL: *base, Dir: *dir,
		Creds: harness.Credentials{PanelToken: *panel, APIKey: *apiKey},
	}
	if err := comparer.Compare(context.Background()); err != nil {
		fmt.Fprintln(stderr, "oracle: "+err.Error())
		return 1
	}
	fmt.Fprintln(stdout, "oracle comparison passed for all selected cases")
	return 0
}
