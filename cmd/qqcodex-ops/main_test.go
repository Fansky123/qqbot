package main

import (
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
