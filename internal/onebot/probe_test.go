package onebot

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestProbe(t *testing.T) {
	t.Parallel()

	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer probe-token" {
			t.Errorf("Authorization = %q", got)
			return
		}
		login := readProbeAction(t, ctx, conn, "get_login_info")
		_ = wsjson.Write(ctx, conn, map[string]any{"post_type": "meta_event", "meta_event_type": "lifecycle"})
		if err := wsjson.Write(ctx, conn, ActionResponse{Status: "ok", RetCode: 0, Data: json.RawMessage(`{"user_id":42,"nickname":"Alice"}`), Echo: login.Echo}); err != nil {
			t.Errorf("write login response: %v", err)
			return
		}

		groups := readProbeAction(t, ctx, conn, "get_group_list")
		_ = wsjson.Write(ctx, conn, map[string]any{"post_type": "notice", "notice_type": "group_upload"})
		if err := wsjson.Write(ctx, conn, ActionResponse{Status: "ok", RetCode: 0, Data: json.RawMessage(`[{"group_id":"0007"},{"group_id":8},{"group_id":"7"}]`), Echo: groups.Echo}); err != nil {
			t.Errorf("write group response: %v", err)
		}
	})
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	account, err := Probe(ctx, webSocketURL(server.URL), "probe-token")
	if err != nil {
		t.Fatal(err)
	}
	if account.SelfID != "42" || account.Nickname != "Alice" {
		t.Fatalf("account = %#v", account)
	}
	if got, want := strings.Join(idsToStrings(account.GroupIDs), ","), "7,8"; got != want {
		t.Fatalf("group IDs = %q, want %q", got, want)
	}
}

