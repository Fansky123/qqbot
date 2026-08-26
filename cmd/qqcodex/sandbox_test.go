package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qqcodex/internal/codex"
	"qqcodex/internal/config"
)

func TestValidateStartupUsesTrustedSandboxAndCapabilityProbe(t *testing.T) {
	cfg, _ := validGitConfig(t)
	options := newTestSandboxOptions(t)
	if err := validateTrustedSandboxExecutable(options.root, options.path, options.trustedUID); err != nil {
		t.Fatalf("test sandbox is not trusted: %v", err)
	}
	wantCodex, err := resolveCodexExecutable(cfg.Codex.Binary)
	if err != nil {
		t.Fatal(err)
	}
	wantCodeModeHost := filepath.Join(filepath.Dir(wantCodex), "codex-code-mode-host")
	called := 0
	options.probe = func(_ context.Context, binary, codexBinary, codeModeHostBinary, workspace string) error {
		called++
		if binary != options.path || codexBinary != wantCodex || codeModeHostBinary != wantCodeModeHost || workspace != cfg.Consultation.Workspace {
			t.Fatalf("probe inputs = %q/%q/%q/%q, want %q/%q/%q/%q", binary, codexBinary, codeModeHostBinary, workspace, options.path, wantCodex, wantCodeModeHost, cfg.Consultation.Workspace)
		}
		return nil
	}

	if err := validateStartupWithSandbox(&cfg, options); err != nil {
		t.Fatal(err)
	}
	if called != 1 || cfg.Consultation.SandboxBinary != options.path || cfg.Consultation.CodeModeHostBinary != wantCodeModeHost {
		t.Fatalf("probe calls=%d sandbox=%q companion=%q, want one call, %q and %q", called, cfg.Consultation.SandboxBinary, cfg.Consultation.CodeModeHostBinary, options.path, wantCodeModeHost)
	}
}

func TestValidateStartupRejectsMissingOrInvalidCodeModeHost(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{name: "missing", mutate: func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "not executable", mutate: func(t *testing.T, path string) {
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, _ := validGitConfig(t)
			companion := filepath.Join(filepath.Dir(cfg.Codex.Binary), "codex-code-mode-host")
			tt.mutate(t, companion)
			options := newTestSandboxOptions(t)
			probeCalled := false
			options.probe = func(context.Context, string, string, string, string) error { probeCalled = true; return nil }
			err := validateStartupWithSandbox(&cfg, options)
			if err == nil || !strings.Contains(err.Error(), "codex code mode host") {
				t.Fatalf("startup error = %v, want code mode host rejection", err)
			}
			if probeCalled {
				t.Fatal("sandbox probe ran without a valid code mode host")
			}
		})
	}
}

