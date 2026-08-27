package onebot

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestClientAuthenticatesDispatchesAndSends(t *testing.T) {
	t.Parallel()

	auth := make(chan string, 1)
	action := make(chan ActionRequest, 1)
	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, request *http.Request) {
		auth <- request.Header.Get("Authorization")
		event := map[string]any{
			"post_type": "message", "message_type": "group",
			"message_id": 1, "group_id": 2, "user_id": 3, "self_id": 4,
			"message": []any{
				map[string]any{"type": "at", "data": map[string]any{"qq": "4"}},
				map[string]any{"type": "text", "data": map[string]any{"text": "[orders] fix"}},
			},
		}
		if err := wsjson.Write(ctx, conn, event); err != nil {
			return
		}
		var requestAction ActionRequest
		if err := wsjson.Read(ctx, conn, &requestAction); err != nil {
			return
		}
		action <- requestAction
		_ = wsjson.Write(ctx, conn, ActionResponse{Status: "ok", RetCode: 0, Echo: requestAction.Echo})
	})
	defer server.Close()

	client := &Client{URL: webSocketURL(server.URL), Token: "napcat-secret", SelfID: "4", MessageRunes: 1200}
	runCtx, stop := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	messages := make(chan GroupMessage, 1)
	go func() {
		runErr <- client.Run(runCtx, func(_ context.Context, message GroupMessage) error {
			messages <- message
			return errors.New("one message failed")
		})
	}()

	message := receive(t, messages)
	if message.Text != "[orders] fix" || !message.Mentioned {
		t.Fatalf("unexpected group message: %#v", message)
	}
	if got := receive(t, auth); got != "Bearer napcat-secret" {
		t.Fatalf("Authorization = %q", got)
	}

	text := `a&b[c],d`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Send(ctx, "2", text); err != nil {
		t.Fatal(err)
	}
	got := receive(t, action)
	if got.Action != "send_group_msg" || got.Params.GroupID != "2" || got.Echo == "" {
		t.Fatalf("unexpected action: %#v", got)
	}
	if decoded, err := hex.DecodeString(got.Echo); err != nil || len(decoded) != 16 {
		t.Fatalf("echo is not a random 128-bit value: %q", got.Echo)
	}
	var sentText string
	if len(got.Params.Message) != 1 || json.Unmarshal(got.Params.Message[0].Data["text"], &sentText) != nil || sentText != text {
		t.Fatalf("message was not sent as raw array text: %#v", got.Params.Message)
	}

	stop()
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestClientCorrelatesConcurrentOutOfOrderResponses(t *testing.T) {
	t.Parallel()

	const sends = 24
	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		actions := make([]ActionRequest, 0, sends)
		for range sends {
			var action ActionRequest
			if err := wsjson.Read(ctx, conn, &action); err != nil {
				return
			}
			actions = append(actions, action)
		}
		for i := len(actions) - 1; i >= 0; i-- {
			if err := wsjson.Write(ctx, conn, ActionResponse{Status: "ok", RetCode: 0, Echo: actions[i].Echo}); err != nil {
				return
			}
		}
	})
	defer server.Close()

	client, stop, runErr := runTestClient(t, server.URL)
	waitForConnection(t, client)

	var wg sync.WaitGroup
	errs := make(chan error, sends)
	for i := range sends {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			errs <- client.Send(ctx, "2", fmt.Sprintf("message-%d", i))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Send returned %v", err)
		}
	}

	stop()
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestClientReturnsActionFailure(t *testing.T) {
	t.Parallel()

	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		var action ActionRequest
		if err := wsjson.Read(ctx, conn, &action); err != nil {
			return
		}
		_ = wsjson.Write(ctx, conn, ActionResponse{
			Status: "failed", RetCode: 1404, Message: "bad request", Wording: "invalid params", Echo: action.Echo,
		})
	})
	defer server.Close()

	client, stop, runErr := runTestClient(t, server.URL)
	waitForConnection(t, client)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := client.Send(ctx, "2", "message")
	if err == nil || !strings.Contains(err.Error(), "1404") || !strings.Contains(err.Error(), "invalid params") {
		t.Fatalf("Send error = %v", err)
	}
	if got := clientPending(client); got != 0 {
		t.Fatalf("pending actions = %d, want 0", got)
	}
	stop()
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestClientEventEchoDoesNotCompletePendingAction(t *testing.T) {
	t.Parallel()

	allowResponse := make(chan struct{})
	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		var action ActionRequest
		if wsjson.Read(ctx, conn, &action) != nil {
			return
		}
		event := map[string]any{
			"post_type": "message", "message_type": "group", "echo": action.Echo,
			"message_id": 7, "group_id": 2, "user_id": 3, "self_id": 4,
			"message": []any{map[string]any{"type": "text", "data": map[string]any{"text": "event-with-echo"}}},
		}
		if wsjson.Write(ctx, conn, event) != nil {
			return
		}
		select {
		case <-allowResponse:
		case <-ctx.Done():
			return
		}
		_ = wsjson.Write(ctx, conn, ActionResponse{Status: "ok", RetCode: 0, Echo: action.Echo})
	})
	defer server.Close()

	client := &Client{URL: webSocketURL(server.URL), Token: "token", SelfID: "4", MessageRunes: 1200}
	runCtx, stop := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	messages := make(chan GroupMessage, 1)
	go func() {
		runErr <- client.Run(runCtx, func(_ context.Context, message GroupMessage) error {
			messages <- message
			return nil
		})
	}()
	waitForConnection(t, client)

	sendCtx, cancelSend := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelSend()
	sendErr := make(chan error, 1)
	go func() { sendErr <- client.Send(sendCtx, "2", "message") }()
	select {
	case message := <-messages:
		if message.Text != "event-with-echo" {
			t.Fatalf("event text = %q", message.Text)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("group event with echo was not dispatched")
	}
	select {
	case err := <-sendErr:
		t.Fatalf("event completed pending action with %v", err)
	default:
	}
	close(allowResponse)
	if err := receive(t, sendErr); err != nil {
		t.Fatalf("Send returned %v", err)
	}

	stop()
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestClientRejectsMalformedMatchingActionResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		response func(string) map[string]any
	}{
		{name: "missing retcode", response: func(echo string) map[string]any { return map[string]any{"status": "ok", "echo": echo} }},
		{name: "null retcode", response: func(echo string) map[string]any { return map[string]any{"status": "ok", "retcode": nil, "echo": echo} }},
		{name: "string retcode", response: func(echo string) map[string]any { return map[string]any{"status": "ok", "retcode": "0", "echo": echo} }},
		{name: "float retcode", response: func(echo string) map[string]any { return map[string]any{"status": "ok", "retcode": 0.5, "echo": echo} }},
		{name: "missing status", response: func(echo string) map[string]any { return map[string]any{"retcode": 0, "echo": echo} }},
		{name: "null status", response: func(echo string) map[string]any { return map[string]any{"status": nil, "retcode": 0, "echo": echo} }},
		{name: "numeric status", response: func(echo string) map[string]any { return map[string]any{"status": 1, "retcode": 0, "echo": echo} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
				var action ActionRequest
				if wsjson.Read(ctx, conn, &action) != nil {
					return
				}
				_ = wsjson.Write(ctx, conn, test.response(action.Echo))
			})
			defer server.Close()

			client, stop, runErr := runTestClient(t, server.URL)
			waitForConnection(t, client)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := client.Send(ctx, "2", "message")
			if err == nil || !strings.Contains(err.Error(), "malformed OneBot action response") {
				t.Fatalf("Send error = %v", err)
			}
			if got := clientPending(client); got != 0 {
				t.Fatalf("pending actions = %d, want 0", got)
			}
			stop()
			if err := receive(t, runErr); err != nil {
				t.Fatalf("Run returned %v", err)
			}
		})
	}
}

