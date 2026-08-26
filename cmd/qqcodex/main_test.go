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
	"slices"
	"strings"
	"testing"
	"time"

	"qqcodex/internal/app"
	"qqcodex/internal/config"
	"qqcodex/internal/model"
	"qqcodex/internal/ops"
	"qqcodex/internal/store"
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
	if err := validateStartupForTest(t, &cfg); err == nil {
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
			if err := validateStartupForTest(t, &cfg); err == nil {
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
		if err := validateStartupForTest(t, &cfg); err == nil {
			t.Fatal("accepted non-Git repository")
		}
	})

	t.Run("missing executable check", func(t *testing.T) {
		cfg, opsPath := validGitConfig(t)
		cfg.Projects[0].Checks = [][]string{{"qqcodex-check-does-not-exist"}}
		cfg.OpsCommand = []string{"/bin/true", "-config", opsPath}
		if err := validateStartupForTest(t, &cfg); err == nil {
			t.Fatal("accepted missing check executable")
		}
	})

	t.Run("missing base ref", func(t *testing.T) {
		cfg, opsPath := validGitConfig(t)
		cfg.OpsCommand = []string{"/bin/true", "-config", opsPath}
		cfg.Projects[0].BaseBranch = "does-not-exist"
		if err := validateStartupForTest(t, &cfg); err == nil {
			t.Fatal("accepted missing base ref")
		}
	})

}

func TestValidateStartupAcceptsValidGitConfig(t *testing.T) {
	cfg, _ := validGitConfig(t)
	cfg.OpsCommand[2] = filepath.Join(t.TempDir(), "worker-must-not-read-ops.json")
	if err := validateStartupForTest(t, &cfg); err != nil {
		t.Fatalf("validateStartup(valid) = %v", err)
	}
}

func TestCleanupStartupIgnoresConsultationSandbox(t *testing.T) {
	cfg, _ := validGitConfig(t)
	cfg.Consultation.SandboxBinary = "/missing/bwrap"
	cfg.Consultation.CodeModeHostBinary = "/missing/codex-code-mode-host"
	if err := validateCleanupStartup(&cfg); err != nil {
		t.Fatalf("cleanup validation required consultation binaries: %v", err)
	}
}

func TestNewCodexRunnerReceivesConsultationSandbox(t *testing.T) {
	cfg := validConfig(t)
	cfg.Codex.Binary = "/opt/codex"
	cfg.Consultation.SandboxBinary = "/usr/bin/bwrap"
	cfg.Consultation.CodeModeHostBinary = "/opt/codex-code-mode-host"
	runner := newCodexRunner(cfg, nil)
	if runner.Binary != cfg.Codex.Binary || runner.ConsultationSandboxBinary != cfg.Consultation.SandboxBinary || runner.ConsultationCodeModeHostBinary != cfg.Consultation.CodeModeHostBinary {
		t.Fatalf("runner binaries = %q/%q/%q, want %q/%q/%q", runner.Binary, runner.ConsultationSandboxBinary, runner.ConsultationCodeModeHostBinary, cfg.Codex.Binary, cfg.Consultation.SandboxBinary, cfg.Consultation.CodeModeHostBinary)
	}
}

func TestValidateStartupCreatesPrivateConsultationWorkspace(t *testing.T) {
	tests := []struct {
		name     string
		validate func(*config.Config) error
	}{
		{name: "runtime", validate: func(cfg *config.Config) error { return validateStartupForTest(t, cfg) }},
		{name: "cleanup", validate: validateCleanupStartup},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, _ := validGitConfig(t)
			workspace := filepath.Join(filepath.Dir(cfg.LogDir), "created-consultation")
			cfg = loadConfigWithConsultation(t, cfg, workspace, 90)

			if err := tt.validate(&cfg); err != nil {
				t.Fatalf("startup validation error = %v", err)
			}
			info, err := os.Stat(workspace)
			if err != nil {
				t.Fatal(err)
			}
			if !info.IsDir() || info.Mode().Perm() != 0o700 {
				t.Fatalf("consultation workspace mode = %v, want private directory mode 0700", info.Mode())
			}
		})
	}
}

