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
	"strings"
	"syscall"
	"time"

	"qqcodex/internal/app"
	"qqcodex/internal/auth"
	"qqcodex/internal/codex"
	"qqcodex/internal/config"
	"qqcodex/internal/consultsvc"
	"qqcodex/internal/gitwork"
	"qqcodex/internal/onebot"
	"qqcodex/internal/ops"
	"qqcodex/internal/store"
	"qqcodex/internal/tasklog"
	"qqcodex/internal/tasksvc"
)

type starter func(context.Context, config.Config, string, func(string) string, *slog.Logger, bool) error

const (
	productionSandboxPath         = "/usr/bin/bwrap"
	productionSandboxProbeTimeout = 3 * time.Second
)

type sandboxStartupOptions struct {
	path, root   string
	trustedUID   uint32
	probeTimeout time.Duration
	probe        func(context.Context, string, string, string, string) error
}

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
	configPath, cleanupExpired, err := parseArgs(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	var token string
	if !cleanupExpired {
		token = getenv(cfg.OneBot.AccessTokenEnv)
		if token == "" {
			return errors.New("onebot access token is required")
		}
	}
	return start(ctx, cfg, token, getenv, logger, cleanupExpired)
}

func parseArgs(args []string) (string, bool, error) {
	flags := flag.NewFlagSet("qqcodex", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "")
	cleanupExpired := flags.Bool("cleanup-expired", false, "")
	if err := flags.Parse(args); err != nil || *configPath == "" || flags.NArg() != 0 {
		return "", false, errors.New("usage: qqcodex -config <path> [-cleanup-expired]")
	}
	return *configPath, *cleanupExpired, nil
}

func buildAndRun(ctx context.Context, cfg config.Config, token string, getenv func(string) string, logger *slog.Logger, cleanupExpired bool) error {
	if cleanupExpired {
		return buildAndCleanup(ctx, cfg)
	}
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
	for _, project := range cfg.Projects {
		if err := operator.Preflight(ctx, project.ID, project.Remote, project.BaseBranch, project.RCBranch, project.Checks); err != nil {
			return errors.Join(errors.New("ops project preflight failed"), db.Close())
		}
	}
	runner := newCodexRunner(cfg, logs)
	worktrees := &gitwork.Manager{Root: cfg.WorktreeRoot}
	authorizer := auth.New(cfg.AllowedGroupIDs, cfg.EmployeeIDs, cfg.AdminIDs)
	client := &onebot.Client{URL: cfg.OneBot.URL, Token: token, SelfID: cfg.OneBot.SelfID, MessageRunes: cfg.OneBot.MessageRunes}
	scheduler := tasksvc.NewScheduler(registry, db, runner, worktrees, operator, client, logs, logger, 0)
	service := tasksvc.NewService(registry, db, authorizer, runner, scheduler, client, logs)
	consultations := consultsvc.NewService(registry, cfg.Consultation, db, authorizer, runner, client, logs)
	application := app.App{
		Store: db, Client: client, Scheduler: scheduler, Service: service, Consultations: consultations, Groups: authorizer,
		Projects: registry, Worktrees: worktrees, Logs: logs,
		MessageWorkers: cfg.MessageWorkers, Logger: logger,
	}
	return errors.Join(application.Run(ctx), db.Close())
}

func buildAndCleanup(ctx context.Context, cfg config.Config) error {
	if err := validateCleanupStartup(&cfg); err != nil {
		return err
	}
	registry, err := config.NewRegistry(cfg)
	if err != nil {
		return errors.New("project registry is invalid")
	}
	logs, err := tasklog.Open(cfg.LogDir, nil)
	if err != nil {
		return errors.New("task log directory is invalid")
	}
	db, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return errors.New("sqlite store is invalid")
	}
	application := app.App{
		Store: db, Projects: registry, Worktrees: &gitwork.Manager{Root: cfg.WorktreeRoot}, Logs: logs,
	}
	return errors.Join(application.CleanupExpired(ctx), db.Close())
}

func validateStartup(cfg *config.Config) error {
	return validateStartupWithSandbox(cfg, sandboxStartupOptions{
		path: productionSandboxPath, root: "/", trustedUID: 0,
		probeTimeout: productionSandboxProbeTimeout, probe: probeConsultationSandbox,
	})
}

