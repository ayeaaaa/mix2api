package main

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/core/version"
)

// runCLI dispatches the command line. It returns the process exit code. The
// server is the default when no subcommand is given; release, migration, and
// update subcommands are deliberately explicit so a mistyped argument can
// never silently start a long-running gateway.
func runCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		runServer()
		return 0
	}
	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case "version":
		return runVersionCommand(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	case "migrate":
		return runMigrateCommand(args[1:], stdout, stderr)
	case "rollback":
		return runRollbackCommand(args[1:], stdout, stderr)
	case "update":
		return runUpdateCommand(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "mix2api: unknown command %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `mix2api - multi-provider AI gateway

Usage:
  mix2api                     start the server (HOST/PORT env)
  mix2api version [--json]    print build version, commit, and date
  mix2api migrate [flags]     copy a v1.6.5 data tree to v2 (copy-only)
  mix2api rollback [flags]    copy a v2 data tree back to a v1-compatible tree
  mix2api update <action>     run the privileged update executor

Run "mix2api <command> -h" for command flags.
`)
}

func runVersionCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("version", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "print one JSON object")
	short := flags.Bool("short", false, "print only the semantic version")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "mix2api version: unexpected arguments: "+strings.Join(flags.Args(), " "))
		return 2
	}
	info := version.Current()
	if *short {
		fmt.Fprintln(stdout, info.Version)
		return 0
	}
	if *asJSON {
		raw, err := info.JSON()
		if err != nil {
			fmt.Fprintln(stderr, "mix2api version: "+err.Error())
			return 1
		}
		fmt.Fprintln(stdout, string(raw))
		return 0
	}
	fmt.Fprint(stdout, info.String())
	return 0
}
