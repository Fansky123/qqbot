package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"qqcodex/internal/config"
	"qqcodex/internal/ops"
)

func TestValidateStartupDoesNotChangeExistingPrivateDirectoryMode(t *testing.T) {
	cfg := validConfig(t)
	if err := os.Mkdir(cfg.LogDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cfg.WorktreeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(cfg.LogDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateStartup(&cfg); err == nil {
		t.Fatal("validateStartup accepted group-readable log directory")
	}
	after, err := os.Stat(cfg.LogDir)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode().Perm() != after.Mode().Perm() {
		t.Fatalf("existing log mode changed from %o to %o", before.Mode().Perm(), after.Mode().Perm())
	}
}

func TestValidateStartupRejectsRootAndSymlinkAndOverlap(t *testing.T) {
	cases := []struct {
		name string
		edit func(*config.Config, string)
	}{
		{"root log", func(cfg *config.Config, _ string) { cfg.LogDir = "/" }},
		{"symlink log", func(cfg *config.Config, root string) {
			cfg.LogDir = filepath.Join(root, "log-link")
			if err := os.Symlink(filepath.Join(root, "real-log"), cfg.LogDir); err != nil {
				panic(err)
			}
		}},
		{"database parent overlaps repository", func(cfg *config.Config, _ string) {
			cfg.DatabasePath = filepath.Join(cfg.Projects[0].RepoPath, "tasks.db")
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			cfg := validConfig(t)
			root := filepath.Dir(cfg.DatabasePath)
			if test.name == "symlink log" {
				if err := os.Mkdir(filepath.Join(root, "real-log"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			test.edit(&cfg, root)
			if err := validateStartup(&cfg); err == nil {
				t.Fatal("validateStartup accepted unsafe path")
			}
		})
	}
}

func TestValidateStartupRejectsNonGitMissingRefCheckAndOpsMismatch(t *testing.T) {
	t.Run("non git repository", func(t *testing.T) {
		cfg := validConfig(t)
		if err := os.Mkdir(cfg.LogDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(cfg.WorktreeRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := validateStartup(&cfg); err == nil {
			t.Fatal("accepted non-Git repository")
		}
	})

	t.Run("missing executable check", func(t *testing.T) {
		cfg, opsPath := validGitConfig(t)
		cfg.Projects[0].Checks = [][]string{{"qqcodex-check-does-not-exist"}}
		cfg.OpsCommand = []string{"/bin/true", "-config", opsPath}
		if err := validateStartup(&cfg); err == nil {
			t.Fatal("accepted missing check executable")
		}
	})

	t.Run("missing base ref", func(t *testing.T) {
		cfg, opsPath := validGitConfig(t)
		cfg.OpsCommand = []string{"/bin/true", "-config", opsPath}
		cfg.Projects[0].BaseBranch = "does-not-exist"
		if err := validateStartup(&cfg); err == nil {
			t.Fatal("accepted missing base ref")
		}
	})

}

func TestValidateStartupAcceptsValidGitConfig(t *testing.T) {
	cfg, _ := validGitConfig(t)
	cfg.OpsCommand[2] = filepath.Join(t.TempDir(), "worker-must-not-read-ops.json")
	if err := validateStartup(&cfg); err != nil {
		t.Fatalf("validateStartup(valid) = %v", err)
	}
}

func TestValidateStartupRejectsUnsafeOrDuplicateBranches(t *testing.T) {
	for _, branch := range []string{"", "-bad", "feature..bad", "feature~bad"} {
		t.Run("base "+branch, func(t *testing.T) {
			cfg, _ := validGitConfig(t)
			cfg.Projects[0].BaseBranch = branch
			if err := validateStartup(&cfg); err == nil {
				t.Fatalf("accepted unsafe base branch %q", branch)
			}
		})
	}
	t.Run("same branches", func(t *testing.T) {
		cfg, _ := validGitConfig(t)
		cfg.Projects[0].RCBranch = cfg.Projects[0].BaseBranch
		if err := validateStartup(&cfg); err == nil {
			t.Fatal("accepted identical base and RC branches")
		}
	})
}

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
	}, discardLogger(), func(_ context.Context, got config.Config, value string, _ func(string) string, _ *slog.Logger, cleanupExpired bool) error {
		called = true
		gotToken = value
		if cleanupExpired {
			t.Fatal("normal run selected cleanup mode")
		}
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
		{"-unknown", path},
	} {
		if err := run(context.Background(), args, func(string) string { return token }, discardLogger(), unexpectedStart(t)); err == nil {
			t.Fatalf("run(%q) succeeded, want flag error", args)
		}
	}
}

func TestRunCleanupDoesNotReadCredentials(t *testing.T) {
	cfg := validConfig(t)
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, path, cfg)

	called := false
	err := run(context.Background(), []string{"-config", path, "-cleanup-expired"}, func(name string) string {
		t.Fatalf("cleanup read credential environment variable %q", name)
		return ""
	}, discardLogger(), func(_ context.Context, _ config.Config, token string, _ func(string) string, _ *slog.Logger, cleanupExpired bool) error {
		called = true
		if !cleanupExpired || token != "" {
			t.Fatalf("cleanup=%v token=%q, want cleanup with empty token", cleanupExpired, token)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("cleanup starter was not called")
	}
}

func TestBuildAndRunCleanupDoesNotStartRuntimeDependencies(t *testing.T) {
	cfg, _ := validGitConfig(t)
	cfg.Codex.Binary = "qqcodex-codex-must-not-run"
	cfg.OpsCommand = []string{"qqcodex-ops-must-not-run"}

	err := buildAndRun(context.Background(), cfg, "", func(name string) string {
		t.Fatalf("cleanup read credential environment variable %q", name)
		return ""
	}, discardLogger(), true)
	if err != nil {
		t.Fatal(err)
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
	return func(context.Context, config.Config, string, func(string) string, *slog.Logger, bool) error {
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

func validGitConfig(t *testing.T) (config.Config, string) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	remote := filepath.Join(root, "remote.git")
	if err := runCmd(root, "git", "init", "--bare", remote); err != nil {
		t.Fatal(err)
	}
	seed := filepath.Join(root, "seed")
	if err := runCmd(root, "git", "init", "-b", "main", seed); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"config", "user.name", "qqcodex-test"}, {"config", "user.email", "test@example.invalid"}} {
		if err := runCmd(seed, "git", args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(seed, "README"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "README"}, {"commit", "-m", "base"}, {"remote", "add", "origin", remote}, {"push", "-u", "origin", "main"}, {"branch", "rc"}, {"push", "origin", "rc"}} {
		if err := runCmd(seed, "git", args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := runCmd(root, "git", "clone", remote, repo); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(repo, "git", "fetch", "origin", "main", "rc"); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(repo, "git", "switch", "-c", "main", "origin/main"); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state")
	for _, dir := range []string{state, filepath.Join(root, "logs"), filepath.Join(root, "worktrees")} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	opsPath := filepath.Join(root, "ops.json")
	opsCfg := ops.Config{GitBinary: "/usr/bin/git", Projects: map[string]ops.Project{
		"project": {RepoPath: repo, Remote: "origin", RemoteURL: remote, AllowLocalRemote: true, BaseBranch: "main", RCBranch: "rc", CheckRunner: []string{"/bin/true"}, Checks: [][]string{{"/bin/sh", "-c", "true"}}, DeployAction: []string{"/bin/true"}},
	}}
	data, err := json.Marshal(opsCfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opsPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Config{
		OneBot:       config.OneBotConfig{URL: "ws://127.0.0.1:3001", AccessTokenEnv: "TOKEN", SelfID: "10000", MessageRunes: 1200},
		DatabasePath: filepath.Join(state, "tasks.db"), LogDir: filepath.Join(root, "logs"), WorktreeRoot: filepath.Join(root, "worktrees"), MessageWorkers: 1,
		Codex: config.CodexConfig{Binary: "/bin/sh"}, OpsCommand: []string{"/bin/true", "-config", opsPath},
		Projects: []config.Project{{ID: "project", Aliases: []string{"p"}, RepoPath: repo, BaseBranch: "main", RCBranch: "rc", Remote: "origin", Checks: [][]string{{"/bin/sh", "-c", "true"}}, DeployAction: "deploy", MaxConcurrent: 1, CodexTimeoutSeconds: 30, LogRetentionDays: 7}},
	}, opsPath
}

func runCmd(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	return cmd.Run()
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
