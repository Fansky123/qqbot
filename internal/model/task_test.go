package model

import "testing"

func TestCanTransition(t *testing.T) {
	statuses := []Status{
		StatusDraft,
		StatusAwaitingConfirmation,
		StatusQueued,
		StatusRunning,
		StatusBlocked,
		StatusChecking,
		StatusPushed,
		StatusAwaitingMergeApproval,
		StatusMerging,
		StatusMergeConflict,
		StatusMerged,
		StatusAwaitingDeployApproval,
		StatusDeploying,
		StatusDeployFailed,
		StatusDeployed,
		StatusFailed,
		StatusCancelled,
	}
	if got, want := len(statuses), 17; got != want {
		t.Fatalf("got %d statuses, want %d", got, want)
	}
	allowed := []struct {
		from Status
		to   Status
	}{
		{StatusDraft, StatusAwaitingConfirmation},
		{StatusDraft, StatusFailed},
		{StatusDraft, StatusCancelled},
		{StatusAwaitingConfirmation, StatusQueued},
		{StatusAwaitingConfirmation, StatusFailed},
		{StatusAwaitingConfirmation, StatusCancelled},
		{StatusQueued, StatusRunning},
		{StatusQueued, StatusFailed},
		{StatusQueued, StatusCancelled},
		{StatusRunning, StatusBlocked},
		{StatusRunning, StatusChecking},
		{StatusRunning, StatusFailed},
		{StatusRunning, StatusCancelled},
		{StatusBlocked, StatusQueued},
		{StatusBlocked, StatusFailed},
		{StatusBlocked, StatusCancelled},
		{StatusChecking, StatusPushed},
		{StatusChecking, StatusFailed},
		{StatusChecking, StatusCancelled},
		{StatusPushed, StatusAwaitingMergeApproval},
		{StatusPushed, StatusFailed},
		{StatusPushed, StatusCancelled},
		{StatusAwaitingMergeApproval, StatusQueued},
		{StatusAwaitingMergeApproval, StatusMerging},
		{StatusAwaitingMergeApproval, StatusCancelled},
		{StatusMerging, StatusMerged},
		{StatusMerging, StatusMergeConflict},
		{StatusMerging, StatusFailed},
		{StatusMerged, StatusAwaitingDeployApproval},
		{StatusAwaitingDeployApproval, StatusDeploying},
		{StatusDeploying, StatusDeployed},
		{StatusDeploying, StatusDeployFailed},
		{StatusDeployFailed, StatusDeploying},
	}
	if got, want := len(allowed), 33; got != want {
		t.Fatalf("got %d allowed transitions, want %d", got, want)
	}
	expected := make(map[struct{ from, to Status }]bool, len(allowed))
	for _, transition := range allowed {
		expected[struct{ from, to Status }{transition.from, transition.to}] = true
	}

	for _, from := range statuses {
		for _, to := range statuses {
			want := expected[struct{ from, to Status }{from, to}]
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%q, %q) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestTerminalStatus(t *testing.T) {
	statuses := []Status{
		StatusDraft,
		StatusAwaitingConfirmation,
		StatusQueued,
		StatusRunning,
		StatusBlocked,
		StatusChecking,
		StatusPushed,
		StatusAwaitingMergeApproval,
		StatusMerging,
		StatusMergeConflict,
		StatusMerged,
		StatusAwaitingDeployApproval,
		StatusDeploying,
		StatusDeployFailed,
		StatusDeployed,
		StatusFailed,
		StatusCancelled,
	}
	terminal := map[Status]bool{
		StatusCancelled:     true,
		StatusFailed:        true,
		StatusMergeConflict: true,
		StatusDeployed:      true,
	}

	for _, status := range statuses {
		if got, want := status.Terminal(), terminal[status]; got != want {
			t.Errorf("Status(%q).Terminal() = %v, want %v", status, got, want)
		}
	}
}
