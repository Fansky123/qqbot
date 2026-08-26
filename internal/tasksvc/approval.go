package tasksvc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"qqcodex/internal/command"
	"qqcodex/internal/model"
	"qqcodex/internal/ops"
)

func (s *Service) approveMerge(ctx context.Context, message Message, parsed command.Command, key string) error {
	return s.approve(ctx, message, parsed, key, "merge", model.StatusAwaitingMergeApproval, model.StatusMerging, "批准合并")
}

func (s *Service) approveDeploy(ctx context.Context, message Message, parsed command.Command, key string) error {
	return s.approve(ctx, message, parsed, key, "deploy", model.StatusAwaitingDeployApproval, model.StatusDeploying, "批准部署")
}

func (s *Service) approve(ctx context.Context, message Message, parsed command.Command, key, kind string, expected, next model.Status, label string) error {
	if !s.authorizer.CanApprove(message.UserID) {
		return errTaskAccess
	}
	task, err := s.loadTask(ctx, message, parsed.TaskID)
	if err != nil {
		return err
	}
	if task.Status != expected && (kind != "deploy" || task.Status != model.StatusDeployFailed) {
		return fmt.Errorf("task %s cannot be approved for %s from %s", task.ID, kind, task.Status)
	}
	boundCommit := task.TaskCommit
	if kind == "deploy" {
		boundCommit = task.RCCommit
		task.DeployKey = ops.DeployKey(task.ProjectID, task.ID, boundCommit)
	}
	if !schedulerCommitPattern.MatchString(boundCommit) {
		return errors.New("approval commit is missing or invalid")
	}
	version := task.Version
	if err := s.transition(task, next); err != nil {
		return err
	}
	now := time.Now().UTC()
	approval := model.Approval{
		TaskID: task.ID, Kind: kind, UserID: message.UserID, GroupID: message.GroupID,
		MessageID: message.MessageID, BoundCommit: boundCommit, Result: "approved", CreatedAt: now,
	}
	input := model.Input{
		TaskID: task.ID, Kind: "approve_" + kind, UserID: message.UserID, GroupID: message.GroupID,
		MessageID: message.MessageID, Body: message.Text, CreatedAt: now,
	}
	detail := "approved exact " + kind + " commit " + boundCommit
	if kind == "deploy" {
		detail += " with deploy key " + task.DeployKey
	}
	if err := s.db.CommitApprovalMutation(ctx, task, version, approval, input, kind+"_approval", bound(detail), key, now); err != nil {
		return fatalStore(err)
	}
	s.scheduler.Wake()
	return s.send(ctx, message.GroupID, "任务 #"+task.ID+" 已"+label+"，绑定提交："+boundCommit)
}
