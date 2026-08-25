package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{name: "valid configuration"},
		{
			name: "duplicate project IDs",
			mutate: func(cfg *Config) {
				second := cfg.Projects[0]
				second.Aliases = []string{"second"}
				cfg.Projects = append(cfg.Projects, second)
			},
			wantErr: true,
		},
		{
			name: "duplicate aliases",
			mutate: func(cfg *Config) {
				second := cfg.Projects[0]
				second.ID = "second-api"
				second.Aliases = []string{"orders"}
				cfg.Projects = append(cfg.Projects, second)
			},
			wantErr: true,
		},
		{
			name: "empty project ID",
			mutate: func(cfg *Config) {
				cfg.Projects[0].ID = ""
			},
			wantErr: true,
		},
		{
			name: "project without aliases",
			mutate: func(cfg *Config) {
				cfg.Projects[0].Aliases = nil
			},
			wantErr: true,
		},
		{
			name: "relative repository path",
			mutate: func(cfg *Config) {
				cfg.Projects[0].RepoPath = "order-api"
			},
			wantErr: true,
		},
		{
			name: "empty checks",
			mutate: func(cfg *Config) {
				cfg.Projects[0].Checks = nil
			},
			wantErr: true,
		},
		{
			name: "empty check argv",
			mutate: func(cfg *Config) {
				cfg.Projects[0].Checks = [][]string{{}}
			},
			wantErr: true,
		},
		{
			name: "empty check executable",
			mutate: func(cfg *Config) {
				cfg.Projects[0].Checks = [][]string{{"", "test"}}
			},
			wantErr: true,
		},
		{
			name: "admin absent from employees",
			mutate: func(cfg *Config) {
				cfg.AdminIDs = []string{"99999"}
			},
			wantErr: true,
		},
		{
			name: "non-positive message workers",
			mutate: func(cfg *Config) {
				cfg.MessageWorkers = 0
			},
			wantErr: true,
		},
		{
			name: "non-positive concurrency",
			mutate: func(cfg *Config) {
				cfg.Projects[0].MaxConcurrent = 0
			},
			wantErr: true,
		},
		{
			name: "non-positive timeout",
			mutate: func(cfg *Config) {
				cfg.Projects[0].CodexTimeoutSeconds = 0
			},
			wantErr: true,
		},
		{
			name: "non-positive retention",
			mutate: func(cfg *Config) {
				cfg.Projects[0].LogRetentionDays = 0
			},
			wantErr: true,
		},
		{
			name: "malformed aliases",
			mutate: func(cfg *Config) {
				cfg.Projects[0].Aliases = []string{" orders"}
			},
			wantErr: true,
		},
		{
			name: "invalid alias characters",
			mutate: func(cfg *Config) {
				cfg.Projects[0].Aliases = []string{"orders!"}
			},
			wantErr: true,
		},
		{
			name: "empty ops executable",
			mutate: func(cfg *Config) {
				cfg.OpsCommand[0] = ""
			},
			wantErr: true,
		},
		{
			name: "empty ops argument",
			mutate: func(cfg *Config) {
				cfg.OpsCommand[1] = ""
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := validConfig(t)
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}
			if err := Validate(cfg); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, want error: %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadAppliesDefaultsOnlyDuringLoad(t *testing.T) {
	t.Parallel()

	cfg := validConfig(t)
	cfg.OneBot.MessageRunes = 0
	cfg.MessageWorkers = 0

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "qqcodex.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.OneBot.MessageRunes != 1200 {
		t.Errorf("MessageRunes = %d, want 1200", loaded.OneBot.MessageRunes)
	}
	if loaded.MessageWorkers != 4 {
		t.Errorf("MessageWorkers = %d, want 4", loaded.MessageWorkers)
	}
	if err := Validate(cfg); err == nil {
		t.Fatal("Validate() error = nil for zero MessageWorkers, want error")
	}
	if _, err := NewRegistry(cfg); err == nil {
		t.Fatal("NewRegistry() error = nil for zero MessageWorkers, want error")
	}
}

func TestRegistryResolvesAliasesAndReturnsCopies(t *testing.T) {
	t.Parallel()

	cfg := validConfig(t)
	registry, err := NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}

	cfg.Projects[0].Aliases[0] = "changed-in-source"
	cfg.Projects[0].Checks[0][0] = "changed-in-source"

	a, ok := registry.Project("orders")
	if !ok || a.ID != "order-api" || a.Aliases[0] != "orders" || a.Checks[0][0] != "go" {
		t.Fatalf("orders project = %#v, %v", a, ok)
	}
	b, ok := registry.Project("订单")
	if !ok || b.ID != a.ID {
		t.Fatalf("订单 project = %#v, %v", b, ok)
	}

	a.Aliases[0] = "changed"
	a.Checks[0][0] = "changed"
	again, ok := registry.Project("orders")
	if !ok || again.Aliases[0] != "orders" || again.Checks[0][0] != "go" {
		t.Fatalf("registry project was mutable: %#v, %v", again, ok)
	}
}

func validConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	return Config{
		OneBot:          OneBotConfig{URL: "ws://127.0.0.1:3001", AccessTokenEnv: "NAPCAT_ACCESS_TOKEN", SelfID: "10000", MessageRunes: 1200},
		DatabasePath:    filepath.Join(root, "tasks.db"),
		LogDir:          filepath.Join(root, "logs"),
		WorktreeRoot:    filepath.Join(root, "worktrees"),
		MessageWorkers:  4,
		AllowedGroupIDs: []string{"20000"},
		EmployeeIDs:     []string{"30000", "30001"},
		AdminIDs:        []string{"30001"},
		Codex:           CodexConfig{Binary: "codex"},
		OpsCommand:      []string{"qqcodex-ops", "-config", filepath.Join(root, "ops.json")},
		Projects: []Project{{
			ID: "order-api", Aliases: []string{"orders", "订单"}, RepoPath: filepath.Join(root, "order-api"),
			BaseBranch: "main", RCBranch: "rc", Remote: "origin", Checks: [][]string{{"go", "test", "./..."}},
			DeployAction: "order-rc", MaxConcurrent: 2, CodexTimeoutSeconds: 1800, LogRetentionDays: 7,
		}},
	}
}
