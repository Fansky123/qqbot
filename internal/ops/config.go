package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

var (
	projectIDPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	remotePattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	remotePathPattern = regexp.MustCompile(`^/[A-Za-z0-9._~/-]+$`)
)

type Config struct {
	GitBinary string             `json:"git_binary"`
	Projects  map[string]Project `json:"projects"`
}

type Project struct {
	RepoPath         string     `json:"repo_path"`
	Remote           string     `json:"remote"`
	RemoteURL        string     `json:"remote_url"`
	AllowLocalRemote bool       `json:"allow_local_remote"`
	BaseBranch       string     `json:"base_branch"`
	RCBranch         string     `json:"rc_branch"`
	CheckRunner      []string   `json:"check_runner"`
	Checks           [][]string `json:"checks"`
	DeployAction     []string   `json:"deploy_action"`
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read ops config: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode ops config: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Config{}, err
	}
	return normalizeConfig(cfg)
}

func ValidateConfig(cfg Config) error {
	_, err := normalizeConfig(cfg)
	return err
}

func normalizeConfig(cfg Config) (Config, error) {
	if len(cfg.Projects) == 0 {
		return Config{}, errors.New("ops config has no projects")
	}
	normalized := cloneConfig(cfg)
	repositories := make([]string, 0, len(normalized.Projects))
	for projectID, project := range normalized.Projects {
		if err := validateProjectID(projectID); err != nil {
			return Config{}, err
		}
		repository, err := canonicalDirectory(project.RepoPath)
		if err != nil {
			return Config{}, fmt.Errorf("project %q repository: %w", projectID, err)
		}
		project.RepoPath = repository
		normalized.Projects[projectID] = project
		repositories = append(repositories, repository)
	}

	gitBinary, err := trustedExecutable(normalized.GitBinary, repositories, true)
	if err != nil {
		return Config{}, fmt.Errorf("git binary: %w", err)
	}
	normalized.GitBinary = gitBinary
	for projectID, project := range normalized.Projects {
		project, err = normalizeProject(gitBinary, project, repositories)
		if err != nil {
			return Config{}, fmt.Errorf("project %q: %w", projectID, err)
		}
		normalized.Projects[projectID] = project
	}
	return normalized, nil
}

func normalizeProject(gitBinary string, project Project, repositories []string) (Project, error) {
	if !remotePattern.MatchString(project.Remote) {
		return Project{}, errors.New("invalid remote")
	}
	for name, value := range map[string]string{
		"remote": project.Remote, "base branch": project.BaseBranch, "RC branch": project.RCBranch,
	} {
		if err := checkBranchName(gitBinary, value); err != nil {
			return Project{}, fmt.Errorf("invalid %s: %w", name, err)
		}
	}
	if project.BaseBranch == project.RCBranch {
		return Project{}, errors.New("base and RC branches must differ")
	}
	remoteURL, err := normalizeRemoteURL(project.RemoteURL, project.AllowLocalRemote)
	if err != nil {
		return Project{}, err
	}
	project.RemoteURL = remoteURL
	if err := validateArgv(project.CheckRunner); err != nil {
		return Project{}, fmt.Errorf("check runner: %w", err)
	}
	checkRunner, err := trustedExecutable(project.CheckRunner[0], repositories, true)
	if err != nil {
		return Project{}, fmt.Errorf("check runner: %w", err)
	}
	project.CheckRunner[0] = checkRunner
	if len(project.Checks) == 0 {
		return Project{}, errors.New("checks are required")
	}
	for i, check := range project.Checks {
		if err := validateArgv(check); err != nil {
			return Project{}, fmt.Errorf("check %d: %w", i+1, err)
		}
	}
	if err := validateArgv(project.DeployAction); err != nil {
		return Project{}, fmt.Errorf("deploy action: %w", err)
	}
	deploy, err := trustedExecutable(project.DeployAction[0], repositories, true)
	if err != nil {
		return Project{}, fmt.Errorf("deploy action: %w", err)
	}
	project.DeployAction[0] = deploy
	return project, nil
}

