package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"qqcodex/internal/config"
)

func TestRunAcceptsOnlyConfigAndReadsConfiguredToken(t *testing.T) {
	cfg := validConfig(t)
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, path, cfg)

	const token = "napcat-token-value"
	var gotToken string
	called := false
	err := run(context.Background(), []string{"-config", path}, func(name string) string {
		if name == cfg.OneBot.AccessTokenEnv {
			return token
		}
		return ""
	}, discardLogger(), func(_ context.Context, got config.Config, value string, _ func(string) string, _ *slog.Logger) error {
		called = true
		gotToken = value
		if got.DatabasePath != cfg.DatabasePath {
			t.Fatalf("loaded database path = %q, want %q", got.DatabasePath, cfg.DatabasePath)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called || gotToken != token {
		t.Fatalf("starter called=%v token=%q", called, gotToken)
	}

	for _, args := range [][]string{
		nil,
		{"-config", path, "extra"},
		{"-config", path, "-cleanup-expired"},
		{"-unknown", path},
	} {
		if err := run(context.Background(), args, func(string) string { return token }, discardLogger(), unexpectedStart(t)); err == nil {
			t.Fatalf("run(%q) succeeded, want flag error", args)
		}
	}
}

func TestRunRejectsMissingTokenWithoutLoggingSecrets(t *testing.T) {
	cfg := validConfig(t)
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, path, cfg)

	const secret = "napcat-token-secret"
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	err := run(context.Background(), []string{"-config", path}, func(string) string { return "" }, logger, unexpectedStart(t))
	if err == nil || !strings.Contains(err.Error(), "access token") {
		t.Fatalf("run error = %v, want missing access token", err)
	}
	logRunFailure(logger, errors.New("fatal URL wss://localhost/ws?access_token="+secret))
	if strings.Contains(output.String(), secret) || strings.Contains(output.String(), "access_token=") {
		t.Fatalf("JSON log leaked secret or query: %s", output.String())
	}
}

func unexpectedStart(t *testing.T) starter {
	t.Helper()
	return func(context.Context, config.Config, string, func(string) string, *slog.Logger) error {
		t.Fatal("starter must not be called")
		return nil
	}
}

func validConfig(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	return config.Config{
		OneBot:       config.OneBotConfig{URL: "ws://127.0.0.1:3001", AccessTokenEnv: "NAPCAT_TOKEN", SelfID: "10000", MessageRunes: 1200},
		DatabasePath: filepath.Join(root, "tasks.db"), LogDir: filepath.Join(root, "logs"), WorktreeRoot: filepath.Join(root, "worktrees"), MessageWorkers: 2,
		AllowedGroupIDs: []string{"100"}, EmployeeIDs: []string{"200", "201"}, AdminIDs: []string{"201"},
		Codex:      config.CodexConfig{Binary: "codex"},
		OpsCommand: []string{"/usr/local/bin/qqcodex-ops", "-config", "/etc/qqcodex/ops.json"},
		Projects: []config.Project{{
			ID: "project", Aliases: []string{"p"}, RepoPath: repo, BaseBranch: "main", RCBranch: "rc", Remote: "origin",
			Checks: [][]string{{"go", "test", "./..."}}, DeployAction: "deploy-rc", MaxConcurrent: 1, CodexTimeoutSeconds: 30, LogRetentionDays: 7,
		}},
	}
}

func writeConfig(t *testing.T, path string, cfg config.Config) {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
