package onebot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	maxNicknameRunes = 128
	maxProbeGroups   = 10000
)

type Account struct {
	SelfID   ID
	Nickname string
	GroupIDs []ID
}

// Probe reads the authenticated account and group list from one OneBot connection.
func Probe(ctx context.Context, endpoint, token string) (Account, error) {
	if ctx == nil {
		return Account{}, errors.New("onebot probe context is required")
	}
	if err := ctx.Err(); err != nil {
		return Account{}, err
	}
	if err := validateProbeEndpoint(endpoint); err != nil {
		return Account{}, err
	}
	if token == "" {
		return Account{}, errors.New("onebot access token is required")
	}

	header := make(http.Header, 1)
	header.Set("Authorization", "Bearer "+token)
	conn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return Account{}, fmt.Errorf("connect OneBot WebSocket: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxEventBytes)

	loginData, err := performAction(ctx, conn, LoginInfoAction)
	if err != nil {
		return Account{}, err
	}
	selfID, nickname, err := decodeLoginInfo(loginData)
	if err != nil {
		return Account{}, err
	}

	groupData, err := performAction(ctx, conn, GroupListAction)
	if err != nil {
		return Account{}, err
	}
	groupIDs, err := decodeGroupList(groupData)
	if err != nil {
		return Account{}, err
	}
	return Account{SelfID: selfID, Nickname: nickname, GroupIDs: groupIDs}, nil
}

func validateProbeEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "ws" && parsed.Scheme != "wss") || parsed.User != nil {
		return errors.New("onebot WebSocket URL is invalid")
	}
	return nil
}

func performAction(ctx context.Context, conn *websocket.Conn, constructor func(string) ActionRequest) (json.RawMessage, error) {
	echo, err := randomEcho()
	if err != nil {
		return nil, fmt.Errorf("create OneBot action echo: %w", err)
	}
	if err := wsjson.Write(ctx, conn, constructor(echo)); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("write OneBot action: %w", err)
	}

	for {
		var raw json.RawMessage
		if err := wsjson.Read(ctx, conn, &raw); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("read OneBot action response: %w", err)
		}
		var envelope struct {
			PostType json.RawMessage `json:"post_type"`
			Status   json.RawMessage `json:"status"`
			RetCode  json.RawMessage `json:"retcode"`
			Echo     json.RawMessage `json:"echo"`
		}
		if json.Unmarshal(raw, &envelope) != nil || hasPostType(envelope.PostType) || len(envelope.Echo) == 0 {
			continue
		}
		var responseEcho string
		if json.Unmarshal(envelope.Echo, &responseEcho) != nil || responseEcho == "" || responseEcho != echo {
			continue
		}

		status, retCode, err := decodeActionResult(envelope.Status, envelope.RetCode)
		if err != nil {
			return nil, fmt.Errorf("malformed OneBot action response: %w", err)
		}
		var response ActionResponse
		if err := json.Unmarshal(raw, &response); err != nil {
			return nil, fmt.Errorf("malformed OneBot action response: %w", err)
		}
		if status == "ok" && retCode == 0 {
			return bytes.Clone(response.Data), nil
		}
		detail := response.Wording
		if detail == "" {
			detail = response.Message
		}
		if detail == "" {
			detail = "request failed"
		}
		return nil, fmt.Errorf("onebot action failed: status=%q retcode=%d: %s", status, retCode, detail)
	}
}

func decodeLoginInfo(data json.RawMessage) (ID, string, error) {
	var response struct {
		UserID   json.RawMessage `json:"user_id"`
		Nickname json.RawMessage `json:"nickname"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return "", "", errors.New("OneBot login info is invalid")
	}
	if len(response.UserID) == 0 || bytes.Equal(bytes.TrimSpace(response.UserID), []byte("null")) {
		return "", "", errors.New("OneBot login info is missing user ID")
	}
	var selfID ID
	if err := selfID.UnmarshalJSON(response.UserID); err != nil {
		return "", "", errors.New("OneBot login info user ID is invalid")
	}
	if len(response.Nickname) == 0 || bytes.Equal(bytes.TrimSpace(response.Nickname), []byte("null")) {
		return "", "", errors.New("OneBot login info is missing nickname")
	}
	var nickname string
	if err := json.Unmarshal(response.Nickname, &nickname); err != nil {
		return "", "", errors.New("OneBot login info nickname is invalid")
	}
	if utf8.RuneCountInString(nickname) > maxNicknameRunes {
		return "", "", errors.New("OneBot login info nickname is too long")
	}
	return selfID, nickname, nil
}

func decodeGroupList(data json.RawMessage) ([]ID, error) {
	var rows []struct {
		GroupID json.RawMessage `json:"group_id"`
	}
	if err := json.Unmarshal(data, &rows); err != nil || rows == nil {
		return nil, errors.New("OneBot group list is invalid")
	}
	if len(rows) > maxProbeGroups {
		return nil, errors.New("OneBot group list has too many groups")
	}
	groupIDs := make([]ID, 0, len(rows))
	seen := make(map[ID]struct{}, len(rows))
	for _, row := range rows {
		if len(row.GroupID) == 0 || bytes.Equal(bytes.TrimSpace(row.GroupID), []byte("null")) {
			return nil, errors.New("OneBot group list contains an invalid group ID")
		}
		var groupID ID
		if err := groupID.UnmarshalJSON(row.GroupID); err != nil {
			return nil, errors.New("OneBot group list contains an invalid group ID")
		}
		if _, exists := seen[groupID]; exists {
			continue
		}
		seen[groupID] = struct{}{}
		groupIDs = append(groupIDs, groupID)
	}
	return groupIDs, nil
}
