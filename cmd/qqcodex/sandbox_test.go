package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"qqcodex/internal/config"
)

func TestValidateStartupUsesTrustedSandboxAndCapabilityProbe(t *testing.T) {
	cfg, _ := validGitConfig(t)
	options := newTestSandboxOptions(t)
	if err := validateTrustedSandboxExecutable(options.root, options.path, options.trustedUID); err != nil {
		t.Fatalf("test sandbox is not trusted: %v", err)
	}
	called := 0
	options.probe = func(_ context.Context, binary, workspace string) error {
		called++
		if binary != options.path || workspace != cfg.Consultation.Workspace {
			t.Fatalf("probe inputs = %q/%q, want %q/%q", binary, workspace, options.path, cfg.Consultation.Workspace)
		}
		return nil
	}

	if err := validateStartupWithSandbox(&cfg, options); err != nil {
		t.Fatal(err)
	}
	if called != 1 || cfg.Consultation.SandboxBinary != options.path {
		t.Fatalf("probe calls=%d sandbox=%q, want one call and %q", called, cfg.Consultation.SandboxBinary, options.path)
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
			options.probe = func(context.Context, string, string) error { probeCalled = true; return nil }
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
		options.probe = func(context.Context, string, string) error { return errors.New("private probe detail") }
		err := validateStartupWithSandbox(&cfg, options)
		if err == nil || err.Error() != "consultation sandbox probe failed" || strings.Contains(err.Error(), "private") {
			t.Fatalf("probe failure = %v, want generic error", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		cfg, _ := validGitConfig(t)
		options := newTestSandboxOptions(t)
		options.probeTimeout = 20 * time.Millisecond
		options.probe = func(ctx context.Context, _, _ string) error { <-ctx.Done(); return ctx.Err() }
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

func TestConsultationSandboxProbeArgsUseRequiredIsolation(t *testing.T) {
	want := []string{
		"--die-with-parent", "--new-session", "--unshare-all", "--share-net", "--unshare-user", "--cap-drop", "ALL",
		"--ro-bind", "/usr", "/usr",
		"--symlink", "usr/bin", "/bin",
		"--symlink", "usr/sbin", "/sbin",
		"--symlink", "usr/lib", "/lib",
		"--symlink", "usr/lib64", "/lib64",
		"--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp", "--tmpfs", "/run",
		"--dir", "/workspace", "--chdir", "/workspace", "/bin/true",
	}
	if got := consultationSandboxProbeArgs(); !slices.Equal(got, want) {
		t.Fatalf("probe args = %q, want %q", got, want)
	}
}

func TestInstalledBubblewrapCapabilityProbe(t *testing.T) {
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
	if err := probeConsultationSandbox(ctx, productionSandboxPath, t.TempDir()); err != nil {
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
		probe: func(context.Context, string, string) error { return nil },
	}
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}
