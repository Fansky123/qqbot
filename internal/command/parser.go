package command

import (
	"errors"
	"regexp"
	"strings"
)

type Kind string

const (
	KindCreate         Kind = "create"
	KindConfirm        Kind = "confirm"
	KindSupplement     Kind = "supplement"
	KindCancel         Kind = "cancel"
	KindStatus         Kind = "status"
	KindLog            Kind = "log"
	KindApproveMerge   Kind = "approve_merge"
	KindApproveDeploy  Kind = "approve_deploy"
	KindConsult        Kind = "consult"
	KindProjectConsult Kind = "project_consult"
)

type Command struct {
	Kind         Kind
	ProjectAlias string
	TaskID       string
	Body         string
}

const taskIDPattern = `T-[A-F0-9]{12}`

var createPattern = regexp.MustCompile(`(?s)^\[([\p{L}\p{N}_-]+)\]\s+(.+)$`)

var projectConsultPattern = regexp.MustCompile(`(?s)^问\s+\[([\p{L}\p{N}_-]+)\]\s+(.+)$`)

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

	if matches := projectConsultPattern.FindStringSubmatch(text); matches != nil {
		if !mentioned {
			return Command{}, errors.New("consult commands require a bot mention")
		}
		body := strings.TrimSpace(matches[2])
		if body == "" {
			return Command{}, errors.New("project consultation requires a question")
		}
		return Command{Kind: KindProjectConsult, ProjectAlias: matches[1], Body: body}, nil
	}

	if strings.HasPrefix(text, "问") {
		return Command{}, errors.New("invalid project consultation")
	}
	if strings.HasPrefix(text, "[") {
		return Command{}, errors.New("invalid create command")
	}
	if mentioned && text != "" {
		return Command{Kind: KindConsult, Body: text}, nil
	}

	return Command{}, errors.New("unrecognized command")
}