func TestValidateStartupRejectsConsultationWorkspaceOverlap(t *testing.T) {
	tests := []struct {
		name      string
		workspace func(config.Config) string
		prepare   func(*config.Config) error
	}{
		{name: "database parent", workspace: func(cfg config.Config) string { return filepath.Dir(cfg.DatabasePath) }},
		{name: "log root", workspace: func(cfg config.Config) string { return cfg.LogDir }},
		{name: "nested under log root", workspace: func(cfg config.Config) string { return filepath.Join(cfg.LogDir, "consultation") }},
		{
			name:      "contains protected roots",
			workspace: func(cfg config.Config) string { return filepath.Dir(cfg.LogDir) },
			prepare: func(cfg *config.Config) error {
				cfg.Projects = nil
				return nil
			},
		},
		{name: "worktree root", workspace: func(cfg config.Config) string { return cfg.WorktreeRoot }},
		{
			name:      "project repository",
			workspace: func(cfg config.Config) string { return cfg.Projects[0].RepoPath },
			prepare: func(cfg *config.Config) error {
				return os.Chmod(cfg.Projects[0].RepoPath, 0o700)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, _ := validGitConfig(t)
			if tt.prepare != nil {
				if err := tt.prepare(&cfg); err != nil {
					t.Fatal(err)
				}
			}
			cfg = loadConfigWithConsultation(t, cfg, tt.workspace(cfg), 90)
			if err := validateStartupForTest(t, &cfg); err == nil || err.Error() != "configured paths overlap" {
				t.Fatalf("validateStartup() error = %v, want configured paths overlap", err)
			}
		})
	}
}

