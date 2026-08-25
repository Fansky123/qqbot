package model

import "testing"

func TestCanTransition(t *testing.T) {
	tests := []struct {
		from Status
		to   Status
		want bool
	}{
		{StatusDraft, StatusAwaitingConfirmation, true},
		{StatusAwaitingConfirmation, StatusQueued, true},
		{StatusQueued, StatusRunning, true},
		{StatusRunning, StatusChecking, true},
		{StatusChecking, StatusPushed, true},
		{StatusPushed, StatusAwaitingMergeApproval, true},
		{StatusAwaitingMergeApproval, StatusQueued, true},
		{StatusAwaitingMergeApproval, StatusMerging, true},
		{StatusMerged, StatusAwaitingDeployApproval, true},
		{StatusAwaitingDeployApproval, StatusDeploying, true},
		{StatusDeploying, StatusDeployed, true},
		{StatusDeployed, StatusRunning, false},
		{StatusCancelled, StatusQueued, false},
	}

	for _, tt := range tests {
		if got := CanTransition(tt.from, tt.to); got != tt.want {
			t.Errorf("CanTransition(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.want)
		}
	}
}

func TestTerminalStatus(t *testing.T) {
	for _, status := range []Status{StatusCancelled, StatusFailed, StatusMergeConflict, StatusDeployed} {
		if !status.Terminal() {
			t.Fatalf("%q must be terminal", status)
		}
	}
}