func TestClientHandlerCanSendWithoutBlockingReader(t *testing.T) {
	t.Parallel()

	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		event := map[string]any{
			"post_type": "message", "message_type": "group",
			"message_id": 1, "group_id": 2, "user_id": 3, "self_id": 4,
			"message": []any{map[string]any{"type": "text", "data": map[string]any{"text": "reply"}}},
		}
		if wsjson.Write(ctx, conn, event) != nil {
			return
		}
		var action ActionRequest
		if wsjson.Read(ctx, conn, &action) != nil {
			return
		}
		_ = wsjson.Write(ctx, conn, ActionResponse{Status: "ok", RetCode: 0, Echo: action.Echo})
		<-ctx.Done()
	})
	defer server.Close()

	client := &Client{URL: webSocketURL(server.URL), Token: "token", SelfID: "4", MessageRunes: 1200}
	runCtx, stop := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	handlerErr := make(chan error, 1)
	go func() {
		runErr <- client.Run(runCtx, func(ctx context.Context, message GroupMessage) error {
			sendCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			defer cancel()
			err := client.Send(sendCtx, string(message.GroupID), "handled")
			handlerErr <- err
			return err
		})
	}()
	if err := receive(t, handlerErr); err != nil {
		t.Fatalf("handler Send returned %v", err)
	}
	stop()
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestClientSplitsSequentiallyAndStopsAtFirstFailure(t *testing.T) {
	t.Parallel()

	actions := make(chan ActionRequest, 3)
	third := make(chan bool, 1)
	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		for i := range 2 {
			var action ActionRequest
			if err := wsjson.Read(ctx, conn, &action); err != nil {
				return
			}
			actions <- action
			response := ActionResponse{Status: "ok", RetCode: 0, Echo: action.Echo}
			if i == 1 {
				response.Status = "failed"
				response.RetCode = 500
				response.Wording = "stopped"
			}
			if err := wsjson.Write(ctx, conn, response); err != nil {
				return
			}
		}
		readCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
		defer cancel()
		var action ActionRequest
		third <- wsjson.Read(readCtx, conn, &action) == nil
	})
	defer server.Close()

	client := &Client{URL: webSocketURL(server.URL), Token: "token", SelfID: "4", MessageRunes: 3}
	runCtx, stop := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- client.Run(runCtx, func(context.Context, GroupMessage) error { return nil }) }()
	waitForConnection(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := client.Send(ctx, "2", "abcdefg")
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("Send error = %v", err)
	}
	first, second := receive(t, actions), receive(t, actions)
	if got := actionText(t, first); got != "abc" {
		t.Fatalf("first segment = %q", got)
	}
	if got := actionText(t, second); got != "def" {
		t.Fatalf("second segment = %q", got)
	}
	if receive(t, third) {
		t.Fatal("third segment was sent after an action failure")
	}

	stop()
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestClientCallerCancellationRemovesPendingAction(t *testing.T) {
	t.Parallel()

	actionRead := make(chan struct{}, 1)
	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		var action ActionRequest
		if wsjson.Read(ctx, conn, &action) == nil {
			actionRead <- struct{}{}
		}
		<-ctx.Done()
	})
	defer server.Close()

	client, stop, runErr := runTestClient(t, server.URL)
	waitForConnection(t, client)
	sendCtx, cancelSend := context.WithCancel(context.Background())
	sendErr := make(chan error, 1)
	go func() { sendErr <- client.Send(sendCtx, "2", "message") }()
	receive(t, actionRead)
	cancelSend()
	if err := receive(t, sendErr); !errors.Is(err, context.Canceled) {
		t.Fatalf("Send error = %v, want context cancellation", err)
	}
	if got := clientPending(client); got != 0 {
		t.Fatalf("pending actions = %d, want 0", got)
	}

	stop()
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestClientDisconnectFailsPendingWithoutReplaying(t *testing.T) {
	t.Parallel()

	var connections atomic.Int32
	firstAction := make(chan struct{}, 1)
	replayed := make(chan bool, 1)
	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		switch connections.Add(1) {
		case 1:
			var action ActionRequest
			if wsjson.Read(ctx, conn, &action) == nil {
				firstAction <- struct{}{}
			}
		case 2:
			readCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
			defer cancel()
			var action ActionRequest
			replayed <- wsjson.Read(readCtx, conn, &action) == nil
		default:
			<-ctx.Done()
		}
	})
	defer server.Close()

	client := &Client{URL: webSocketURL(server.URL), Token: "token", SelfID: "4", MessageRunes: 1200}
	client.sleep = func(ctx context.Context, _ time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	runCtx, stop := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- client.Run(runCtx, func(context.Context, GroupMessage) error { return nil }) }()
	waitForConnection(t, client)

	sendCtx, cancelSend := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelSend()
	sendErr := make(chan error, 1)
	go func() { sendErr <- client.Send(sendCtx, "2", "message") }()
	receive(t, firstAction)
	if err := receive(t, sendErr); !errors.Is(err, ErrDisconnected) {
		t.Fatalf("Send error = %v, want disconnected", err)
	}
	if receive(t, replayed) {
		t.Fatal("action was automatically replayed on the replacement connection")
	}
	if got := clientPending(client); got != 0 {
		t.Fatalf("replacement connection pending actions = %d, want 0", got)
	}

	stop()
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestClientDisconnectFailsAllPendingActions(t *testing.T) {
	t.Parallel()

	const sends = 32
	closeConnection := make(chan struct{})
	server := newWebSocketServer(t, func(ctx context.Context, _ *websocket.Conn, _ *http.Request) {
		select {
		case <-closeConnection:
		case <-ctx.Done():
		}
	})
	defer server.Close()

	client := &Client{URL: webSocketURL(server.URL), Token: "token", SelfID: "4", MessageRunes: 1200}
	client.sleep = func(ctx context.Context, _ time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	}
	runCtx, stop := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- client.Run(runCtx, func(context.Context, GroupMessage) error { return nil }) }()
	waitForConnection(t, client)

	errs := make(chan error, sends)
	for i := range sends {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			errs <- client.Send(ctx, "2", fmt.Sprintf("message-%d", i))
		}()
	}
	waitForPending(t, client, sends)
	close(closeConnection)
	for range sends {
		if err := receive(t, errs); !errors.Is(err, ErrDisconnected) {
			t.Errorf("Send error = %v, want disconnected", err)
		}
	}
	if got := clientPending(client); got != 0 {
		t.Fatalf("pending actions = %d, want 0", got)
	}

	stop()
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestClientSendDuringReconnectFailsImmediately(t *testing.T) {
	t.Parallel()

	server := newWebSocketServer(t, func(context.Context, *websocket.Conn, *http.Request) {})
	defer server.Close()
	waiting := make(chan struct{}, 1)
	client := &Client{URL: webSocketURL(server.URL), Token: "token", SelfID: "4", MessageRunes: 1200}
	client.sleep = func(ctx context.Context, _ time.Duration) error {
		waiting <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	runCtx, stop := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- client.Run(runCtx, func(context.Context, GroupMessage) error { return nil }) }()
	receive(t, waiting)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	if err := client.Send(ctx, "2", "message"); !errors.Is(err, ErrDisconnected) {
		t.Fatalf("Send error = %v, want disconnected", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("Send during reconnect took %s", elapsed)
	}

	stop()
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestClientReconnectsAfterServerClose(t *testing.T) {
	t.Parallel()

	var connections atomic.Int32
	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		connection := connections.Add(1)
		event := map[string]any{
			"post_type": "message", "message_type": "group",
			"message_id": connection, "group_id": 2, "user_id": 3, "self_id": 4,
			"message": []any{map[string]any{"type": "text", "data": map[string]any{"text": fmt.Sprintf("connection-%d", connection)}}},
		}
		_ = wsjson.Write(ctx, conn, event)
		if connection > 1 {
			<-ctx.Done()
		}
	})
	defer server.Close()

	client := &Client{URL: webSocketURL(server.URL), Token: "token", SelfID: "4", MessageRunes: 1200}
	delays := make(chan time.Duration, 1)
	client.sleep = func(_ context.Context, delay time.Duration) error {
		delays <- delay
		return nil
	}
	runCtx, stop := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	messages := make(chan GroupMessage, 2)
	go func() {
		runErr <- client.Run(runCtx, func(_ context.Context, message GroupMessage) error {
			messages <- message
			return nil
		})
	}()

	if got := receive(t, messages).Text; got != "connection-1" {
		t.Fatalf("first message = %q", got)
	}
	if got := receive(t, delays); got != time.Second {
		t.Fatalf("first reconnect delay = %s", got)
	}
	if got := receive(t, messages).Text; got != "connection-2" {
		t.Fatalf("reconnected message = %q", got)
	}

	stop()
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestClientReconnectBackoffCapsAtFifteenSeconds(t *testing.T) {
	t.Parallel()

	server := newWebSocketServer(t, func(context.Context, *websocket.Conn, *http.Request) {})
	defer server.Close()

	client := &Client{URL: webSocketURL(server.URL), Token: "token", SelfID: "4", MessageRunes: 1200}
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var got []time.Duration
	client.sleep = func(ctx context.Context, delay time.Duration) error {
		mu.Lock()
		got = append(got, delay)
		count := len(got)
		mu.Unlock()
		if count == 7 {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	if err := client.Run(ctx, func(context.Context, GroupMessage) error { return nil }); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 15 * time.Second, 15 * time.Second}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("reconnect delays = %v, want %v", got, want)
	}
}

func TestClientHealthyConnectionResetsReconnectBackoff(t *testing.T) {
	t.Parallel()

	server := newWebSocketServer(t, func(context.Context, *websocket.Conn, *http.Request) {})
	defer server.Close()

	client := &Client{URL: webSocketURL(server.URL), Token: "token", SelfID: "4", MessageRunes: 1200}
	times := []time.Time{
		time.Unix(0, 0), time.Unix(1, 0),
		time.Unix(2, 0), time.Unix(3, 0),
		time.Unix(4, 0), time.Unix(35, 0),
	}
	var timeMu sync.Mutex
	client.now = func() time.Time {
		timeMu.Lock()
		defer timeMu.Unlock()
		got := times[0]
		times = times[1:]
		return got
	}
	ctx, cancel := context.WithCancel(context.Background())
	var delays []time.Duration
	client.sleep = func(ctx context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		if len(delays) == 3 {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	if err := client.Run(ctx, func(context.Context, GroupMessage) error { return nil }); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, time.Second}
	if fmt.Sprint(delays) != fmt.Sprint(want) {
		t.Fatalf("reconnect delays = %v, want %v", delays, want)
	}
}

func TestClientReconnectsAfterOversizeEvent(t *testing.T) {
	t.Parallel()

	var connections atomic.Int32
	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		if connections.Add(1) == 1 {
			_ = wsjson.Write(ctx, conn, map[string]any{"padding": strings.Repeat("x", maxEventBytes)})
			return
		}
		event := map[string]any{
			"post_type": "message", "message_type": "group",
			"message_id": 9, "group_id": 2, "user_id": 3, "self_id": 4,
			"message": []any{map[string]any{"type": "text", "data": map[string]any{"text": "after-oversize"}}},
		}
		_ = wsjson.Write(ctx, conn, event)
		<-ctx.Done()
	})
	defer server.Close()

	client := &Client{URL: webSocketURL(server.URL), Token: "token", SelfID: "4", MessageRunes: 1200}
	client.sleep = func(context.Context, time.Duration) error { return nil }
	runCtx, stop := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	messages := make(chan GroupMessage, 1)
	go func() {
		runErr <- client.Run(runCtx, func(_ context.Context, message GroupMessage) error {
			messages <- message
			return nil
		})
	}()
	if got := receive(t, messages).Text; got != "after-oversize" {
		t.Fatalf("message after oversize event = %q", got)
	}
	stop()
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestClientIgnoresOrphanResponseAndReturnsMalformedResponse(t *testing.T) {
	t.Parallel()

	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		_ = wsjson.Write(ctx, conn, map[string]any{"status": "ok", "echo": "orphan"})
		var action ActionRequest
		if wsjson.Read(ctx, conn, &action) != nil {
			return
		}
		_ = wsjson.Write(ctx, conn, map[string]any{"status": "ok", "retcode": "invalid", "echo": action.Echo})
	})
	defer server.Close()

	client, stop, runErr := runTestClient(t, server.URL)
	waitForConnection(t, client)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := client.Send(ctx, "2", "message")
	if err == nil || !strings.Contains(err.Error(), "malformed OneBot action response") {
		t.Fatalf("Send error = %v", err)
	}
	if got := clientPending(client); got != 0 {
		t.Fatalf("pending actions = %d, want 0", got)
	}
	stop()
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestClientRejectsMissingTokenAndConcurrentRun(t *testing.T) {
	t.Parallel()

	missingToken := &Client{URL: "ws://127.0.0.1:1", SelfID: "4", MessageRunes: 1200}
	err := missingToken.Run(context.Background(), func(context.Context, GroupMessage) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "access token") {
		t.Fatalf("Run without token error = %v", err)
	}
	if err := missingToken.Send(context.Background(), "2", "message"); !errors.Is(err, ErrDisconnected) {
		t.Fatalf("Send before Run error = %v", err)
	}

	server := newWebSocketServer(t, func(ctx context.Context, _ *websocket.Conn, _ *http.Request) { <-ctx.Done() })
	defer server.Close()
	client, stop, runErr := runTestClient(t, server.URL)
	waitForConnection(t, client)
	if err := client.Run(context.Background(), func(context.Context, GroupMessage) error { return nil }); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("concurrent Run error = %v", err)
	}
	stop()
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.Run(canceled, func(context.Context, GroupMessage) error { return nil }); err != nil {
		t.Fatalf("reuse after shutdown returned %v", err)
	}
}