func TestValidateStartupRejectsUntrustedSandboxPath(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *sandboxStartupOptions)
	}{
		{name: "symlink target", mutate: func(t *testing.T, options *sandboxStartupOptions) {
			real := filepath.Join(options.root, "real-bwrap")
			writeExecutable(t, real)
			if err := os.Remove(options.path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, options.path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink ancestor", mutate: func(t *testing.T, options *sandboxStartupOptions) {
			usr := filepath.Join(options.root, "usr")
			real := filepath.Join(options.root, "real-usr")
			if err := os.Rename(usr, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, usr); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "wrong owner", mutate: func(_ *testing.T, options *sandboxStartupOptions) {
			options.trustedUID++
		}},
		{name: "writable target", mutate: func(t *testing.T, options *sandboxStartupOptions) {
			if err := os.Chmod(options.path, 0o775); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "writable parent", mutate: func(t *testing.T, options *sandboxStartupOptions) {
			if err := os.Chmod(filepath.Dir(options.path), 0o775); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "not regular", mutate: func(t *testing.T, options *sandboxStartupOptions) {
			if err := os.Remove(options.path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(options.path, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "not executable", mutate: func(t *testing.T, options *sandboxStartupOptions) {
			if err := os.Chmod(options.path, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, _ := validGitConfig(t)
			options := newTestSandboxOptions(t)
			tt.mutate(t, &options)
			probeCalled := false
			options.probe = func(context.Context, string, string, string, string) error { probeCalled = true; return nil }
			if err := validateStartupWithSandbox(&cfg, options); err == nil || !strings.Contains(err.Error(), "consultation sandbox") {
				t.Fatalf("startup error = %v, want consultation sandbox rejection", err)
			}
			if probeCalled {
				t.Fatal("untrusted sandbox was executed")
			}
		})
	}
}

func TestValidateStartupRejectsSandboxProbeFailureAndTimeout(t *testing.T) {
	t.Run("failure", func(t *testing.T) {
		cfg, _ := validGitConfig(t)
		options := newTestSandboxOptions(t)
		options.probe = func(context.Context, string, string, string, string) error { return errors.New("private probe detail") }
		err := validateStartupWithSandbox(&cfg, options)
		if err == nil || err.Error() != "consultation sandbox probe failed" || strings.Contains(err.Error(), "private") {
			t.Fatalf("probe failure = %v, want generic error", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		cfg, _ := validGitConfig(t)
		options := newTestSandboxOptions(t)
		options.probeTimeout = 20 * time.Millisecond
		options.probe = func(ctx context.Context, _, _, _, _ string) error { <-ctx.Done(); return ctx.Err() }
		started := time.Now()
		err := validateStartupWithSandbox(&cfg, options)
		if err == nil || err.Error() != "consultation sandbox probe failed" {
			t.Fatalf("probe timeout = %v, want generic error", err)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("probe timeout took %s", elapsed)
		}
	})
}

func TestConsultationSandboxProbeArgsUseProductionMountShape(t *testing.T) {
	args := codex.ConsultationSandboxProbeArgs("/host/bin/codex", "/host/bin/codex-code-mode-host", "/host/workspace")
	for _, sequence := range [][]string{
		{"--die-with-parent", "--new-session", "--unshare-all", "--share-net", "--unshare-user", "--cap-drop", "ALL"},
		{"--ro-bind", "/usr", "/usr"},
		{"--symlink", "usr/bin", "/bin"}, {"--symlink", "usr/sbin", "/sbin"},
		{"--symlink", "usr/lib", "/lib"}, {"--symlink", "usr/lib64", "/lib64"},
		{"--dev", "/dev"}, {"--proc", "/proc"}, {"--tmpfs", "/tmp"},
		{"--dir", "/etc"}, {"--dir", "/etc/ssl"}, {"--dir", "/etc/ssl/certs"},
		{"--ro-bind", "/etc/ssl/certs", "/etc/ssl/certs"},
		{"--ro-bind-try", "/etc/ssl/openssl.cnf", "/etc/ssl/openssl.cnf"},
		{"--ro-bind-try", "/etc/resolv.conf", "/etc/resolv.conf"},
		{"--ro-bind-try", "/etc/hosts", "/etc/hosts"},
		{"--ro-bind-try", "/etc/nsswitch.conf", "/etc/nsswitch.conf"},
		{"--ro-bind-try", "/etc/gai.conf", "/etc/gai.conf"},
		{"--tmpfs", "/run"}, {"--dir", "/run/qqcodex"}, {"--dir", "/run/qqcodex/home"},
		{"--dir", "/run/qqcodex/codex-home"}, {"--dir", "/run/qqcodex/bin"},
		{"--ro-bind", "/host/bin/codex", "/run/qqcodex/bin/codex"},
		{"--ro-bind", "/host/bin/codex-code-mode-host", "/run/qqcodex/bin/codex-code-mode-host"},
		{"--ro-bind", "/host/workspace", "/workspace"},
		{"--ro-bind-data", "3", "/run/qqcodex/codex-home/config.toml"},
		{"--setenv", "HOME", "/run/qqcodex/home"}, {"--setenv", "CODEX_HOME", "/run/qqcodex/codex-home"},
		{"--setenv", "TMPDIR", "/tmp"}, {"--setenv", "TMP", "/tmp"}, {"--setenv", "TEMP", "/tmp"},
		{"--setenv", "PATH", "/usr/bin:/bin"}, {"--chdir", "/workspace"}, {"/bin/true"},
	} {
		if !containsArgSequence(args, sequence) {
			t.Errorf("probe argv lacks %#v: %#v", sequence, args)
		}
	}
	if got := args[len(args)-1]; got != "/bin/true" {
		t.Fatalf("probe command = %q, want /bin/true", got)
	}
}

func TestInstalledBubblewrapCapabilityProbe(t *testing.T) {
	if os.Getenv("QQCODEX_RUN_BWRAP_INTEGRATION") != "1" {
		t.Skip("set QQCODEX_RUN_BWRAP_INTEGRATION=1 to run the installed bwrap probe")
	}
	if _, err := os.Lstat(productionSandboxPath); errors.Is(err, os.ErrNotExist) {
		t.Skip("/usr/bin/bwrap is not installed")
	} else if err != nil {
		t.Fatal(err)
	}
	if err := validateTrustedSandboxExecutable("/", productionSandboxPath, 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), productionSandboxProbeTimeout)
	defer cancel()
	workspace := t.TempDir()
	if err := os.Chmod(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	codexBinary, err := resolveCodexExecutable("codex")
	if err != nil {
		t.Fatal(err)
	}
	codeModeHostBinary, err := resolveCodexCodeModeHostExecutable(codexBinary)
	if err != nil {
		t.Fatal(err)
	}
	if err := probeConsultationSandbox(ctx, productionSandboxPath, codexBinary, codeModeHostBinary, workspace); err != nil {
		t.Fatalf("installed bubblewrap capability probe failed: %v", err)
	}
}

func validateStartupForTest(t *testing.T, cfg *config.Config) error {
	t.Helper()
	return validateStartupWithSandbox(cfg, newTestSandboxOptions(t))
}

func newTestSandboxOptions(t *testing.T) sandboxStartupOptions {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "usr", "bin", "bwrap")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{filepath.Join(root, "usr"), filepath.Join(root, "usr", "bin")} {
		if err := os.Chmod(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, path)
	return sandboxStartupOptions{
		path: path, root: root, trustedUID: uint32(os.Geteuid()), probeTimeout: time.Second,
		probe: func(context.Context, string, string, string, string) error { return nil },
	}
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func containsArgSequence(args, want []string) bool {
	for index := 0; index+len(want) <= len(args); index++ {
		match := true
		for offset := range want {
			if args[index+offset] != want[offset] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
