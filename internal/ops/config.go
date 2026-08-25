package ops

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
)

var (
	projectIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	remotePattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

type Config struct {
	Projects map[string]Project `json:"projects"`
}

type Project struct {
	RepoPath     string     `json:"repo_path"`
	Remote       string     `json:"remote"`
	BaseBranch   string     `json:"base_branch"`
	RCBranch     string     `json:"rc_branch"`
	Checks       [][]string `json:"checks"`
	DeployAction []string   `json:"deploy_action"`
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
	if len(cfg.Projects) == 0 {
		return Config{}, errors.New("ops config has no projects")
	}
	for projectID, project := range cfg.Projects {
		if err := validateProjectID(projectID); err != nil {
			return Config{}, err
		}
		canonical, err := canonicalDirectory(project.RepoPath)
		if err != nil {
			return Config{}, fmt.Errorf("project %q repository: %w", projectID, err)
		}
		project.RepoPath = canonical
		if err := validateProject(project); err != nil {
			return Config{}, fmt.Errorf("project %q: %w", projectID, err)
		}
		cfg.Projects[projectID] = project
	}
	return cloneConfig(cfg), nil
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

func validateProject(project Project) error {
	if !remotePattern.MatchString(project.Remote) {
		return errors.New("invalid remote")
	}
	for name, value := range map[string]string{
		"remote":      project.Remote,
		"base branch": project.BaseBranch,
		"RC branch":   project.RCBranch,
	} {
		if err := checkBranchName(value); err != nil {
			return fmt.Errorf("invalid %s: %w", name, err)
		}
	}
	if project.BaseBranch == project.RCBranch {
		return errors.New("base and RC branches must differ")
	}
	if len(project.Checks) == 0 {
		return errors.New("checks are required")
	}
	for i, check := range project.Checks {
		if err := validateArgv(check); err != nil {
			return fmt.Errorf("check %d: %w", i+1, err)
		}
	}
	if err := validateArgv(project.DeployAction); err != nil {
		return fmt.Errorf("deploy action: %w", err)
	}
	return nil
}

func validateArgv(argv []string) error {
	if len(argv) == 0 || argv[0] == "" {
		return errors.New("executable is required")
	}
	for _, arg := range argv {
		if arg == "" {
			return errors.New("empty argument")
		}
	}
	return nil
}

func checkBranchName(value string) error {
	if value == "" {
		return errors.New("empty ref")
	}
	cmd := exec.Command("git", "check-ref-format", "--branch", value)
	if err := cmd.Run(); err != nil {
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

func validateProjectID(projectID string) error {
	if !projectIDPattern.MatchString(projectID) {
		return errors.New("invalid project ID")
	}
	return nil
}

func cloneConfig(cfg Config) Config {
	cloned := Config{Projects: make(map[string]Project, len(cfg.Projects))}
	for projectID, project := range cfg.Projects {
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