func TestClientCancellationInterruptsReconnectWait(t *testing.T) {
	t.Parallel()

	client := &Client{URL: "ws://127.0.0.1:1", Token: "token", SelfID: "4", MessageRunes: 1200}
	waiting := make(chan struct{}, 1)
	client.sleep = func(ctx context.Context, _ time.Duration) error {
		waiting <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- client.Run(ctx, func(context.Context, GroupMessage) error { return nil }) }()
	receive(t, waiting)
	cancel()
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestClientCancellationWaitsForContextAwareHandler(t *testing.T) {
	t.Parallel()

	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		event := map[string]any{
			"post_type": "message", "message_type": "group",
			"message_id": 1, "group_id": 2, "user_id": 3, "self_id": 4,
			"message": []any{map[string]any{"type": "text", "data": map[string]any{"text": "wait"}}},
		}
		_ = wsjson.Write(ctx, conn, event)
		<-ctx.Done()
	})
	defer server.Close()

	client := &Client{URL: webSocketURL(server.URL), Token: "token", SelfID: "4", MessageRunes: 1200}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 1)
	returned := make(chan struct{}, 1)
	runErr := make(chan error, 1)
	go func() {
		runErr <- client.Run(ctx, func(ctx context.Context, _ GroupMessage) error {
			started <- struct{}{}
			<-ctx.Done()
			returned <- struct{}{}
			return ctx.Err()
		})
	}()
	receive(t, started)
	cancel()
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	receive(t, returned)
}

