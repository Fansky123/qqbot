package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"qqcodex/internal/app"
	"qqcodex/internal/auth"
	"qqcodex/internal/codex"
	"qqcodex/internal/config"
	"qqcodex/internal/gitwork"
	"qqcodex/internal/onebot"
	"qqcodex/internal/ops"
	"qqcodex/internal/store"
	"qqcodex/internal/tasklog"
	"qqcodex/internal/tasksvc"
)

type starter func(context.Context, config.Config, string, func(string) string, *slog.Logger) error

func main() {
	os.Exit(mainCode())
}

func mainCode() int {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv, logger, buildAndRun); err != nil {
		logRunFailure(logger, err)
		return 1
	}
	return 0
}

func run(ctx context.Context, args []string, getenv func(string) string, logger *slog.Logger, start starter) error {
	if ctx == nil || getenv == nil || logger == nil || start == nil {
		return errors.New("main dependencies are required")
	}
	configPath, err := parseArgs(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	token := getenv(cfg.OneBot.AccessTokenEnv)
	if token == "" {
		return errors.New("onebot access token is required")
	}
	return start(ctx, cfg, token, getenv, logger)
}

func parseArgs(args []string) (string, error) {
	flags := flag.NewFlagSet("qqcodex", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "")
	if err := flags.Parse(args); err != nil || *configPath == "" || flags.NArg() != 0 {
		return "", errors.New("usage: qqcodex -config <path>")
	}
	return *configPath, nil
}

func buildAndRun(ctx context.Context, cfg config.Config, token string, getenv func(string) string, logger *slog.Logger) error {
	if err := validateStartup(&cfg); err != nil {
		return err
	}
	registry, err := config.NewRegistry(cfg)
	if err != nil {
		return errors.New("project registry is invalid")
	}

	apiKey := getenv("CODEX_API_KEY")
	if apiKey == "" {
		return errors.New("CODEX_API_KEY is required")
	}
	secrets := []string{token, apiKey}
	if openAIKey := getenv("OPENAI_API_KEY"); openAIKey != "" {
		secrets = append(secrets, openAIKey)
	}
	logs, err := tasklog.Open(cfg.LogDir, secrets)
	if err != nil {
		return errors.New("task log directory is invalid")
	}
	db, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return errors.New("sqlite store is invalid")
	}

	sourceRepos := make(map[string]string, len(cfg.Projects))
	for _, project := range cfg.Projects {
		sourceRepos[project.ID] = project.RepoPath
	}
	operator, err := ops.NewClient(cfg.OpsCommand, sourceRepos, logs)
	if err != nil {
		return errors.Join(errors.New("ops command or repositories are invalid"), db.Close())
	}
	runner := &codex.Runner{Binary: cfg.Codex.Binary, KeepEnv: cfg.Codex.EnvironmentKeep, LogDir: cfg.LogDir, Log: logs}
	worktrees := &gitwork.Manager{Root: cfg.WorktreeRoot}
	authorizer := auth.New(cfg.AllowedGroupIDs, cfg.EmployeeIDs, cfg.AdminIDs)
	client := &onebot.Client{URL: cfg.OneBot.URL, Token: token, SelfID: cfg.OneBot.SelfID, MessageRunes: cfg.OneBot.MessageRunes}
	scheduler := tasksvc.NewScheduler(registry, db, runner, worktrees, operator, client, logs, logger, 0)
	service := tasksvc.NewService(registry, db, authorizer, runner, scheduler, client, logs)
	application := app.App{
		Store: db, Client: client, Scheduler: scheduler, Service: service, Groups: authorizer,
		MessageWorkers: cfg.MessageWorkers, Logger: logger,
	}
	return errors.Join(application.Run(ctx), db.Close())
}

func validateStartup(cfg *config.Config) error {
	binary, err := exec.LookPath(cfg.Codex.Binary)
	if err != nil {
		return errors.New("codex binary is unavailable")
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return errors.New("codex binary is invalid")
	}
	info, err := os.Stat(binary)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return errors.New("codex binary is invalid")
	}
	cfg.Codex.Binary = binary

	if err := requireDirectory(filepath.Dir(cfg.DatabasePath)); err != nil {
		return errors.New("database directory is invalid")
	}
	if err := ensurePrivateDirectory(cfg.LogDir); err != nil {
		return errors.New("log directory is invalid")
	}
	if err := ensurePrivateDirectory(cfg.WorktreeRoot); err != nil {
		return errors.New("worktree directory is invalid")
	}
	for _, project := range cfg.Projects {
		if err := requireDirectory(project.RepoPath); err != nil {
			return errors.New("project repository is invalid")
		}
	}
	return nil
}

func requireDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("path is not a directory")
	}
	return nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	return requireDirectory(path)
}

func logRunFailure(logger *slog.Logger, err error) {
	class := "service"
	if errors.Is(err, store.ErrRuntimeLocked) {
		class = "runtime_lock"
	}
	logger.Error("qqcodex stopped", "class", class)
}
