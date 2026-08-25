package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"

	"qqcodex/internal/ops"
)

type response struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	RCCommit string `json:"rc_commit,omitempty"`
}

func main() {
	result, code := run(context.Background(), os.Args[1:])
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		os.Exit(1)
	}
	os.Exit(code)
}

func run(ctx context.Context, args []string) (response, int) {
	configPath, action, actionArgs, err := parseGlobal(args)
	if err != nil {
		return failed(err), 2
	}
	cfg, err := ops.LoadConfig(configPath)
	if err != nil {
		return failed(errors.New("load ops config failed")), 1
	}
	operator, err := ops.NewOperator(cfg)
	if err != nil {
		return failed(errors.New("invalid ops config")), 1
	}

	switch action {
	case "sync":
		project, err := parseSync(actionArgs)
		if err == nil {
			err = operator.Sync(ctx, project)
		}
		if err != nil {
			return failed(err), 1
		}
	case "push":
		project, task, branch, commit, err := parsePush(actionArgs)
		if err == nil {
			err = operator.PushTaskBundle(ctx, project, task, branch, commit, os.Stdin)
		}
		if err != nil {
			return failed(err), 1
		}
	case "merge":
		project, task, commit, err := parseMerge(actionArgs)
		if err != nil {
			return failed(err), 1
		}
		rcCommit, err := operator.MergeRC(ctx, project, task, commit)
		if err != nil {
			return failed(err), 1
		}
		return response{OK: true, RCCommit: rcCommit}, 0
	case "deploy":
		project, task, commit, err := parseDeploy(actionArgs)
		if err == nil {
			err = operator.DeployRC(ctx, project, task, commit)
		}
		if err != nil {
			return failed(err), 1
		}
	default:
		return failed(errors.New("unknown action")), 2
	}
	return response{OK: true}, 0
}

func parseGlobal(args []string) (string, string, []string, error) {
	flags := flag.NewFlagSet("qqcodex-ops", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "")
	if err := flags.Parse(args); err != nil || *configPath == "" || flags.NArg() == 0 {
		return "", "", nil, errors.New("invalid command")
	}
	rest := flags.Args()
	return *configPath, rest[0], rest[1:], nil
}

func parseSync(args []string) (string, error) {
	flags := newActionFlags("sync")
	project := flags.String("project", "", "")
	if err := parseAction(flags, args); err != nil || *project == "" {
		return "", errors.New("invalid sync command")
	}
	return *project, nil
}

func parsePush(args []string) (string, string, string, string, error) {
	flags := newActionFlags("push")
	project := flags.String("project", "", "")
	task := flags.String("task", "", "")
	branch := flags.String("branch", "", "")
	commit := flags.String("commit", "", "")
	if err := parseAction(flags, args); err != nil || *project == "" || *task == "" || *branch == "" || *commit == "" {
		return "", "", "", "", errors.New("invalid push command")
	}
	return *project, *task, *branch, *commit, nil
}

func parseMerge(args []string) (string, string, string, error) {
	flags := newActionFlags("merge")
	project := flags.String("project", "", "")
	task := flags.String("task", "", "")
	commit := flags.String("commit", "", "")
	if err := parseAction(flags, args); err != nil || *project == "" || *task == "" || *commit == "" {
		return "", "", "", errors.New("invalid merge command")
	}
	return *project, *task, *commit, nil
}

func parseDeploy(args []string) (string, string, string, error) {
	flags := newActionFlags("deploy")
	project := flags.String("project", "", "")
	task := flags.String("task", "", "")
	commit := flags.String("rc-commit", "", "")
	if err := parseAction(flags, args); err != nil || *project == "" || *task == "" || *commit == "" {
		return "", "", "", errors.New("invalid deploy command")
	}
	return *project, *task, *commit, nil
}

func newActionFlags(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

func parseAction(flags *flag.FlagSet, args []string) error {
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("invalid action flags")
	}
	return nil
}

func failed(err error) response {
	return response{OK: false, Error: err.Error()}
}
