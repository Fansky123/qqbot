package main

import (
	"strings"
	"testing"

	"qqcodex/internal/ops"
)

func TestFailedIncludesStableCommitErrorCode(t *testing.T) {
	for _, test := range []struct {
		err  error
		code string
	}{
		{ops.NewTaskCommitChanged("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), "task_commit_changed"},
		{ops.NewRCCommitChanged("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), "rc_commit_changed"},
		{ops.ErrMergeConflict, "merge_conflict"},
	} {
		got := failed(test.err)
		if got.ErrorCode != test.code {
			t.Fatalf("failed(%v) code = %q, want %q", test.err, got.ErrorCode, test.code)
		}
		if got.CurrentCommit != ops.ChangedCommit(test.err) {
			t.Fatalf("failed(%v) commit = %q", test.err, got.CurrentCommit)
		}
	}
}

func TestParseValidate(t *testing.T) {
	fingerprint := strings.Repeat("a", 64)
	request, err := parseValidate([]string{"--project", "order-api", "--config-sha256", fingerprint})
	if err != nil || request.project != "order-api" || request.fingerprint != fingerprint {
		t.Fatalf("parseValidate = %#v, %v", request, err)
	}
	if _, err := parseValidate([]string{"--project", "", "--config-sha256", fingerprint}); err == nil {
		t.Fatal("parseValidate accepted empty project")
	}
}

func TestValidateExpectedProjectRejectsReleaseConfigMismatch(t *testing.T) {
	project := ops.Project{Remote: "origin", BaseBranch: "main", RCBranch: "rc", Checks: [][]string{{"go", "test", "./..."}}}
	expected := validateRequest{project: "order-api", fingerprint: ops.ProjectFingerprint(project)}
	if err := validateExpectedProject(project, expected); err != nil {
		t.Fatal(err)
	}
	changed := project
	changed.RCBranch = "other-rc"
	expected.fingerprint = ops.ProjectFingerprint(changed)
	if err := validateExpectedProject(project, expected); err == nil {
		t.Fatal("accepted mismatched release configuration")
	}
}
