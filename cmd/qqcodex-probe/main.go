package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"qqcodex/internal/config"
	"qqcodex/internal/onebot"
)

type prober func(context.Context, string, string) (onebot.Account, error)

type probeOutput struct {
	SelfID          string   `json:"self_id"`
	Nickname        string   `json:"nickname"`
	GroupIDs        []string `json:"group_ids"`
	MissingGroupIDs []string `json:"missing_group_ids"`
}

func main() {
	os.Exit(mainCode())
}

func mainCode() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, onebot.Probe); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer, probe prober) error {
	if ctx == nil || getenv == nil || stdout == nil || probe == nil {
		return errors.New("probe command dependencies are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	configPath, err := parseArgs(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return errors.New("load configuration failed")
	}
	token := getenv(cfg.OneBot.AccessTokenEnv)
	if token == "" {
		return errors.New("onebot access token is required")
	}

	account, err := probe(ctx, cfg.OneBot.URL, token)
	if err != nil {
		return errors.New("probe OneBot account failed")
	}

	output := probeOutput{
		SelfID:          string(account.SelfID),
		Nickname:        account.Nickname,
		GroupIDs:        groupIDStrings(account.GroupIDs),
		MissingGroupIDs: missingGroupIDs(cfg.AllowedGroupIDs, account.GroupIDs),
	}
	if err := json.NewEncoder(stdout).Encode(output); err != nil {
		return errors.New("write probe output failed")
	}
	return nil
}

func parseArgs(args []string) (string, error) {
	flags := flag.NewFlagSet("qqcodex-probe", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "")
	if len(args) != 2 || args[0] != "-config" || args[1] == "" || flags.Parse(args) != nil || flags.NArg() != 0 || *configPath == "" {
		return "", errors.New("usage: qqcodex-probe -config <path>")
	}
	return *configPath, nil
}

func groupIDStrings(groupIDs []onebot.ID) []string {
	values := make([]string, len(groupIDs))
	for i, groupID := range groupIDs {
		values[i] = string(groupID)
	}
	return values
}

func missingGroupIDs(configured []string, groupIDs []onebot.ID) []string {
	present := make(map[string]struct{}, len(groupIDs))
	for _, groupID := range groupIDs {
		present[string(groupID)] = struct{}{}
	}

	missing := make([]string, 0, len(configured))
	seen := make(map[string]struct{}, len(configured))
	for _, groupID := range configured {
		if _, exists := present[groupID]; exists {
			continue
		}
		if _, exists := seen[groupID]; exists {
			continue
		}
		seen[groupID] = struct{}{}
		missing = append(missing, groupID)
	}
	return missing
}