func normalizeRemoteURL(value string, allowLocal bool) (string, error) {
	if allowLocal {
		if !filepath.IsAbs(value) {
			return "", errors.New("local remote URL must be absolute")
		}
		remote, err := canonicalDirectory(value)
		if err != nil {
			return "", fmt.Errorf("local remote URL: %w", err)
		}
		return remote, nil
	}
	if filepath.IsAbs(value) {
		return "", errors.New("local remote URL requires allow_local_remote")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Opaque != "" || parsed.User != nil || parsed.Hostname() == "" {
		return "", errors.New("invalid remote URL")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "ssh" {
		return "", errors.New("remote URL must use HTTPS or SSH")
	}
	if parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery ||
		!remotePathPattern.MatchString(parsed.Path) || parsed.Path != pathpkg.Clean(parsed.Path) ||
		parsed.Path == "/" || strings.Contains(value, "%") {
		return "", errors.New("remote URL contains unsupported components")
	}
	return value, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("ops config contains multiple JSON values")
		}
		return fmt.Errorf("decode trailing ops config data: %w", err)
	}
	return nil
}

func validateArgv(argv []string) error {
	if len(argv) == 0 || argv[0] == "" {
		return errors.New("executable is required")
	}
	for _, arg := range argv {
		if arg == "" || strings.ContainsRune(arg, 0) {
			return errors.New("empty or invalid argument")
		}
	}
	return nil
}

func checkBranchName(gitBinary, value string) error {
	if value == "" {
		return errors.New("empty ref")
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if _, err := runProcess(ctx, "/", []string{gitBinary, "check-ref-format", "--branch", value}, fixedEnvironment("/nonexistent", "/tmp")); err != nil {
		return errors.New("invalid ref")
	}
	return nil
}

func canonicalDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("path must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("path is not a directory")
	}
	return resolved, nil
}

func trustedExecutable(path string, repositories []string, allowCurrentUID bool) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("path must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	for _, repository := range repositories {
		if pathWithin(repository, resolved) {
			return "", errors.New("executable is inside a configured repository")
		}
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("path is not a regular executable")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return "", errors.New("executable is group or world writable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 && (!allowCurrentUID || stat.Uid != uint32(os.Geteuid())) {
		return "", errors.New("executable has an untrusted owner")
	}
	if err := validateExecutableParents(resolved, info); err != nil {
		return "", err
	}
	return resolved, nil
}

func validateExecutableParents(path string, child os.FileInfo) error {
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("executable parent is not a canonical directory")
		}
		if info.Mode().Perm()&0o022 != 0 && (info.Mode()&os.ModeSticky == 0 || !trustedPrivateChild(child)) {
			return fmt.Errorf("executable parent %q is replaceable", parent)
		}
		if parent == filepath.Dir(parent) {
			return nil
		}
		child = info
	}
}

func trustedPrivateChild(info os.FileInfo) bool {
	if info.Mode().Perm()&0o022 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && (stat.Uid == uint32(os.Geteuid()) || stat.Uid == 0)
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

func validateProjectID(projectID string) error {
	if !projectIDPattern.MatchString(projectID) {
		return errors.New("invalid project ID")
	}
	return nil
}

func cloneConfig(cfg Config) Config {
	cloned := Config{GitBinary: cfg.GitBinary, Projects: make(map[string]Project, len(cfg.Projects))}
	for projectID, project := range cfg.Projects {
		project.CheckRunner = append([]string(nil), project.CheckRunner...)
		checks := make([][]string, len(project.Checks))
		for i, check := range project.Checks {
			checks[i] = append([]string(nil), check...)
		}
		project.Checks = checks
		project.DeployAction = append([]string(nil), project.DeployAction...)
		cloned.Projects[projectID] = project
	}
	return cloned
}

func fixedEnvironment(home, temp string) []string {
	return []string{
		"PATH=/usr/bin:/bin", "HOME=" + home, "LANG=C", "LC_ALL=C", "TZ=UTC", "TMPDIR=" + temp, "TMP=" + temp, "TEMP=" + temp,
	}
}
