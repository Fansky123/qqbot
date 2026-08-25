package model

import "time"

type Status string

const (
	StatusDraft                  Status = "draft"
	StatusAwaitingConfirmation   Status = "awaiting_confirmation"
	StatusQueued                 Status = "queued"
	StatusRunning                Status = "running"
	StatusBlocked                Status = "blocked"
	StatusChecking               Status = "checking"
	StatusPushed                 Status = "pushed"
	StatusAwaitingMergeApproval  Status = "awaiting_merge_approval"
	StatusMerging                Status = "merging"
	StatusMergeConflict          Status = "merge_conflict"
	StatusMerged                 Status = "merged"
	StatusAwaitingDeployApproval Status = "awaiting_deploy_approval"
	StatusDeploying              Status = "deploying"
	StatusDeployFailed           Status = "deploy_failed"
	StatusDeployed               Status = "deployed"
	StatusFailed                 Status = "failed"
	StatusCancelled              Status = "cancelled"
)

type Task struct {
	ID           string
	ProjectID    string
	GroupID      string
	CreatorID    string
	Requirement  string
	Plan         string
	Status       Status
	Branch       string
	Worktree     string
	BaseCommit   string
	GitCommonDir string
	TaskCommit   string
	RCCommit     string
	SessionID    string
	Summary      string
	Failure      string
	Version      int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type Input struct {
	TaskID    string
	Kind      string
	UserID    string
	GroupID   string
	MessageID string
	Body      string
	CreatedAt time.Time
}

type Approval struct {
	TaskID      string
	Kind        string
	UserID      string
	GroupID     string
	MessageID   string
	BoundCommit string
	Result      string
	CreatedAt   time.Time
}

var transitions = map[Status]map[Status]bool{
	StatusDraft: {
		StatusAwaitingConfirmation: true,
		StatusFailed:               true,
		StatusCancelled:            true,
	},
	StatusAwaitingConfirmation: {
		StatusQueued:    true,
		StatusFailed:    true,
		StatusCancelled: true,
	},
	StatusQueued: {
		StatusRunning:   true,
		StatusFailed:    true,
		StatusCancelled: true,
	},
	StatusRunning: {
		StatusBlocked:   true,
		StatusChecking:  true,
		StatusFailed:    true,
		StatusCancelled: true,
	},
	StatusBlocked: {
		StatusQueued:    true,
		StatusFailed:    true,
		StatusCancelled: true,
	},
	StatusChecking: {
		StatusPushed:    true,
		StatusFailed:    true,
		StatusCancelled: true,
	},
	StatusPushed: {
		StatusAwaitingMergeApproval: true,
		StatusFailed:                true,
		StatusCancelled:             true,
	},
	StatusAwaitingMergeApproval: {
		StatusQueued:    true,
		StatusMerging:   true,
		StatusCancelled: true,
	},
	StatusMerging: {
		StatusMerged:        true,
		StatusMergeConflict: true,
		StatusFailed:        true,
	},
	StatusMerged: {
		StatusAwaitingDeployApproval: true,
	},
	StatusAwaitingDeployApproval: {
		StatusDeploying: true,
	},
	StatusDeploying: {
		StatusDeployed:     true,
		StatusDeployFailed: true,
	},
	StatusDeployFailed: {
		StatusDeploying: true,
	},
}

func CanTransition(from, to Status) bool {
	return transitions[from][to]
}

func (s Status) Terminal() bool {
	switch s {
	case StatusCancelled, StatusFailed, StatusMergeConflict, StatusDeployed:
		return true
	default:
		return false
	}
}
