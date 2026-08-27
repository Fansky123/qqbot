package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"qqcodex/internal/config"
	"qqcodex/internal/onebot"
)

func TestRunRequiresExactConfigFlag(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		nil,
		{"-config"},
		{"--config", "config.json"},
		{"-config=config.json"},
		{"-config", "config.json", "extra"},
		{"-unknown", "config.json"},
	} {
		var stdout bytes.Buffer
		err := run(context.Background(), args, func(string) string { return "token" }, &stdout, unexpectedProbe(t))
		if err == nil || err.Error() != "usage: qqcodex-probe -config <path>" {
			t.Fatalf("run(%q) error = %v, want usage error", args, err)
		}
		if stdout.Len() != 0 {
			t.Fatalf("run(%q) wrote %q on error", args, stdout.String())
		}
	}
}

func TestRunProbesConfiguredAccountAndReportsMissingGroups(t *testing.T) {
	t.Parallel()

	cfg := validProbeConfig(t)
	cfg.AllowedGroupIDs = []string{"2163011680", "987654321", "987654321"}
	path := writeProbeConfig(t, cfg)
	const token = "probe-token"
	var stdout bytes.Buffer
	called := false
	err := run(context.Background(), []string{"-config", path}, func(name string) string {
		if name != cfg.OneBot.AccessTokenEnv {
			t.Fatalf("getenv(%q), want %q", name, cfg.OneBot.AccessTokenEnv)
		}
		return token
	}, &stdout, func(ctx context.Context, url, gotToken string) (onebot.Account, error) {
		called = true
		if ctx == nil {
			t.Fatal("probe received nil context")
		}
		if url != cfg.OneBot.URL || gotToken != token {
			t.Fatalf("probe(url, token) = (%q, %q), want (%q, %q)", url, gotToken, cfg.OneBot.URL, token)
		}
		return onebot.Account{
			SelfID:   onebot.ID("123456789"),
			Nickname: "NapCat",
			GroupIDs: []onebot.ID{"2163011680", "2163011680"},
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("probe was not called")
	}

	var output probeOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	want := probeOutput{
		SelfID:          "123456789",
		Nickname:        "NapCat",
		GroupIDs:        []string{"2163011680", "2163011680"},
		MissingGroupIDs: []string{"987654321"},
	}
	if !reflect.DeepEqual(output, want) {
		t.Fatalf("output = %#v, want %#v", output, want)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sortedKeys(fields), []string{"group_ids", "missing_group_ids", "nickname", "self_id"}) {
		t.Fatalf("output fields = %v", sortedKeys(fields))
	}
	for _, forbidden := range []string{"config", "project", "repository", "employee", "admin", "codex", "ops", "token"} {
		if _, exists := fields[forbidden]; exists {
			t.Fatalf("output exposed forbidden field %q", forbidden)
		}
	}
}

func TestRunRejectsMissingOrEmptyToken(t *testing.T) {
	t.Parallel()

	path := writeProbeConfig(t, validProbeConfig(t))
	for _, token := range []string{""} {
		var stdout bytes.Buffer
		err := run(context.Background(), []string{"-config", path}, func(string) string { return token }, &stdout, unexpectedProbe(t))
		if err == nil || err.Error() != "onebot access token is required" {
			t.Fatalf("run token %q error = %v", token, err)
		}
		if stdout.Len() != 0 {
			t.Fatalf("run token %q wrote %q", token, stdout.String())
		}
	}
}

func TestRunSanitizesProbeErrors(t *testing.T) {
	t.Parallel()

	const token = "probe-token-must-not-leak"
	var stdout bytes.Buffer
	err := run(context.Background(), []string{"-config", writeProbeConfig(t, validProbeConfig(t))}, func(string) string { return token }, &stdout, func(context.Context, string, string) (onebot.Account, error) {
		return onebot.Account{}, errors.New("upstream included " + token)
	})
	if err == nil || err.Error() != "probe OneBot account failed" {
		t.Fatalf("run error = %v", err)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(stdout.String(), token) {
		t.Fatalf("run exposed token: error=%q stdout=%q", err, stdout.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("run wrote partial JSON: %q", stdout.String())
	}
}

func TestRunFailsSafelyForMalformedConfigAndCanceledContext(t *testing.T) {
	t.Parallel()

	malformed := filepath.Join(t.TempDir(), "malformed.json")
	if err := os.WriteFile(malformed, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		ctx    context.Context
		path   string
		getenv func(string) string
	}{
		{name: "malformed config", ctx: context.Background(), path: malformed, getenv: func(string) string { return "token" }},
		{name: "canceled context", ctx: canceledContext(), path: writeProbeConfig(t, validProbeConfig(t)), getenv: func(name string) string {
			t.Fatalf("canceled run read environment variable %q", name)
			return ""
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			err := run(test.ctx, []string{"-config", test.path}, test.getenv, &stdout, unexpectedProbe(t))
			if err == nil {
				t.Fatal("run succeeded")
			}
			if stdout.Len() != 0 {
				t.Fatalf("run wrote %q on error", stdout.String())
			}
		})
	}
}

func TestRunEmitsEmptyArrays(t *testing.T) {
	t.Parallel()

	cfg := validProbeConfig(t)
	cfg.AllowedGroupIDs = []string{}
	var stdout bytes.Buffer
	err := run(context.Background(), []string{"-config", writeProbeConfig(t, cfg)}, func(string) string { return "token" }, &stdout, func(context.Context, string, string) (onebot.Account, error) {
		return onebot.Account{SelfID: onebot.ID("1"), Nickname: "NapCat", GroupIDs: nil}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "null") {
		t.Fatalf("output has null arrays: %q", stdout.String())
	}
	var output probeOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if output.GroupIDs == nil || output.MissingGroupIDs == nil || len(output.GroupIDs) != 0 || len(output.MissingGroupIDs) != 0 {
		t.Fatalf("output arrays = %#v", output)
	}
}

func unexpectedProbe(t *testing.T) prober {
	t.Helper()
	return func(context.Context, string, string) (onebot.Account, error) {
		t.Fatal("probe must not be called")
		return onebot.Account{}, nil
	}
}

func validProbeConfig(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	return config.Config{
		OneBot:          config.OneBotConfig{URL: "ws://127.0.0.1:3001", AccessTokenEnv: "NAPCAT_TOKEN", SelfID: "auto", MessageRunes: 1200},
		DatabasePath:    filepath.Join(root, "tasks.db"),
		LogDir:          filepath.Join(root, "logs"),
		WorktreeRoot:    filepath.Join(root, "worktrees"),
		Consultation:    config.ConsultationConfig{Workspace: filepath.Join(root, "consultation"), TimeoutSeconds: 90},
		MessageWorkers:  1,
		AllowedGroupIDs: []string{"2163011680"},
		EmployeeIDs:     []string{"100"},
		AdminIDs:        []string{"100"},
		Codex:           config.CodexConfig{Binary: "codex"},
		OpsCommand:      []string{"/usr/local/bin/qqcodex-ops", "-config", "/etc/qqcodex/ops.json"},
		Projects: []config.Project{{
			ID: "project", Aliases: []string{"project"}, RepoPath: filepath.Join(root, "repo"), BaseBranch: "main", RCBranch: "rc", Remote: "origin",
			Checks: [][]string{{"go", "test", "./..."}}, DeployAction: "deploy", MaxConcurrent: 1, CodexTimeoutSeconds: 60, LogRetentionDays: 7,
		}},
	}
}

func writeProbeConfig(t *testing.T, cfg config.Config) string {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "qqcodex.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func sortedKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
