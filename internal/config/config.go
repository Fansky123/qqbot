package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

type Config struct {
	OneBot          OneBotConfig `json:"onebot"`
	DatabasePath    string       `json:"database_path"`
	LogDir          string       `json:"log_dir"`
	WorktreeRoot    string       `json:"worktree_root"`
	MessageWorkers  int          `json:"message_workers"`
	AllowedGroupIDs []string     `json:"allowed_group_ids"`
	EmployeeIDs     []string     `json:"employee_ids"`
	AdminIDs        []string     `json:"admin_ids"`
	Codex           CodexConfig  `json:"codex"`
	OpsCommand      []string     `json:"ops_command"`
	Projects        []Project    `json:"projects"`
}

type OneBotConfig struct {
	URL            string `json:"url"`
	AccessTokenEnv string `json:"access_token_env"`
	SelfID         string `json:"self_id"`
	MessageRunes   int    `json:"message_runes"`
}

type CodexConfig struct {
	Binary          string   `json:"binary"`
	EnvironmentKeep []string `json:"environment_keep"`
}

type Project struct {
	ID                  string     `json:"id"`
	Aliases             []string   `json:"aliases"`
	RepoPath            string     `json:"repo_path"`
	BaseBranch          string     `json:"base_branch"`
	RCBranch            string     `json:"rc_branch"`
	Remote              string     `json:"remote"`
	Checks              [][]string `json:"checks"`
	DeployAction        string     `json:"deploy_action"`
	MaxConcurrent       int        `json:"max_concurrent"`
	CodexTimeoutSeconds int        `json:"codex_timeout_seconds"`
	LogRetentionDays    int        `json:"log_retention_days"`
}

// Load decodes and validates the configuration file.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	if cfg.OneBot.MessageRunes == 0 {
		cfg.OneBot.MessageRunes = 1200
	}
	if cfg.MessageWorkers == 0 {
		cfg.MessageWorkers = 4
	}
	if err := Validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate verifies configuration required to run the service without changing it.
func Validate(cfg Config) error {
	if cfg.MessageWorkers <= 0 {
		return fmt.Errorf("message workers must be positive")
	}
	if err := requireAbsolutePath("database path", cfg.DatabasePath); err != nil {
		return err
	}
	if err := requireAbsolutePath("log directory", cfg.LogDir); err != nil {
		return err
	}
	if err := requireAbsolutePath("worktree root", cfg.WorktreeRoot); err != nil {
		return err
	}
	if cfg.OneBot.URL == "" || cfg.OneBot.AccessTokenEnv == "" || cfg.OneBot.SelfID == "" {
		return fmt.Errorf("onebot URL, access token environment variable, and self ID are required")
	}
	if cfg.Codex.Binary == "" {
		return fmt.Errorf("codex binary is required")
	}
	if err := validateArgv("ops command", cfg.OpsCommand); err != nil {
		return err
	}

	employees := make(map[string]struct{}, len(cfg.EmployeeIDs))
	for _, employeeID := range cfg.EmployeeIDs {
		employees[employeeID] = struct{}{}
	}
	for _, adminID := range cfg.AdminIDs {
		if _, ok := employees[adminID]; !ok {
			return fmt.Errorf("admin %q is not an employee", adminID)
		}
	}

	projectIDs := make(map[string]struct{}, len(cfg.Projects))
	aliases := make(map[string]string)
	for _, project := range cfg.Projects {
		if project.ID == "" {
			return fmt.Errorf("project ID is required")
		}
		if _, ok := projectIDs[project.ID]; ok {
			return fmt.Errorf("duplicate project ID %q", project.ID)
		}
		projectIDs[project.ID] = struct{}{}
		if len(project.Aliases) == 0 {
			return fmt.Errorf("project %q has no aliases", project.ID)
		}
		for _, alias := range project.Aliases {
			if err := validateAlias(alias); err != nil {
				return fmt.Errorf("project %q: %w", project.ID, err)
			}
			if previous, ok := aliases[alias]; ok {
				return fmt.Errorf("alias %q is used by both %q and %q", alias, previous, project.ID)
			}
			aliases[alias] = project.ID
		}
		if err := requireAbsolutePath("project "+project.ID+" repository path", project.RepoPath); err != nil {
			return err
		}
		if len(project.Checks) == 0 {
			return fmt.Errorf("project %q has no checks", project.ID)
		}
		for _, check := range project.Checks {
			if err := validateArgv("project "+project.ID+" check", check); err != nil {
				return err
			}
		}
		if project.MaxConcurrent <= 0 {
			return fmt.Errorf("project %q maximum concurrency must be positive", project.ID)
		}
		if project.CodexTimeoutSeconds <= 0 {
			return fmt.Errorf("project %q codex timeout must be positive", project.ID)
		}
		if project.LogRetentionDays <= 0 {
			return fmt.Errorf("project %q log retention must be positive", project.ID)
		}
	}
	return nil
}

type Registry struct {
	projects map[string]Project
}

func NewRegistry(cfg Config) (*Registry, error) {
	if err := Validate(cfg); err != nil {
		return nil, err
	}

	projects := make(map[string]Project, len(cfg.Projects))
	for _, project := range cfg.Projects {
		project = cloneProject(project)
		for _, alias := range project.Aliases {
			projects[alias] = project
		}
	}
	return &Registry{projects: projects}, nil
}

// Project looks up an alias and returns an independent copy of its configuration.
func (r *Registry) Project(alias string) (Project, bool) {
	project, ok := r.projects[alias]
	if !ok {
		return Project{}, false
	}
	return cloneProject(project), true
}

func requireAbsolutePath(name, path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s must be an absolute path", name)
	}
	return nil
}

func validateArgv(name string, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("%s must contain an executable", name)
	}
	for _, arg := range argv {
		if arg == "" {
			return fmt.Errorf("%s contains an empty argument", name)
		}
	}
	return nil
}

func validateAlias(alias string) error {
	if alias == "" {
		return fmt.Errorf("alias is empty")
	}
	if strings.TrimSpace(alias) != alias {
		return fmt.Errorf("alias %q has surrounding whitespace", alias)
	}
	for _, r := range alias {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' {
			return fmt.Errorf("alias %q contains an invalid character", alias)
		}
	}
	return nil
}

func cloneProject(project Project) Project {
	project.Aliases = append([]string(nil), project.Aliases...)
	checks := project.Checks
	project.Checks = make([][]string, len(project.Checks))
	for i, check := range checks {
		project.Checks[i] = append([]string(nil), check...)
	}
	return project
}
