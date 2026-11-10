package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/update"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/update/apply"
)

type repeatFlag []string

func (r *repeatFlag) String() string { return strings.Join(*r, ",") }
func (r *repeatFlag) Set(value string) error {
	*r = append(*r, value)
	return nil
}

func runUpdateCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "mix2api update: missing action (run, install, rollback, recover)")
		return 2
	}
	action := strings.ToLower(strings.TrimSpace(args[0]))
	flags := flag.NewFlagSet("update "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	requestDir := flags.String("request-dir", update.DefaultRequestDir, "executor request directory")
	root := flags.String("root", "", "install root (default: directory of this executable)")
	backupDir := flags.String("backup-dir", "", "backup directory (default: <root>/.mix2api-backups)")
	archive := flags.String("archive", "", "local release archive (offline use; requires --checksums)")
	checksums := flags.String("checksums", "", "local SHA256SUMS for --archive")
	stopCommand := flags.String("stop-command", "", "service stop executable (no shell)")
	startCommand := flags.String("start-command", "", "service start executable (no shell)")
	var stopArgs, startArgs repeatFlag
	flags.Var(&stopArgs, "stop-arg", "argument for --stop-command (repeatable)")
	flags.Var(&startArgs, "start-arg", "argument for --start-command (repeatable)")
	version := flags.String("version", "", "target version for install/rollback (default: queued request)")
	asJSON := flags.Bool("json", false, "print one JSON result")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "mix2api update: unexpected arguments: "+strings.Join(flags.Args(), " "))
		return 2
	}

	options := apply.Options{
		RequestDir: *requestDir, Root: *root, BackupDir: *backupDir,
		ArchivePath: *archive, Checksums: *checksums,
		Logger: func(message string) { fmt.Fprintln(stderr, message) },
		Controller: &apply.CommandController{
			StopCommand:  apply.CommandSpec{Path: *stopCommand, Args: append([]string(nil), stopArgs...)},
			StartCommand: apply.CommandSpec{Path: *startCommand, Args: append([]string(nil), startArgs...)},
			Logger:       func(message string) { fmt.Fprintln(stderr, message) },
		},
	}
	applier, err := apply.New(options)
	if err != nil {
		fmt.Fprintln(stderr, "mix2api update: "+err.Error())
		return 1
	}
	ctx := context.Background()

	if action == "recover" {
		if err := applier.Recover(ctx); err != nil {
			fmt.Fprintln(stderr, "mix2api update: "+err.Error())
			return 1
		}
		result := apply.Result{State: "restored", Message: "已恢复中断的更新"}
		writeUpdateResult(stdout, *asJSON, result)
		return 0
	}

	request, err := updateRequestForAction(action, *version, *requestDir, applier.Root())
	if err != nil {
		fmt.Fprintln(stderr, "mix2api update: "+err.Error())
		return 1
	}
	result, err := applier.Run(ctx, request)
	if err != nil {
		writeUpdateResult(stdout, *asJSON, result)
		fmt.Fprintln(stderr, "mix2api update: "+err.Error())
		return 1
	}
	writeUpdateResult(stdout, *asJSON, result)
	return 0
}

func updateRequestForAction(action, version, requestDir, root string) (update.Request, error) {
	switch action {
	case "run":
		request, ok := update.NewExecutor(requestDir).ReadRequest()
		if !ok {
			return update.Request{}, fmt.Errorf("no valid request in %s", requestDir)
		}
		if request.Root == "" {
			request.Root = root
		}
		return request, nil
	case "install", "rollback":
		version = strings.TrimPrefix(strings.TrimSpace(version), "v")
		if version == "" {
			return update.Request{}, fmt.Errorf("--version is required for %s", action)
		}
		return update.Request{Action: action, Version: version, Tag: "v" + version, Root: root}, nil
	default:
		return update.Request{}, fmt.Errorf("unknown action %q (want run, install, rollback, recover)", action)
	}
}

func writeUpdateResult(stdout io.Writer, asJSON bool, result apply.Result) {
	if asJSON {
		raw, err := json.Marshal(result)
		if err == nil {
			fmt.Fprintln(stdout, string(raw))
			return
		}
	}
	fmt.Fprintf(stdout, "%s %s: %s\n", result.State, result.Version, result.Message)
}
