package command

import (
	"errors"
	"regexp"
	"strings"
)

type Kind string

const (
	KindCreate        Kind = "create"
	KindConfirm       Kind = "confirm"
	KindSupplement    Kind = "supplement"
	KindCancel        Kind = "cancel"
	KindStatus        Kind = "status"
	KindLog           Kind = "log"
	KindApproveMerge  Kind = "approve_merge"
	KindApproveDeploy Kind = "approve_deploy"
)

type Command struct {
	Kind         Kind
	ProjectAlias string
	TaskID       string
	Body         string
}

const taskIDPattern = `T-[A-F0-9]{12}`

var createPattern = regexp.MustCompile(`(?s)^\[([\p{L}\p{N}_-]+)\]\s+(.+)$`)

var fixedCommands = []struct {
	kind    Kind
	pattern *regexp.Regexp
}{
	{kind: KindConfirm, pattern: regexp.MustCompile(`^确认\s+#(` + taskIDPattern + `)$`)},
	{kind: KindSupplement, pattern: regexp.MustCompile(`(?s)^补充\s+#(` + taskIDPattern + `)\s+(.+)$`)},
	{kind: KindCancel, pattern: regexp.MustCompile(`^取消\s+#(` + taskIDPattern + `)$`)},
	{kind: KindStatus, pattern: regexp.MustCompile(`^状态\s+#(` + taskIDPattern + `)$`)},
	{kind: KindLog, pattern: regexp.MustCompile(`^日志\s+#(` + taskIDPattern + `)$`)},
	{kind: KindApproveMerge, pattern: regexp.MustCompile(`^批准合并\s+#(` + taskIDPattern + `)$`)},
	{kind: KindApproveDeploy, pattern: regexp.MustCompile(`^批准部署\s+#(` + taskIDPattern + `)$`)},
}

// Parse converts a trimmed OneBot message into one supported command.
func Parse(text string, mentioned bool) (Command, error) {
	text = strings.TrimSpace(text)

	if matches := createPattern.FindStringSubmatch(text); matches != nil {
		if !mentioned {
			return Command{}, errors.New("create commands require a bot mention")
		}
		body := strings.TrimSpace(matches[2])
		if body == "" {
			return Command{}, errors.New("create command requires a task description")
		}
		return Command{Kind: KindCreate, ProjectAlias: matches[1], Body: body}, nil
	}

	for _, command := range fixedCommands {
		matches := command.pattern.FindStringSubmatch(text)
		if matches == nil {
			continue
		}

		parsed := Command{Kind: command.kind, TaskID: matches[1]}
		if command.kind == KindSupplement {
			parsed.Body = strings.TrimSpace(matches[2])
			if parsed.Body == "" {
				return Command{}, errors.New("supplement command requires content")
			}
		}
		return parsed, nil
	}

	return Command{}, errors.New("unrecognized command")
}
