package ops

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestClientBuildsTypedArgvAndParsesResponse(t *testing.T) {
	ctx := context.Background()
	record := filepath.Join(t.TempDir(), "argv.json")
	client := Client{Command: helperCommand(t, record, "success")}
	commit := strings.Repeat("a", 40)

	tests := []struct {
		name string
		call func() (string, error)
		want []string
	}{
		{
			name: "sync",
			call: func() (string, error) {
				return "", client.Sync(ctx, "order-api")
			},
			want: []string{"sync", "--project", "order-api"},
		},
		{
			name: "push",
			call: func() (string, error) {
				return "", client.PushTask(ctx, "order-api", testTaskID, "codex/"+testTaskID, commit)
			},
			want: []string{"push", "--project", "order-api", "--task", testTaskID, "--branch", "codex/" + testTaskID, "--commit", commit},
		},
		{
			name: "merge",
			call: func() (string, error) {
				return client.MergeRC(ctx, "order-api", testTaskID, commit)
			},
			want: []string{"merge", "--project", "order-api", "--task", testTaskID, "--commit", commit},
		},
		{
			name: "deploy",
			call: func() (string, error) {
				return "", client.DeployRC(ctx, "order-api", testTaskID, commit)
			},
			want: []string{"deploy", "--project", "order-api", "--task", testTaskID, "--rc-commit", commit},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCommit, err := tt.call()
			if err != nil {
				t.Fatal(err)
			}
			if tt.name == "merge" && gotCommit != commit {
				t.Fatalf("MergeRC() commit = %q, want %q", gotCommit, commit)
			}
			var got []string
			data, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("argv = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestClientRejectsInvalidOrMultipleJSONResponses(t *testing.T) {
	for _, mode := range []string{
		"unknown-field", "multiple", "success-with-error", "failure-without-error",
		"missing-status", "null-status",
	} {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			client := Client{Command: helperCommand(t, filepath.Join(t.TempDir(), "argv.json"), mode)}
			err := client.Sync(context.Background(), "order-api")
			if err == nil {
				t.Fatal("Sync() error = nil, want strict response error")
			}
			if (mode == "missing-status" || mode == "null-status") && err.Error() != "ops helper failed with an invalid response" {
				t.Fatalf("Sync() error = %v, want invalid response error", err)
			}
		})
	}
}

func TestClientDoesNotPassCodexCredential(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "must-not-cross-boundary")
	t.Setenv("OPENAI_API_KEY", "must-not-cross-boundary")
	t.Setenv("NAPCAT_ACCESS_TOKEN", "must-not-cross-boundary")
	t.Setenv("CUSTOM_TOKEN", "must-not-cross-boundary")
	t.Setenv("HOME", "/attacker/home")
	t.Setenv("PATH", "/attacker/bin")
	client := Client{Command: helperCommand(t, filepath.Join(t.TempDir(), "argv.json"), "check-env")}
	if err := client.Sync(context.Background(), "order-api"); err != nil {
		t.Fatal(err)
	}
}

func TestClientRejectsUntrustedExecutable(t *testing.T) {
	client := Client{Command: []string{"qqcodex-ops"}}
	if err := client.Sync(context.Background(), "order-api"); err == nil {
		t.Fatal("Sync() error = nil, want untrusted executable rejection")
	}
}

func TestClientDoesNotExposeHelperStderr(t *testing.T) {
	client := Client{Command: helperCommand(t, filepath.Join(t.TempDir(), "argv.json"), "secret-stderr")}
	err := client.Sync(context.Background(), "order-api")
	if err == nil || err.Error() != "public failure" {
		t.Fatalf("Sync() error = %v, want only public JSON error", err)
	}
}

func TestOpsClientHelper(t *testing.T) {
	separator := -1
	for i, arg := range os.Args {
		if arg == "qqcodex-client-helper" {
			separator = i
			break
		}
	}
	if separator < 0 {
		return
	}
	if separator < 0 || len(os.Args) < separator+4 {
		os.Exit(120)
	}
	record, mode := os.Args[separator+1], os.Args[separator+2]
	args := os.Args[separator+3:]
	data, _ := json.Marshal(args)
	_ = os.WriteFile(record, data, 0o600)
	commit := strings.Repeat("a", 40)
	switch mode {
	case "success":
		if len(args) > 0 && args[0] == "merge" {
			_, _ = os.Stdout.WriteString(`{"ok":true,"rc_commit":"` + commit + `"}`)
		} else {
			_, _ = os.Stdout.WriteString(`{"ok":true}`)
		}
	case "unknown-field":
		_, _ = os.Stdout.WriteString(`{"ok":true,"extra":1}`)
	case "multiple":
		_, _ = os.Stdout.WriteString("{\"ok\":true}\n{\"ok\":true}\n")
	case "success-with-error":
		_, _ = os.Stdout.WriteString(`{"ok":true,"error":"bad"}`)
	case "failure-without-error":
		_, _ = os.Stdout.WriteString(`{"ok":false}`)
	case "missing-status":
		_, _ = os.Stdout.WriteString(`{"error":"public failure"}`)
		os.Exit(1)
	case "null-status":
		_, _ = os.Stdout.WriteString(`{"ok":null,"error":"public failure"}`)
		os.Exit(1)
	case "check-env":
		if os.Getenv("PATH") != "/usr/bin:/bin" || os.Getenv("HOME") != "/nonexistent" ||
			os.Getenv("CODEX_API_KEY") != "" || os.Getenv("OPENAI_API_KEY") != "" ||
			os.Getenv("NAPCAT_ACCESS_TOKEN") != "" || os.Getenv("CUSTOM_TOKEN") != "" {
			_, _ = os.Stdout.WriteString(`{"ok":false,"error":"credential leaked"}`)
			os.Exit(1)
		}
		_, _ = os.Stdout.WriteString(`{"ok":true}`)
	case "secret-stderr":
		_, _ = os.Stderr.WriteString("PRIVATE-OPS-CREDENTIAL\n")
		_, _ = os.Stdout.WriteString(`{"ok":false,"error":"public failure"}`)
		os.Exit(1)
	default:
		os.Exit(121)
	}
	os.Exit(0)
}

func helperCommand(t *testing.T, record, mode string) []string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	trusted := filepath.Join(privateTempDir(t), "qqcodex-ops-test")
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trusted, data, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := trustedExecutable(trusted, nil); err != nil {
		t.Fatalf("test helper is not trusted: %v", err)
	}
	return []string{trusted, "-test.run=^TestOpsClientHelper$", "--", "qqcodex-client-helper", record, mode}
}