func validateStartupWithSandbox(cfg *config.Config, sandbox sandboxStartupOptions) error {
	if cfg == nil {
		return errors.New("configuration is required")
	}
	binary, err := resolveCodexExecutable(cfg.Codex.Binary)
	if err != nil {
		return err
	}
	cfg.Codex.Binary = binary
	codeModeHostBinary, err := resolveCodexCodeModeHostExecutable(binary)
	if err != nil {
		return err
	}
	if sandbox.probe == nil || sandbox.probeTimeout <= 0 || validateTrustedSandboxExecutable(sandbox.root, sandbox.path, sandbox.trustedUID) != nil {
		return errors.New("consultation sandbox is invalid")
	}

	if err := validateDatabasePath(cfg.DatabasePath); err != nil {
		return errors.New("database directory is invalid")
	}
	if err := ensurePrivateDirectory(cfg.LogDir); err != nil {
		return errors.New("log directory is invalid")
	}
	if err := ensurePrivateDirectory(cfg.WorktreeRoot); err != nil {
		return errors.New("worktree directory is invalid")
	}
	if err := ensurePrivateDirectory(cfg.Consultation.Workspace); err != nil {
		return errors.New("consultation workspace is invalid")
	}
	for _, project := range cfg.Projects {
		if err := validateProject(project); err != nil {
			return fmt.Errorf("project %q is invalid", project.ID)
		}
	}
	if err := validatePathOverlap(cfg); err != nil {
		return errors.New("configured paths overlap")
	}
	probeCtx, cancel := context.WithTimeout(context.Background(), sandbox.probeTimeout)
	defer cancel()
	if err := sandbox.probe(probeCtx, sandbox.path, cfg.Codex.Binary, codeModeHostBinary, cfg.Consultation.Workspace); err != nil || probeCtx.Err() != nil {
		return errors.New("consultation sandbox probe failed")
	}
	cfg.Consultation.SandboxBinary = sandbox.path
	cfg.Consultation.CodeModeHostBinary = codeModeHostBinary
	return nil
}

func resolveCodexExecutable(name string) (string, error) {
	binary, err := exec.LookPath(name)
	if err != nil {
		return "", errors.New("codex binary is unavailable")
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return "", errors.New("codex binary is invalid")
	}
	info, err := os.Stat(binary)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("codex binary is invalid")
	}
	return binary, nil
}

func resolveCodexCodeModeHostExecutable(codexBinary string) (string, error) {
	binary := filepath.Join(filepath.Dir(codexBinary), "codex-code-mode-host")
	info, err := os.Stat(binary)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("codex code mode host is invalid")
	}
	return binary, nil
}

func validateTrustedSandboxExecutable(root, path string, trustedUID uint32) error {
	root, path = filepath.Clean(root), filepath.Clean(path)
	if !filepath.IsAbs(root) || !filepath.IsAbs(path) || path != filepath.Join(root, "usr", "bin", "bwrap") {
		return errors.New("sandbox path is not absolute")
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("sandbox path is outside trusted root")
	}

	components := append([]string{root}, strings.Split(relative, string(filepath.Separator))...)
	current := components[0]
	for index, component := range components {
		if index > 0 {
			current = filepath.Join(current, component)
		}
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
			return errors.New("sandbox path component is unsafe")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != trustedUID {
			return errors.New("sandbox path component owner is unsafe")
		}
		if index < len(components)-1 {
			if !info.IsDir() {
				return errors.New("sandbox path parent is not a directory")
			}
			continue
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || stat.Nlink != 1 {
			return errors.New("sandbox executable is unsafe")
		}
	}
	return nil
}

func probeConsultationSandbox(ctx context.Context, binary, codexBinary, codeModeHostBinary, workspace string) error {
	configFile, err := os.CreateTemp(workspace, ".sandbox-probe-config-*.toml")
	if err != nil {
		return err
	}
	defer configFile.Close()
	defer os.Remove(configFile.Name())
	if _, err := configFile.WriteString("# qqcodex consultation sandbox probe\n"); err != nil {
		return err
	}
	if _, err := configFile.Seek(0, io.SeekStart); err != nil {
		return err
	}

	args := codex.ConsultationSandboxProbeArgs(codexBinary, codeModeHostBinary, workspace)
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = workspace
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.ExtraFiles = []*os.File{configFile}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	return cmd.Run()
}

func newCodexRunner(cfg config.Config, logs codex.LogSink) *codex.Runner {
	return &codex.Runner{
		Binary: cfg.Codex.Binary, ConsultationSandboxBinary: cfg.Consultation.SandboxBinary, ConsultationCodeModeHostBinary: cfg.Consultation.CodeModeHostBinary,
		KeepEnv: cfg.Codex.EnvironmentKeep, LogDir: cfg.LogDir, Log: logs,
	}
}

func validateCleanupStartup(cfg *config.Config) error {
	if cfg == nil {
		return errors.New("configuration is required")
	}
	if err := validateDatabasePath(cfg.DatabasePath); err != nil {
		return errors.New("database directory is invalid")
	}
	if err := ensurePrivateDirectory(cfg.LogDir); err != nil {
		return errors.New("log directory is invalid")
	}
	if err := ensurePrivateDirectory(cfg.WorktreeRoot); err != nil {
		return errors.New("worktree directory is invalid")
	}
	if err := ensurePrivateDirectory(cfg.Consultation.Workspace); err != nil {
		return errors.New("consultation workspace is invalid")
	}
	for _, project := range cfg.Projects {
		if err := validateRepository(project.RepoPath); err != nil {
			return fmt.Errorf("project %q is invalid", project.ID)
		}
	}
	if err := validatePathOverlap(cfg); err != nil {
		return errors.New("configured paths overlap")
	}
	return nil
}

func ensurePrivateDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
		return errors.New("directory path is unsafe")
	}
	path = filepath.Clean(path)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		parent := filepath.Dir(path)
		if err := validatePrivateExistingDirectory(parent); err != nil {
			return fmt.Errorf("directory parent: %w", err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			return err
		}
		return validatePrivateExistingDirectory(path)
	}
	if err != nil {
		return err
	}
	return validatePrivateInfo(path, info)
}

func validatePrivateExistingDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return validatePrivateInfo(path, info)
}

func validatePrivateInfo(_ string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("path must be a real directory")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if stat.Uid != uint32(os.Geteuid()) || stat.Nlink < 1 {
			return errors.New("directory owner or link count is unsafe")
		}
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("directory permits group or other access")
	}
	return nil
}

func validateDatabasePath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
		return errors.New("database path is unsafe")
	}
	path = filepath.Clean(path)
	if err := validatePrivateExistingDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("database file must be regular and not symlinked")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && (stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1) {
		return errors.New("database file owner or link count is unsafe")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("database file permits group or other access")
	}
	return nil
}

func validateProject(project config.Project) error {
	if !safeGitRemote(project.Remote) {
		return errors.New("configured remote is invalid")
	}
	if err := validateRepository(project.RepoPath); err != nil {
		return err
	}
	if _, err := runGit(project.RepoPath, "remote", "get-url", project.Remote); err != nil {
		return errors.New("configured remote is unavailable")
	}
	if project.BaseBranch == project.RCBranch {
		return errors.New("base and RC branches must differ")
	}
	for _, branch := range []string{project.BaseBranch, project.RCBranch} {
		if !safeBranchName(project.RepoPath, branch) {
			return errors.New("configured branch name is invalid")
		}
		if _, err := runGit(project.RepoPath, "rev-parse", "--verify", "refs/remotes/"+project.Remote+"/"+branch+"^{commit}"); err != nil {
			return errors.New("configured branch ref is unavailable")
		}
	}
	for _, check := range project.Checks {
		if len(check) == 0 || check[0] == "" {
			return errors.New("check command is empty")
		}
		for _, arg := range check {
			if arg == "" || strings.ContainsRune(arg, 0) {
				return errors.New("check command contains an invalid argument")
			}
		}
		if _, err := exec.LookPath(check[0]); err != nil {
			return errors.New("check executable is unavailable")
		}
	}
	return nil
}

func validateRepository(repoPath string) error {
	info, err := os.Lstat(repoPath)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("repository must be a real directory")
	}
	if _, err := os.Stat(filepath.Join(repoPath, ".git")); err != nil {
		return errors.New("repository is not a Git worktree")
	}
	if _, err := runGit(repoPath, "rev-parse", "--show-toplevel"); err != nil {
		return errors.New("repository is not a Git worktree")
	}
	return nil
}

func safeBranchName(repo, branch string) bool {
	if branch == "" || strings.HasPrefix(branch, "-") || strings.ContainsRune(branch, 0) {
		return false
	}
	_, err := runGit(repo, "check-ref-format", "--branch", branch)
	return err == nil
}

func safeGitRemote(remote string) bool {
	if remote == "" || strings.HasPrefix(remote, "-") {
		return false
	}
	for _, char := range remote {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '.' && char != '_' && char != '-' {
			return false
		}
	}
	return true
}

func runGit(repo string, args ...string) ([]byte, error) {
	command := exec.Command("git", append([]string{"-C", repo}, args...)...)
	return command.Output()
}

func validatePathOverlap(cfg *config.Config) error {
	paths := []string{filepath.Dir(cfg.DatabasePath), cfg.LogDir, cfg.WorktreeRoot, cfg.Consultation.Workspace}
	for _, project := range cfg.Projects {
		paths = append(paths, project.RepoPath)
	}
	canonical := make([]string, 0, len(paths))
	for _, path := range paths {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		canonical = append(canonical, filepath.Clean(resolved))
	}
	for i := range canonical {
		for j := i + 1; j < len(canonical); j++ {
			if pathContains(canonical[i], canonical[j]) || pathContains(canonical[j], canonical[i]) {
				return errors.New("paths overlap")
			}
		}
	}
	return nil
}

func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)))
}

func logRunFailure(logger *slog.Logger, err error) {
	class := "service"
	var cleanupErr *app.CleanupError
	if errors.As(err, &cleanupErr) {
		logger.Error("qqcodex stopped", "class", "cleanup", "task_ids", cleanupErr.TaskIDs())
		return
	}
	if errors.Is(err, store.ErrRuntimeLocked) {
		class = "runtime_lock"
	}
	logger.Error("qqcodex stopped", "class", class)
}
