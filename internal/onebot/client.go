package onebot

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	maxEventBytes       = 1 << 20
	eventQueueSize      = 64
	defaultHealthyAfter = 30 * time.Second
)

var (
	// ErrDisconnected lets callers decide whether an action is safe to retry.
	ErrDisconnected  = errors.New("onebot websocket is disconnected")
	errEchoCollision = errors.New("onebot action echo collision")
	reconnectDelays  = [...]time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second}
)

// Handler receives decoded group messages serially and must return when its
// context is canceled. Its error affects only that message.
type Handler func(context.Context, GroupMessage) error

// Client maintains one authenticated OneBot WebSocket connection.
type Client struct {
	URL, Token, SelfID string
	MessageRunes       int

	mu      sync.Mutex
	running bool
	active  *clientSession

	sleep        func(context.Context, time.Duration) error
	now          func() time.Time
	healthyAfter time.Duration
}

type clientConfig struct {
	url, token, selfID string
	messageRunes       int
}

type clientSession struct {
	conn         *websocket.Conn
	ctx          context.Context
	cancel       context.CancelFunc
	messageRunes int
	writes       chan outboundAction
	writerDone   chan struct{}

	mu      sync.Mutex
	pending map[string]chan error
	failure error
	stop    sync.Once
}

type outboundAction struct {
	ctx     context.Context
	request ActionRequest
}

// Run connects to OneBot and reconnects until ctx is canceled. A Client can
// have only one Run call active at a time.
func (c *Client) Run(ctx context.Context, handler Handler) error {
	if ctx == nil {
		return errors.New("onebot client context is required")
	}
	cfg, err := c.validate(handler)
	if err != nil {
		return err
	}
	if err := c.start(); err != nil {
		return err
	}
	defer c.finish()
	events := make(chan GroupMessage, eventQueueSize)
	handlerDone := make(chan struct{})
	go handleMessages(ctx, handler, events, handlerDone)
	defer func() {
		close(events)
		<-handlerDone
	}()

	sleep := c.sleep
	if sleep == nil {
		sleep = sleepContext
	}
	now := c.now
	if now == nil {
		now = time.Now
	}
	healthyAfter := c.healthyAfter
	if healthyAfter <= 0 {
		healthyAfter = defaultHealthyAfter
	}

	retry := 0
	for {
		connectedAt := c.serve(ctx, cfg, events)
		if ctx.Err() != nil {
			return nil
		}
		if !connectedAt.IsZero() && now().Sub(connectedAt) >= healthyAfter {
			retry = 0
		}
		delay := reconnectDelays[min(retry, len(reconnectDelays)-1)]
		if retry < len(reconnectDelays)-1 {
			retry++
		}
		if err := sleep(ctx, delay); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("wait before OneBot reconnect: %w", err)
		}
	}
}

// Send sends text to a group in rune-bounded array-format segments. Every
// segment must receive a successful action response before the next is sent.
func (c *Client) Send(ctx context.Context, groupID, text string) error {
	if ctx == nil {
		return errors.New("onebot send context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	session := c.active
	c.mu.Unlock()
	if session == nil {
		return ErrDisconnected
	}

	for _, part := range SplitMessage(text, session.messageRunes) {
		for {
			echo, err := randomEcho()
			if err != nil {
				return fmt.Errorf("create OneBot action echo: %w", err)
			}
			action, err := SendGroupAction(groupID, part, echo)
			if err != nil {
				return err
			}
			err = session.request(ctx, action)
			if errors.Is(err, errEchoCollision) {
				continue
			}
			if err != nil {
				return err
			}
			break
		}
	}
	return nil
}

func (c *Client) validate(handler Handler) (clientConfig, error) {
	if c == nil {
		return clientConfig{}, errors.New("onebot client is required")
	}
	if handler == nil {
		return clientConfig{}, errors.New("onebot message handler is required")
	}
	if c.Token == "" {
		return clientConfig{}, errors.New("onebot access token is required")
	}
	parsed, err := url.Parse(c.URL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "ws" && parsed.Scheme != "wss") || parsed.User != nil {
		return clientConfig{}, errors.New("onebot WebSocket URL is invalid")
	}
	selfID, err := resolveConfiguredSelfID(c.SelfID)
	if err != nil {
		return clientConfig{}, errors.New("onebot self ID is invalid")
	}
	if c.MessageRunes <= 0 {
		return clientConfig{}, errors.New("onebot message rune limit must be positive")
	}
	return clientConfig{url: c.URL, token: c.Token, selfID: selfID, messageRunes: c.MessageRunes}, nil
}

func resolveConfiguredSelfID(value string) (string, error) {
	if value == "auto" {
		return "", nil
	}
	return normalizeID(value)
}

func (c *Client) start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		return errors.New("onebot client is already running")
	}
	c.running = true
	return nil
}