func TestClientBacklogDisconnectPreservesOverflowEvent(t *testing.T) {
	t.Parallel()

	const eventCount = eventQueueSize + 2
	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	firstClosed := make(chan struct{})
	reconnected := make(chan struct{}, 1)
	var connections atomic.Int32
	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		if connections.Add(1) != 1 {
			reconnected <- struct{}{}
			<-ctx.Done()
			return
		}
		if wsjson.Write(ctx, conn, groupEvent(1)) != nil {
			return
		}
		select {
		case <-handlerStarted:
		case <-ctx.Done():
			return
		}
		for id := 2; id <= eventCount; id++ {
			if wsjson.Write(ctx, conn, groupEvent(id)) != nil {
				return
			}
		}
		var ignored json.RawMessage
		_ = wsjson.Read(ctx, conn, &ignored)
		close(firstClosed)
	})
	defer server.Close()

	client := &Client{URL: webSocketURL(server.URL), Token: "token", SelfID: "4", MessageRunes: 1200}
	client.sleep = func(context.Context, time.Duration) error { return nil }
	runCtx, stop := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	processed := make(chan string, eventCount)
	go func() {
		runErr <- client.Run(runCtx, func(_ context.Context, message GroupMessage) error {
			if message.MessageID == "1" {
				close(handlerStarted)
				<-releaseHandler
			}
			processed <- string(message.MessageID)
			return nil
		})
	}()

	receive(t, firstClosed)
	close(releaseHandler)
	receive(t, reconnected)
	for id := 1; id <= eventCount; id++ {
		select {
		case got := <-processed:
			if want := fmt.Sprint(id); got != want {
				t.Fatalf("processed message %d = %q, want %q", id, got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("message %d was not processed", id)
		}
	}
	select {
	case duplicate := <-processed:
		t.Fatalf("message %q was processed more than once", duplicate)
	default:
	}

	stop()
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestClientCancellationWhileBacklogFullReturnsPromptly(t *testing.T) {
	t.Parallel()

	handlerStarted := make(chan struct{})
	firstClosed := make(chan struct{})
	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		if wsjson.Write(ctx, conn, groupEvent(1)) != nil {
			return
		}
		select {
		case <-handlerStarted:
		case <-ctx.Done():
			return
		}
		for id := 2; id <= eventQueueSize+2; id++ {
			if wsjson.Write(ctx, conn, groupEvent(id)) != nil {
				return
			}
		}
		var ignored json.RawMessage
		_ = wsjson.Read(ctx, conn, &ignored)
		close(firstClosed)
	})
	defer server.Close()

	client := &Client{URL: webSocketURL(server.URL), Token: "token", SelfID: "4", MessageRunes: 1200}
	runCtx, stop := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() {
		runErr <- client.Run(runCtx, func(ctx context.Context, message GroupMessage) error {
			if message.MessageID == "1" {
				close(handlerStarted)
				<-ctx.Done()
			}
			return nil
		})
	}()
	receive(t, firstClosed)
	stop()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancellation with a full backlog")
	}
}

