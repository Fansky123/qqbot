package command

import "testing"

func TestParse(t *testing.T) {
	t.Parallel()

	taskID := "T-012345ABCDEF"
	tests := []struct {
		name      string
		text      string
		mentioned bool
		want      Command
	}{
		{
			name:      "create with Chinese alias",
			text:      "  [订单] implement order search  ",
			mentioned: true,
			want:      Command{Kind: KindCreate, ProjectAlias: "订单", Body: "implement order search"},
		},
		{
			name:      "generic consultation",
			text:      "  这个报错是什么意思？  ",
			mentioned: true,
			want:      Command{Kind: KindConsult, Body: "这个报错是什么意思？"},
		},
		{
			name:      "generic consultation beginning with ask",
			text:      "问一下这个错误怎么排查",
			mentioned: true,
			want:      Command{Kind: KindConsult, Body: "问一下这个错误怎么排查"},
		},
		{
			name:      "generic consultation beginning with ask and space",
			text:      "问 something",
			mentioned: true,
			want:      Command{Kind: KindConsult, Body: "问 something"},
		},
		{
			name:      "project consultation",
			text:      "问 [orders] 为什么会死锁？",
			mentioned: true,
			want:      Command{Kind: KindProjectConsult, ProjectAlias: "orders", Body: "为什么会死锁？"},
		},
		{
			name:      "bracketed text remains create",
			text:      "[orders] 修复死锁",
			mentioned: true,
			want:      Command{Kind: KindCreate, ProjectAlias: "orders", Body: "修复死锁"},
		},
		{
			name:      "confirm with mention",
			text:      "确认 #" + taskID,
			mentioned: true,
			want:      Command{Kind: KindConfirm, TaskID: taskID},
		},
		{
			name: "supplement",
			text: "补充 #" + taskID + " include pagination  ",
			want: Command{Kind: KindSupplement, TaskID: taskID, Body: "include pagination"},
		},
		{
			name: "cancel",
			text: "取消 #" + taskID,
			want: Command{Kind: KindCancel, TaskID: taskID},
		},
		{
			name: "status",
			text: "状态 #" + taskID,
			want: Command{Kind: KindStatus, TaskID: taskID},
		},
		{
			name: "log",
			text: "日志 #" + taskID,
			want: Command{Kind: KindLog, TaskID: taskID},
		},
		{
			name: "approve merge",
			text: "批准合并 #" + taskID,
			want: Command{Kind: KindApproveMerge, TaskID: taskID},
		},
		{
			name: "approve deploy",
			text: "批准部署 #" + taskID,
			want: Command{Kind: KindApproveDeploy, TaskID: taskID},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Parse(tt.text, tt.mentioned)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Parse() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseRejectsInvalidCommands(t *testing.T) {
	t.Parallel()

	taskID := "T-012345ABCDEF"
	tests := []struct {
		name      string
		text      string
		mentioned bool
	}{
		{name: "unknown command", text: "帮助"},
		{name: "plain consultation without mention", text: "这个报错是什么意思？"},
		{name: "empty mentioned text", text: "   ", mentioned: true},
		{name: "project consultation missing body after alias", text: "问 [orders]", mentioned: true},
		{name: "project consultation invalid alias", text: "问 [bad alias] x", mentioned: true},
		{name: "project consultation without mention", text: "问 [orders] 为什么会死锁？"},
		{name: "create without mention", text: "[orders] add search"},
		{name: "create empty body", text: "[orders]   ", mentioned: true},
		{name: "create alias with space", text: "[order api] add search", mentioned: true},
		{name: "create alias with punctuation", text: "[orders!] add search", mentioned: true},
		{name: "lowercase task ID", text: "确认 #T-012345abcDEF"},
		{name: "short task ID", text: "确认 #T-012345ABCDE"},
		{name: "long task ID", text: "确认 #T-012345ABCDEFF"},
		{name: "path-like task ID", text: "确认 #T-012345ABCDEF/extra"},
		{name: "fixed command trailing token", text: "取消 #" + taskID + " now"},
		{name: "empty supplement", text: "补充 #" + taskID + "   "},
		{name: "approval phrase substring", text: "请批准合并 #" + taskID},
		{name: "approval trailing token", text: "批准部署 #" + taskID + " 现在"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := Parse(tt.text, tt.mentioned); err == nil {
				t.Fatal("Parse() error = nil, want error")
			}
		})
	}
}