func (c *Client) finish() {
	c.mu.Lock()
	session := c.active
	c.active = nil
	c.running = false
	c.mu.Unlock()
	if session != nil {
		session.shutdown(ErrDisconnected)
	}
}

func (c *Client) serve(ctx context.Context, cfg clientConfig, events chan<- GroupMessage) time.Time {
	header := make(http.Header, 1)
	header.Set("Authorization", "Bearer "+cfg.token)
	conn, _, err := websocket.Dial(ctx, cfg.url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return time.Time{}
	}
	conn.SetReadLimit(maxEventBytes)
	connectedAt := c.currentTime()
	resolvedSelfID := cfg.selfID
	if resolvedSelfID == "" {
		loginData, err := performAction(ctx, conn, LoginInfoAction)
		if err != nil {
			conn.CloseNow()
			return connectedAt
		}
		selfID, _, err := decodeLoginInfo(loginData)
		if err != nil {
			conn.CloseNow()
			return connectedAt
		}
		resolvedSelfID = string(selfID)
	}
	session := newClientSession(ctx, conn, cfg.messageRunes)

	c.mu.Lock()
	if ctx.Err() == nil {
		c.active = session
	}
	c.mu.Unlock()
	if ctx.Err() != nil {
		session.shutdown(ErrDisconnected)
		return connectedAt
	}

	go session.writeLoop()
	defer func() {
		c.mu.Lock()
		if c.active == session {
			c.active = nil
		}
		c.mu.Unlock()
		session.shutdown(ErrDisconnected)
		<-session.writerDone
	}()

	for {
		var raw json.RawMessage
		if err := wsjson.Read(session.ctx, conn, &raw); err != nil {
			session.shutdown(fmt.Errorf("%w: connection closed", ErrDisconnected))
			return connectedAt
		}
		if session.handleResponse(raw) {
			continue
		}
		message, accepted, err := DecodeGroupMessage(raw, resolvedSelfID)
		if err != nil || !accepted {
			continue
		}
		select {
		case events <- message:
			continue
		default:
			session.shutdown(fmt.Errorf("%w: message handler backlog is full", ErrDisconnected))
			select {
			case events <- message:
			case <-ctx.Done():
			}
			return connectedAt
		}
	}
}

func (c *Client) currentTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func newClientSession(parent context.Context, conn *websocket.Conn, messageRunes int) *clientSession {
	ctx, cancel := context.WithCancel(parent)
	return &clientSession{
		conn: conn, ctx: ctx, cancel: cancel, messageRunes: messageRunes,
		writes: make(chan outboundAction, 64), writerDone: make(chan struct{}), pending: make(map[string]chan error),
	}
}

func handleMessages(ctx context.Context, handler Handler, events <-chan GroupMessage, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case message, ok := <-events:
			if !ok || ctx.Err() != nil {
				return
			}
			_ = handler(ctx, message)
		}
	}
}

func (s *clientSession) request(ctx context.Context, request ActionRequest) error {
	result := make(chan error, 1)
	if err := s.register(request.Echo, result); err != nil {
		return err
	}
	defer s.remove(request.Echo, result)

	outbound := outboundAction{ctx: ctx, request: request}
	select {
	case s.writes <- outbound:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return s.sessionError()
	}

	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return s.sessionError()
	}
}