func TestClientAutoSelfIDDiscoversIdentityBeforeDispatch(t *testing.T) {
	login := make(chan ActionRequest, 1)
	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		var action ActionRequest
		if wsjson.Read(ctx, conn, &action) != nil {
			return
		}
		login <- action
		if wsjson.Write(ctx, conn, loginInfoResponse(action.Echo, "3289886218")) != nil {
			return
		}
		_ = wsjson.Write(ctx, conn, groupEventForSelf(1, "3289886218", []any{
			map[string]any{"type": "at", "data": map[string]any{"qq": "3289886218"}},
			map[string]any{"type": "text", "data": map[string]any{"text": "discovered"}},
		}))
		<-ctx.Done()
	})
	defer server.Close()

	client := &Client{URL: webSocketURL(server.URL), Token: "token", SelfID: "auto", MessageRunes: 1200}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	messages := make(chan GroupMessage, 1)
	go func() {
		runErr <- client.Run(ctx, func(_ context.Context, message GroupMessage) error {
			messages <- message
			return nil
		})
	}()

	if got := receive(t, login); got.Action != "get_login_info" || got.Echo == "" {
		t.Fatalf("login action = %#v", got)
	}
	message := receive(t, messages)
	if message.SelfID != "3289886218" || message.Text != "discovered" || !message.Mentioned {
		t.Fatalf("message = %#v", message)
	}
	if client.SelfID != "auto" {
		t.Fatalf("Client.SelfID = %q, want auto", client.SelfID)
	}

	cancel()
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestClientAutoSelfIDDiscardsEventsBeforeIdentity(t *testing.T) {
	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		var action ActionRequest
		if wsjson.Read(ctx, conn, &action) != nil {
			return
		}
		if wsjson.Write(ctx, conn, groupEventForSelf(1, "42", nil)) != nil {
			return
		}
		if wsjson.Write(ctx, conn, loginInfoResponse(action.Echo, "42")) != nil {
			return
		}
		_ = wsjson.Write(ctx, conn, groupEventForSelf(2, "42", nil))
		<-ctx.Done()
	})
	defer server.Close()

	client := &Client{URL: webSocketURL(server.URL), Token: "token", SelfID: "auto", MessageRunes: 1200}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	messages := make(chan GroupMessage, 1)
	go func() {
		runErr <- client.Run(ctx, func(_ context.Context, message GroupMessage) error {
			messages <- message
			return nil
		})
	}()

	if message := receive(t, messages); message.MessageID != "2" {
		t.Fatalf("message ID = %q, want 2", message.MessageID)
	}
	cancel()
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestClientAutoSelfIDFiltersEventsAndMentions(t *testing.T) {
	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		var action ActionRequest
		if wsjson.Read(ctx, conn, &action) != nil {
			return
		}
		if wsjson.Write(ctx, conn, loginInfoResponse(action.Echo, "42")) != nil {
			return
		}
		for _, event := range []map[string]any{
			groupEventForSelf(1, "43", nil),
			groupEventForSelf(2, "42", []any{map[string]any{"type": "at", "data": map[string]any{"qq": "43"}}}),
			groupEventForSelf(3, "42", []any{map[string]any{"type": "at", "data": map[string]any{"qq": "all"}}}),
			groupEventForSelf(4, "42", []any{map[string]any{"type": "at", "data": map[string]any{"qq": "42"}}}),
		} {
			if wsjson.Write(ctx, conn, event) != nil {
				return
			}
		}
		<-ctx.Done()
	})
	defer server.Close()

	client := &Client{URL: webSocketURL(server.URL), Token: "token", SelfID: "auto", MessageRunes: 1200}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	messages := make(chan GroupMessage, 3)
	go func() {
		runErr <- client.Run(ctx, func(_ context.Context, message GroupMessage) error {
			messages <- message
			return nil
		})
	}()

	for _, want := range []struct {
		id        string
		mentioned bool
	}{
		{id: "2"},
		{id: "3"},
		{id: "4", mentioned: true},
	} {
		message := receive(t, messages)
		if string(message.MessageID) != want.id || message.Mentioned != want.mentioned {
			t.Fatalf("message = %#v, want ID %q and mentioned %v", message, want.id, want.mentioned)
		}
	}
	cancel()
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestClientReconnectsWithNewSelfID(t *testing.T) {
	var connections atomic.Int32
	secondConnected := make(chan struct{}, 1)
	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		connection := connections.Add(1)
		var action ActionRequest
		if wsjson.Read(ctx, conn, &action) != nil {
			return
		}
		selfID := "100"
		if connection == 2 {
			selfID = "200"
		}
		if wsjson.Write(ctx, conn, loginInfoResponse(action.Echo, selfID)) != nil {
			return
		}
		if connection == 1 {
			return
		}
		secondConnected <- struct{}{}
		if wsjson.Write(ctx, conn, groupEventForSelf(1, "100", nil)) != nil {
			return
		}
		_ = wsjson.Write(ctx, conn, groupEventForSelf(2, "200", nil))
		<-ctx.Done()
	})
	defer server.Close()

	client := &Client{URL: webSocketURL(server.URL), Token: "token", SelfID: "auto", MessageRunes: 1200}
	delays := make(chan time.Duration, 1)
	client.sleep = func(_ context.Context, delay time.Duration) error {
		delays <- delay
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	messages := make(chan GroupMessage, 1)
	go func() {
		runErr <- client.Run(ctx, func(_ context.Context, message GroupMessage) error {
			messages <- message
			return nil
		})
	}()

	if got := receive(t, delays); got != time.Second {
		t.Fatalf("reconnect delay = %s, want %s", got, time.Second)
	}
	receive(t, secondConnected)
	if message := receive(t, messages); message.MessageID != "2" || message.SelfID != "200" {
		t.Fatalf("message = %#v", message)
	}
	cancel()
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestClientAutoSelfIDRetriesAfterLoginFailure(t *testing.T) {
	for _, response := range []struct {
		name  string
		write func(context.Context, *websocket.Conn, string) error
	}{
		{
			name: "failed response",
			write: func(ctx context.Context, conn *websocket.Conn, echo string) error {
				return wsjson.Write(ctx, conn, ActionResponse{Status: "failed", RetCode: 1, Echo: echo})
			},
		},
		{
			name: "malformed response",
			write: func(ctx context.Context, conn *websocket.Conn, echo string) error {
				return wsjson.Write(ctx, conn, map[string]any{"status": "ok", "echo": echo})
			},
		},
	} {
		t.Run(response.name, func(t *testing.T) {
			var connections atomic.Int32
			server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
				connection := connections.Add(1)
				var action ActionRequest
				if wsjson.Read(ctx, conn, &action) != nil {
					return
				}
				if connection == 1 {
					if wsjson.Write(ctx, conn, groupEventForSelf(1, "42", nil)) != nil {
						return
					}
					_ = response.write(ctx, conn, action.Echo)
					return
				}
				if wsjson.Write(ctx, conn, loginInfoResponse(action.Echo, "42")) != nil {
					return
				}
				_ = wsjson.Write(ctx, conn, groupEventForSelf(2, "42", nil))
				<-ctx.Done()
			})
			defer server.Close()

			client := &Client{URL: webSocketURL(server.URL), Token: "token", SelfID: "auto", MessageRunes: 1200}
			delays := make(chan time.Duration, 1)
			client.sleep = func(_ context.Context, delay time.Duration) error {
				delays <- delay
				return nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runErr := make(chan error, 1)
			messages := make(chan GroupMessage, 1)
			go func() {
				runErr <- client.Run(ctx, func(_ context.Context, message GroupMessage) error {
					messages <- message
					return nil
				})
			}()

			if got := receive(t, delays); got != time.Second {
				t.Fatalf("reconnect delay = %s, want %s", got, time.Second)
			}
			if message := receive(t, messages); message.MessageID != "2" {
				t.Fatalf("message ID = %q, want 2", message.MessageID)
			}
			cancel()
			if err := receive(t, runErr); err != nil {
				t.Fatalf("Run returned %v", err)
			}
		})
	}
}