func TestProbeRejectsInvalidLoginInfo(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		data string
	}{
		{name: "missing user ID", data: `{"nickname":"Alice"}`},
		{name: "null user ID", data: `{"user_id":null,"nickname":"Alice"}`},
		{name: "invalid user ID", data: `{"user_id":"not-an-id","nickname":"Alice"}`},
		{name: "missing nickname", data: `{"user_id":42}`},
		{name: "null nickname", data: `{"user_id":42,"nickname":null}`},
		{name: "non-string nickname", data: `{"user_id":42,"nickname":true}`},
		{name: "long nickname", data: `{"user_id":42,"nickname":"` + strings.Repeat("x", maxNicknameRunes+1) + `"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
				action := readProbeAction(t, ctx, conn, "get_login_info")
				_ = wsjson.Write(ctx, conn, ActionResponse{Status: "ok", RetCode: 0, Data: json.RawMessage(test.data), Echo: action.Echo})
			})
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := Probe(ctx, webSocketURL(server.URL), "token"); err == nil {
				t.Fatal("Probe accepted invalid login info")
			}
		})
	}
}

func TestProbeRejectsInvalidGroupList(t *testing.T) {
	t.Parallel()

	overLimit := make([]map[string]uint64, maxProbeGroups+1)
	for i := range overLimit {
		overLimit[i] = map[string]uint64{"group_id": uint64(i)}
	}
	overLimitJSON, err := json.Marshal(overLimit)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		data json.RawMessage
	}{
		{name: "not an array", data: json.RawMessage(`{}`)},
		{name: "null", data: json.RawMessage(`null`)},
		{name: "malformed row", data: json.RawMessage(`[1]`)},
		{name: "missing group ID", data: json.RawMessage(`[{}]`)},
		{name: "null group ID", data: json.RawMessage(`[{"group_id":null}]`)},
		{name: "invalid group ID", data: json.RawMessage(`[{"group_id":"not-an-id"}]`)},
		{name: "too many rows", data: overLimitJSON},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
				login := readProbeAction(t, ctx, conn, "get_login_info")
				if err := wsjson.Write(ctx, conn, ActionResponse{Status: "ok", RetCode: 0, Data: json.RawMessage(`{"user_id":"1","nickname":"Alice"}`), Echo: login.Echo}); err != nil {
					return
				}
				groups := readProbeAction(t, ctx, conn, "get_group_list")
				_ = wsjson.Write(ctx, conn, ActionResponse{Status: "ok", RetCode: 0, Data: test.data, Echo: groups.Echo})
			})
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := Probe(ctx, webSocketURL(server.URL), "token"); err == nil {
				t.Fatal("Probe accepted invalid group list")
			}
		})
	}
}

func TestProbeRejectsFailedAndMalformedMatchingResponses(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		response any
	}{
		{name: "failed", response: ActionResponse{Status: "failed", RetCode: 1404, Wording: "invalid params"}},
		{name: "malformed", response: map[string]any{"status": true, "retcode": 0}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
				action := readProbeAction(t, ctx, conn, "get_login_info")
				switch response := test.response.(type) {
				case ActionResponse:
					response.Echo = action.Echo
					_ = wsjson.Write(ctx, conn, response)
				default:
					response.(map[string]any)["echo"] = action.Echo
					_ = wsjson.Write(ctx, conn, response)
				}
			})
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := Probe(ctx, webSocketURL(server.URL), "token"); err == nil {
				t.Fatal("Probe accepted unsuccessful action response")
			}
		})
	}
}

func TestProbeIgnoresUnrelatedResponseEcho(t *testing.T) {
	t.Parallel()

	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		login := readProbeAction(t, ctx, conn, "get_login_info")
		_ = wsjson.Write(ctx, conn, ActionResponse{Status: "failed", RetCode: 1, Echo: "another-request"})
		_ = wsjson.Write(ctx, conn, ActionResponse{Status: "ok", RetCode: 0, Data: json.RawMessage(`{"user_id":1,"nickname":"Alice"}`), Echo: login.Echo})
		groups := readProbeAction(t, ctx, conn, "get_group_list")
		_ = wsjson.Write(ctx, conn, ActionResponse{Status: "ok", RetCode: 0, Data: json.RawMessage(`[]`), Echo: groups.Echo})
	})
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := Probe(ctx, webSocketURL(server.URL), "token"); err != nil {
		t.Fatal(err)
	}
}

func TestProbeRejectsOversizeResponse(t *testing.T) {
	t.Parallel()

	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		_ = readProbeAction(t, ctx, conn, "get_login_info")
		_ = wsjson.Write(ctx, conn, map[string]any{"padding": strings.Repeat("x", maxEventBytes)})
	})
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := Probe(ctx, webSocketURL(server.URL), "token"); err == nil {
		t.Fatal("Probe accepted an oversized response")
	}
}

func TestProbeHonorsCallerCancellation(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		_ = readProbeAction(t, ctx, conn, "get_login_info")
		close(started)
		<-ctx.Done()
	})
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { _, err := Probe(ctx, webSocketURL(server.URL), "token"); errCh <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Probe did not send the login action")
	}
	cancel()
	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("Probe error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Probe did not return after cancellation")
	}
}

func TestProbeValidatesInputs(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		ctx      context.Context
		endpoint string
		token    string
	}{
		{name: "nil context", endpoint: "ws://localhost", token: "token"},
		{name: "empty endpoint", ctx: context.Background(), token: "token"},
		{name: "http endpoint", ctx: context.Background(), endpoint: "http://localhost", token: "token"},
		{name: "missing host", ctx: context.Background(), endpoint: "ws:/path", token: "token"},
		{name: "userinfo", ctx: context.Background(), endpoint: "ws://user:password@localhost", token: "token"},
		{name: "empty token", ctx: context.Background(), endpoint: "ws://localhost"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Probe(test.ctx, test.endpoint, test.token); err == nil {
				t.Fatal("Probe accepted invalid input")
			}
		})
	}
}

func readProbeAction(t *testing.T, ctx context.Context, conn *websocket.Conn, wantAction string) ActionRequest {
	t.Helper()
	var action ActionRequest
	if err := wsjson.Read(ctx, conn, &action); err != nil {
		t.Fatalf("read action: %v", err)
	}
	if action.Action != wantAction || action.Echo == "" {
		t.Fatalf("action = %#v, want %q with echo", action, wantAction)
	}
	if action.Params.GroupID != "" || len(action.Params.Message) != 0 {
		t.Fatalf("probe action params = %#v, want empty", action.Params)
	}
	return action
}

func idsToStrings(ids []ID) []string {
	values := make([]string, len(ids))
	for i, id := range ids {
		values[i] = string(id)
	}
	return values
}