func (s *clientSession) register(echo string, result chan error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure != nil {
		return s.failure
	}
	if _, exists := s.pending[echo]; exists {
		return errEchoCollision
	}
	s.pending[echo] = result
	return nil
}

func (s *clientSession) remove(echo string, result chan error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending[echo] == result {
		delete(s.pending, echo)
	}
}

func (s *clientSession) complete(echo string, err error) {
	s.mu.Lock()
	result := s.pending[echo]
	if result != nil {
		delete(s.pending, echo)
	}
	s.mu.Unlock()
	if result != nil {
		result <- err
	}
}

func (s *clientSession) writeLoop() {
	defer close(s.writerDone)
	for {
		select {
		case outbound := <-s.writes:
			if err := outbound.ctx.Err(); err != nil {
				s.complete(outbound.request.Echo, err)
				continue
			}
			if !s.isPending(outbound.request.Echo) {
				continue
			}
			if err := wsjson.Write(s.ctx, s.conn, outbound.request); err != nil {
				s.shutdown(fmt.Errorf("%w: write failed", ErrDisconnected))
				return
			}
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *clientSession) isPending(echo string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, exists := s.pending[echo]
	return exists
}

func (s *clientSession) handleResponse(raw []byte) bool {
	var envelope struct {
		PostType json.RawMessage `json:"post_type"`
		Status   json.RawMessage `json:"status"`
		RetCode  json.RawMessage `json:"retcode"`
		Echo     json.RawMessage `json:"echo"`
	}
	if json.Unmarshal(raw, &envelope) != nil || hasPostType(envelope.PostType) || len(envelope.Echo) == 0 {
		return false
	}
	var echo string
	if json.Unmarshal(envelope.Echo, &echo) != nil || echo == "" {
		return true
	}
	if !s.isPending(echo) {
		return true
	}
	status, retCode, err := decodeActionResult(envelope.Status, envelope.RetCode)
	if err != nil {
		s.complete(echo, fmt.Errorf("malformed OneBot action response: %w", err))
		return true
	}

	var response ActionResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		s.complete(echo, fmt.Errorf("malformed OneBot action response: %w", err))
		return true
	}
	if status == "ok" && retCode == 0 {
		s.complete(echo, nil)
		return true
	}
	detail := response.Wording
	if detail == "" {
		detail = response.Message
	}
	if detail == "" {
		detail = "request failed"
	}
	s.complete(echo, fmt.Errorf("onebot action failed: status=%q retcode=%d: %s", status, retCode, detail))
	return true
}

func hasPostType(raw json.RawMessage) bool {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	var postType string
	if json.Unmarshal(raw, &postType) == nil {
		return postType != ""
	}
	return true
}

func decodeActionResult(statusRaw, retCodeRaw json.RawMessage) (string, int, error) {
	if len(statusRaw) == 0 || bytes.Equal(bytes.TrimSpace(statusRaw), []byte("null")) {
		return "", 0, errors.New("status is required")
	}
	var status string
	if err := json.Unmarshal(statusRaw, &status); err != nil || status == "" {
		return "", 0, errors.New("status must be a non-empty string")
	}
	if len(retCodeRaw) == 0 || bytes.Equal(bytes.TrimSpace(retCodeRaw), []byte("null")) {
		return "", 0, errors.New("retcode is required")
	}
	var retCode int
	if err := json.Unmarshal(retCodeRaw, &retCode); err != nil {
		return "", 0, errors.New("retcode must be an integer")
	}
	return status, retCode, nil
}

func (s *clientSession) shutdown(err error) {
	if err == nil {
		err = ErrDisconnected
	}
	s.stop.Do(func() {
		s.mu.Lock()
		s.failure = err
		pending := make([]chan error, 0, len(s.pending))
		for echo, result := range s.pending {
			pending = append(pending, result)
			delete(s.pending, echo)
		}
		s.mu.Unlock()

		s.cancel()
		for _, result := range pending {
			result <- err
		}
		_ = s.conn.CloseNow()
	})
}

func (s *clientSession) sessionError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure != nil {
		return s.failure
	}
	return ErrDisconnected
}

func randomEcho() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
