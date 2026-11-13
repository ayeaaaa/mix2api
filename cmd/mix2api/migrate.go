package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/core/migrate"
)

func runMigrateCommand(args []string, stdout, stderr io.Writer) int {
	return runCopyCommand("migrate", args, stdout, stderr)
}

func runRollbackCommand(args []string, stdout, stderr io.Writer) int {
	return runCopyCommand("rollback", args, stdout, stderr)
}

func runCopyCommand(direction string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(direction, flag.ContinueOnError)
	flags.SetOutput(stderr)
	from := flags.String("from", "", "source v1 data root or accounts directory")
	to := flags.String("to", "", "new destination directory (must be empty or absent)")
	receipt := flags.String("receipt", "", "checksum receipt path (default: <to>.migration.json)")
	dryRun := flags.Bool("dry-run", false, "validate and summarize without writing")
	asJSON := flags.Bool("json", false, "print one JSON summary")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "mix2api %s: unexpected arguments: %s\n", direction, strings.Join(flags.Args(), " "))
		return 2
	}
	if strings.TrimSpace(*from) == "" || strings.TrimSpace(*to) == "" {
		fmt.Fprintf(stderr, "mix2api %s: --from and --to are required\n", direction)
		return 2
	}
	options := migrate.Options{
		From: *from, To: *to, Receipt: *receipt, DryRun: *dryRun,
		Logger: func(message string) { fmt.Fprintln(stderr, message) },
	}
	var summary migrate.Summary
	var err error
	if direction == "rollback" {
		summary, err = migrate.Rollback(options)
	} else {
		summary, err = migrate.Migrate(options)
	}
	if err != nil {
		fmt.Fprintf(stderr, "mix2api %s: %v\n", direction, err)
		return 1
	}
	if *asJSON {
		raw, marshalErr := json.Marshal(summary)
		if marshalErr != nil {
			fmt.Fprintf(stderr, "mix2api %s: %v\n", direction, marshalErr)
			return 1
		}
		fmt.Fprintln(stdout, string(raw))
		return 0
	}
	verb := "migrated"
	if direction == "rollback" {
		verb = "restored"
	}
	fmt.Fprintf(stdout, "%s %d file(s), %d bytes (%s layout) from %s to %s\n",
		verb, summary.Files, summary.Bytes, summary.Layout, summary.Source, summary.Dest)
	fmt.Fprintf(stdout, "accounts=%d usage_rows=%d sqlite=%d dry_run=%v\n",
		summary.Accounts, summary.UsageRows, summary.SQLite, summary.DryRun)
	if !summary.DryRun {
		receiptPath := *receipt
		if receiptPath == "" {
			receiptPath = summary.Dest + ".migration.json"
		}
		fmt.Fprintf(stdout, "receipt=%s\n", receiptPath)
	}
	return 0
}
