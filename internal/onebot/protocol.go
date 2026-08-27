package onebot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type ID string

func (id *ID) UnmarshalJSON(raw []byte) error {
	if id == nil {
		return errors.New("onebot ID destination is nil")
	}
	value := string(raw)
	if len(raw) > 0 && raw[0] == '"' {
		if err := json.Unmarshal(raw, &value); err != nil {
			return errors.New("onebot ID is invalid")
		}
	}
	normalized, err := normalizeID(value)
	if err != nil {
		return err
	}
	*id = ID(normalized)
	return nil
}

type Segment struct {
	Type string                     `json:"type"`
	Data map[string]json.RawMessage `json:"data"`
}

type GroupMessage struct {
	MessageID ID
	GroupID   ID
	UserID    ID
	SelfID    ID
	Text      string
	Mentioned bool
}

type ActionParams struct {
	GroupID ID        `json:"group_id,omitempty"`
	Message []Segment `json:"message,omitempty"`
}

type ActionRequest struct {
	Action string       `json:"action"`
	Params ActionParams `json:"params"`
	Echo   string       `json:"echo"`
}

type ActionResponse struct {
	Status  string          `json:"status"`
	RetCode int             `json:"retcode"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
	Wording string          `json:"wording"`
	Echo    string          `json:"echo"`
}

func LoginInfoAction(echo string) ActionRequest {
	return ActionRequest{Action: "get_login_info", Echo: echo}
}

func GroupListAction(echo string) ActionRequest {
	return ActionRequest{Action: "get_group_list", Echo: echo}
}

func DecodeGroupMessage(raw []byte, selfID string) (GroupMessage, bool, error) {
	var envelope struct {
		PostType    string `json:"post_type"`
		MessageType string `json:"message_type"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return GroupMessage{}, false, fmt.Errorf("decode OneBot event: %w", err)
	}
	if envelope.PostType != "message" || envelope.MessageType != "group" {
		return GroupMessage{}, false, nil
	}

	configuredSelfID, err := normalizeID(selfID)
	if err != nil {
		return GroupMessage{}, false, errors.New("configured OneBot self ID is invalid")
	}
	var event struct {
		MessageID ID              `json:"message_id"`
		GroupID   ID              `json:"group_id"`
		UserID    ID              `json:"user_id"`
		SelfID    ID              `json:"self_id"`
		Message   json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return GroupMessage{}, false, fmt.Errorf("decode OneBot group message: %w", err)
	}
	if event.MessageID == "" || event.GroupID == "" || event.UserID == "" || event.SelfID == "" {
		return GroupMessage{}, false, errors.New("OneBot group message is missing an ID")
	}
	if string(event.SelfID) != configuredSelfID {
		return GroupMessage{}, false, nil
	}
	messageJSON := bytes.TrimSpace(event.Message)
	if len(messageJSON) == 0 || bytes.Equal(messageJSON, []byte("null")) {
		return GroupMessage{}, false, errors.New("OneBot group message is missing segments")
	}
	var segments []Segment
	if err := json.Unmarshal(messageJSON, &segments); err != nil {
		return GroupMessage{}, false, fmt.Errorf("decode OneBot group message segments: %w", err)
	}

	message := GroupMessage{
		MessageID: event.MessageID,
		GroupID:   event.GroupID,
		UserID:    event.UserID,
		SelfID:    event.SelfID,
	}
	var text strings.Builder
	for _, segment := range segments {
		switch segment.Type {
		case "text":
			part, ok := segment.Data["text"]
			var partText *string
			if !ok || json.Unmarshal(part, &partText) != nil || partText == nil {
				return GroupMessage{}, false, errors.New("OneBot text segment is invalid")
			}
			text.WriteString(*partText)
		case "at":
			target, ok := segment.Data["qq"]
			if !ok {
				return GroupMessage{}, false, errors.New("OneBot at segment is invalid")
			}
			var targetName string
			if json.Unmarshal(target, &targetName) == nil && targetName == "all" {
				continue
			}
			var targetID ID
			if err := json.Unmarshal(target, &targetID); err != nil {
				return GroupMessage{}, false, errors.New("OneBot at segment is invalid")
			}
			if string(targetID) == configuredSelfID {
				message.Mentioned = true
			}
		}
	}
	message.Text = text.String()
	return message, true, nil
}

func SendGroupAction(groupID, text, echo string) (ActionRequest, error) {
	normalizedGroupID, err := normalizeID(groupID)
	if err != nil {
		return ActionRequest{}, errors.New("OneBot group ID is invalid")
	}
	encodedText, err := json.Marshal(text)
	if err != nil {
		return ActionRequest{}, errors.New("encode OneBot group message failed")
	}
	return ActionRequest{
		Action: "send_group_msg",
		Params: ActionParams{
			GroupID: ID(normalizedGroupID),
			Message: []Segment{{
				Type: "text",
				Data: map[string]json.RawMessage{"text": encodedText},
			}},
		},
		Echo: echo,
	}, nil
}

func EscapeCQText(text string) string {
	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "[", "&#91;")
	text = strings.ReplaceAll(text, "]", "&#93;")
	return strings.ReplaceAll(text, ",", "&#44;")
}

func SplitMessage(text string, maxRunes int) []string {
	if maxRunes <= 0 {
		return nil
	}
	runes := []rune(text)
	if len(runes) == 0 {
		return []string{""}
	}
	parts := make([]string, 0, (len(runes)-1)/maxRunes+1)
	for len(runes) > maxRunes {
		parts = append(parts, string(runes[:maxRunes]))
		runes = runes[maxRunes:]
	}
	return append(parts, string(runes))
}

func normalizeID(value string) (string, error) {
	if value == "" {
		return "", errors.New("onebot ID is empty")
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return "", errors.New("onebot ID must be an unsigned decimal integer")
		}
	}
	number, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return "", errors.New("onebot ID is out of range")
	}
	return strconv.FormatUint(number, 10), nil
}