func TestValidateStartupRejectsUnsafeOrDuplicateBranches(t *testing.T) {
	for _, branch := range []string{"", "-bad", "feature..bad", "feature~bad"} {
		t.Run("base "+branch, func(t *testing.T) {
			cfg, _ := validGitConfig(t)
			cfg.Projects[0].BaseBranch = branch
			if err := validateStartupForTest(t, &cfg); err == nil {
				t.Fatalf("accepted unsafe base branch %q", branch)
			}
		})
	}
	t.Run("same branches", func(t *testing.T) {
		cfg, _ := validGitConfig(t)
		cfg.Projects[0].RCBranch = cfg.Projects[0].BaseBranch
		if err := validateStartupForTest(t, &cfg); err == nil {
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

func TestLogRunFailureReportsCleanupTaskIDsWithoutCause(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	project := config.Project{ID: "project", RepoPath: "/private/source", LogRetentionDays: 7}
	for i, id := range []string{"T-000000000402", "T-000000000401"} {
		updated := now.Add(time.Duration(-10+i) * 24 * time.Hour)
		if err := db.CreateTask(context.Background(), &model.Task{
			ID: id, ProjectID: project.ID, GroupID: "1", CreatorID: "2", Requirement: "cleanup",
			Status: model.StatusFailed, Worktree: "/private/worktrees/" + id,
			CreatedAt: updated.Add(-time.Hour), UpdatedAt: updated,
		}); err != nil {
			t.Fatal(err)
		}
	}
	const secret = "cleanup-secret-path-/private/company"
	err = (app.App{
		Store: db,
		Projects: cleanupProjectLookupFunc(func(string) (config.Project, bool) {
			return project, true
		}),
		Worktrees: cleanupWorktreeRemoveFunc(func(context.Context, string, string) error {
			return errors.New(secret)
		}),
		Logs: cleanupLogRemoveFunc(func(string) error {
			t.Fatal("log removal must not follow worktree failure")
			return nil
		}),
		Now: func() time.Time { return now },
	}).CleanupExpired(context.Background())
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "/private") {
		t.Fatalf("unsafe cleanup error = %v", err)
	}

	var output bytes.Buffer
	logRunFailure(slog.New(slog.NewJSONHandler(&output, nil)), errors.Join(err, errors.New("outer-"+secret)))
	if strings.Contains(output.String(), secret) || strings.Contains(output.String(), "/private") {
		t.Fatalf("cleanup log leaked cause or path: %s", output.String())
	}
	var record struct {
		Class   string   `json:"class"`
		TaskIDs []string `json:"task_ids"`
	}
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record.Class != "cleanup" || !slices.Equal(record.TaskIDs, []string{"T-000000000401", "T-000000000402"}) {
		t.Fatalf("cleanup log = %#v", record)
	}
}

func unexpectedStart(t *testing.T) starter {
	t.Helper()
	return func(context.Context, config.Config, string, func(string) string, *slog.Logger, bool) error {
		t.Fatal("starter must not be called")
		return nil
	}
}

type cleanupProjectLookupFunc func(string) (config.Project, bool)

func (f cleanupProjectLookupFunc) ProjectByID(projectID string) (config.Project, bool) {
	return f(projectID)
}

type cleanupWorktreeRemoveFunc func(context.Context, string, string) error

func (f cleanupWorktreeRemoveFunc) Remove(ctx context.Context, repoPath, worktree string) error {
	return f(ctx, repoPath, worktree)
}

type cleanupLogRemoveFunc func(string) error

func (f cleanupLogRemoveFunc) Remove(taskID string) error { return f(taskID) }

func validConfig(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	return config.Config{
		OneBot:       config.OneBotConfig{URL: "ws://127.0.0.1:3001", AccessTokenEnv: "NAPCAT_TOKEN", SelfID: "10000", MessageRunes: 1200},
		DatabasePath: filepath.Join(root, "tasks.db"), LogDir: filepath.Join(root, "logs"), WorktreeRoot: filepath.Join(root, "worktrees"), Consultation: config.ConsultationConfig{Workspace: filepath.Join(root, "consultation"), TimeoutSeconds: 90}, MessageWorkers: 2,
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

func loadConfigWithConsultation(t *testing.T, cfg config.Config, workspace string, timeoutSeconds int) config.Config {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	document["consultation"] = map[string]any{
		"workspace":       workspace,
		"timeout_seconds": timeoutSeconds,
	}
	data, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "qqcodex.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func validGitConfig(t *testing.T) (config.Config, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
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
	for _, dir := range []string{state, filepath.Join(root, "logs"), filepath.Join(root, "worktrees"), filepath.Join(root, "consultation")} {
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
	codexBinary := filepath.Join(root, "codex")
	writeExecutable(t, codexBinary)
	writeExecutable(t, filepath.Join(root, "codex-code-mode-host"))
	return config.Config{
		OneBot:       config.OneBotConfig{URL: "ws://127.0.0.1:3001", AccessTokenEnv: "TOKEN", SelfID: "10000", MessageRunes: 1200},
		DatabasePath: filepath.Join(state, "tasks.db"), LogDir: filepath.Join(root, "logs"), WorktreeRoot: filepath.Join(root, "worktrees"), Consultation: config.ConsultationConfig{Workspace: filepath.Join(root, "consultation"), TimeoutSeconds: 90}, MessageWorkers: 1,
		Codex: config.CodexConfig{Binary: codexBinary}, OpsCommand: []string{"/bin/true", "-config", opsPath},
		Projects: []config.Project{{ID: "project", Aliases: []string{"p"}, RepoPath: repo, BaseBranch: "main", RCBranch: "rc", Remote: "origin", Checks: [][]string{{"/bin/sh", "-c", "true"}}, DeployAction: "deploy", MaxConcurrent: 1, CodexTimeoutSeconds: 30, LogRetentionDays: 7}},
	}, opsPath
}

func runCmd(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	return cmd.Run()
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