func TestClientAutoSelfIDTimeoutRetriesWithoutDispatch(t *testing.T) {
	var connections atomic.Int32
	actions := make(chan ActionRequest, 2)
	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		connections.Add(1)
		var action ActionRequest
		if wsjson.Read(ctx, conn, &action) != nil {
			return
		}
		actions <- action
		_ = wsjson.Write(ctx, conn, groupEventForSelf(1, "42", nil))
		var ignored json.RawMessage
		_ = wsjson.Read(ctx, conn, &ignored)
	})
	defer server.Close()

	client := &Client{URL: webSocketURL(server.URL), Token: "token", SelfID: "auto", MessageRunes: 1200}
	client.identifyTimeout = 20 * time.Millisecond
	delays := make(chan time.Duration, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.sleep = func(ctx context.Context, delay time.Duration) error {
		delays <- delay
		if len(delays) == 2 {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	runErr := make(chan error, 1)
	messages := make(chan GroupMessage, 1)
	go func() {
		runErr <- client.Run(ctx, func(_ context.Context, message GroupMessage) error {
			messages <- message
			return nil
		})
	}()

	for range 2 {
		if action := receive(t, actions); action.Action != "get_login_info" {
			t.Fatalf("action = %q, want get_login_info", action.Action)
		}
	}
	if got := receive(t, delays); got != time.Second {
		t.Fatalf("first reconnect delay = %s, want %s", got, time.Second)
	}
	if got := receive(t, delays); got != 2*time.Second {
		t.Fatalf("second reconnect delay = %s, want %s", got, 2*time.Second)
	}
	if got := connections.Load(); got < 2 {
		t.Fatalf("connections = %d, want at least 2", got)
	}
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	select {
	case message := <-messages:
		t.Fatalf("unexpected message: %#v", message)
	default:
	}
}

func TestClientIdentityFailureDoesNotResetBackoff(t *testing.T) {
	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		var action ActionRequest
		if wsjson.Read(ctx, conn, &action) != nil {
			return
		}
		_ = wsjson.Write(ctx, conn, ActionResponse{Status: "failed", RetCode: 1, Echo: action.Echo})
	})
	defer server.Close()

	client := &Client{URL: webSocketURL(server.URL), Token: "token", SelfID: "auto", MessageRunes: 1200}
	var nowCalls atomic.Int32
	client.now = func() time.Time {
		return time.Unix(int64(nowCalls.Add(1))*31, 0)
	}
	delays := make(chan time.Duration, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.sleep = func(ctx context.Context, delay time.Duration) error {
		delays <- delay
		if len(delays) == 2 {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	runErr := make(chan error, 1)
	go func() { runErr <- client.Run(ctx, func(context.Context, GroupMessage) error { return nil }) }()

	if got := receive(t, delays); got != time.Second {
		t.Fatalf("first reconnect delay = %s, want %s", got, time.Second)
	}
	if got := receive(t, delays); got != 2*time.Second {
		t.Fatalf("second reconnect delay = %s, want %s", got, 2*time.Second)
	}
	if got := nowCalls.Load(); got != 0 {
		t.Fatalf("clock calls after failed identification = %d, want 0", got)
	}
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestClientIdentityHealthBeginsAfterSuccessfulDiscovery(t *testing.T) {
	login := make(chan ActionRequest, 1)
	respond := make(chan struct{})
	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		var action ActionRequest
		if wsjson.Read(ctx, conn, &action) != nil {
			return
		}
		login <- action
		select {
		case <-respond:
		case <-ctx.Done():
			return
		}
		_ = wsjson.Write(ctx, conn, loginInfoResponse(action.Echo, "42"))
	})
	defer server.Close()

	var clock atomic.Int64
	client := &Client{URL: webSocketURL(server.URL), Token: "token", SelfID: "auto", MessageRunes: 1200}
	client.now = func() time.Time { return time.Unix(clock.Load(), 0) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan time.Time, 1)
	go func() {
		served <- client.serve(ctx, clientConfig{url: client.URL, token: client.Token, messageRunes: client.MessageRunes}, make(chan GroupMessage))
	}()

	receive(t, login)
	clock.Store(10)
	close(respond)
	if got, want := receive(t, served), time.Unix(10, 0); !got.Equal(want) {
		t.Fatalf("connectedAt = %s, want %s", got, want)
	}
}

func TestClientFixedSelfIDDoesNotRequestLoginInfo(t *testing.T) {
	actions := make(chan ActionRequest, 1)
	server := newWebSocketServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		if wsjson.Write(ctx, conn, groupEventForSelf(1, "4", nil)) != nil {
			return
		}
		var action ActionRequest
		if wsjson.Read(ctx, conn, &action) != nil {
			return
		}
		actions <- action
		_ = wsjson.Write(ctx, conn, ActionResponse{Status: "ok", RetCode: 0, Echo: action.Echo})
		<-ctx.Done()
	})
	defer server.Close()

	client := &Client{URL: webSocketURL(server.URL), Token: "token", SelfID: "4", MessageRunes: 1200}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	messages := make(chan GroupMessage, 1)
	go func() {
		runErr <- client.Run(ctx, func(_ context.Context, message GroupMessage) error {
			messages <- message
			return nil
		})
	}()
	receive(t, messages)
	if err := client.Send(context.Background(), "2", "fixed"); err != nil {
		t.Fatal(err)
	}
	if got := receive(t, actions); got.Action != "send_group_msg" {
		t.Fatalf("first action = %q, want send_group_msg", got.Action)
	}
	cancel()
	if err := receive(t, runErr); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestClientValidateAutoSelfID(t *testing.T) {
	client := &Client{URL: "ws://127.0.0.1:1", Token: "token", SelfID: "auto", MessageRunes: 1200}
	cfg, err := client.validate(func(context.Context, GroupMessage) error { return nil })
	if err != nil || cfg.selfID != "" || client.SelfID != "auto" {
		t.Fatalf("validate() = %#v, %v; Client.SelfID = %q", cfg, err, client.SelfID)
	}
	for _, selfID := range []string{"", "+1", "-1", "abc", "18446744073709551616"} {
		client.SelfID = selfID
		if _, err := client.validate(func(context.Context, GroupMessage) error { return nil }); err == nil {
			t.Fatalf("validate with self ID %q succeeded", selfID)
		}
	}
}

func groupEvent(messageID int) map[string]any {
	return map[string]any{
		"post_type": "message", "message_type": "group",
		"message_id": messageID, "group_id": 2, "user_id": 3, "self_id": 4,
		"message": []any{map[string]any{"type": "text", "data": map[string]any{"text": fmt.Sprintf("event-%d", messageID)}}},
	}
}

func groupEventForSelf(messageID int, selfID string, message []any) map[string]any {
	if message == nil {
		message = []any{map[string]any{"type": "text", "data": map[string]any{"text": fmt.Sprintf("event-%d", messageID)}}}
	}
	return map[string]any{
		"post_type": "message", "message_type": "group",
		"message_id": messageID, "group_id": 2, "user_id": 3, "self_id": selfID,
		"message": message,
	}
}

func loginInfoResponse(echo, selfID string) ActionResponse {
	return ActionResponse{
		Status: "ok", RetCode: 0, Echo: echo,
		Data: json.RawMessage(fmt.Sprintf(`{"user_id":%q,"nickname":"bot"}`, selfID)),
	}
}

func newWebSocketServer(t *testing.T, handle func(context.Context, *websocket.Conn, *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		conn, err := websocket.Accept(response, request, nil)
		if err != nil {
			t.Errorf("accept WebSocket: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
		defer cancel()
		handle(ctx, conn, request)
	}))
}

func webSocketURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func runTestClient(t *testing.T, serverURL string) (*Client, context.CancelFunc, <-chan error) {
	t.Helper()
	client := &Client{URL: webSocketURL(serverURL), Token: "token", SelfID: "4", MessageRunes: 1200}
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- client.Run(ctx, func(context.Context, GroupMessage) error { return nil }) }()
	return client, cancel, runErr
}

func waitForConnection(t *testing.T, client *Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		client.mu.Lock()
		connected := client.active != nil
		client.mu.Unlock()
		if connected {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("client did not connect")
}

func actionText(t *testing.T, action ActionRequest) string {
	t.Helper()
	if len(action.Params.Message) != 1 {
		t.Fatalf("action has %d message segments, want 1", len(action.Params.Message))
	}
	var text string
	if err := json.Unmarshal(action.Params.Message[0].Data["text"], &text); err != nil {
		t.Fatalf("decode action text: %v", err)
	}
	return text
}

func clientPending(client *Client) int {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.active == nil {
		return 0
	}
	client.active.mu.Lock()
	defer client.active.mu.Unlock()
	return len(client.active.pending)
}

func waitForPending(t *testing.T, client *Client, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if clientPending(client) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pending actions = %d, want %d", clientPending(client), want)
}

func receive[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for value")
		var zero T
		return zero
	}
}
