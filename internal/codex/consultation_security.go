package codex

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/sys/unix"
)

const (
	maxConsultationConfigBytes = 64 << 10
	maxConsultationBodyBytes   = 8 << 20
	consultationResponsesPath  = "/v1/responses"
	consultationProviderName   = "qq_consultation_proxy"
)

type consultationConfig struct {
	Model                  string
	ReasoningEffort        string
	DisableResponseStorage bool
	BaseURL                *url.URL
}

type consultationSnapshot struct {
	file   *os.File
	config consultationConfig
}

func loadConsultationConfig(codexHome string) (*consultationSnapshot, error) {
	data, err := readPrivateConsultationConfig(codexHome)
	if err != nil {
		return nil, err
	}
	config, err := parseConsultationConfig(data)
	if err != nil {
		return nil, err
	}
	file, err := unix.MemfdCreate("qqcodex-consultation-config", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("create consultation config snapshot: %w", err)
	}
	snapshot := os.NewFile(uintptr(file), "consultation-config.toml")
	content := consultationConfigTOML(config, "http://127.0.0.1:0/v1")
	if _, err := snapshot.Write([]byte(content)); err != nil {
		_ = snapshot.Close()
		return nil, fmt.Errorf("write consultation config snapshot: %w", err)
	}
	if _, err := snapshot.Seek(0, io.SeekStart); err != nil {
		_ = snapshot.Close()
		return nil, fmt.Errorf("rewind consultation config snapshot: %w", err)
	}
	return &consultationSnapshot{file: snapshot, config: config}, nil
}

func (s *consultationSnapshot) setProxyURL(proxyURL string) error {
	if err := s.file.Truncate(0); err != nil {
		return fmt.Errorf("truncate consultation config snapshot: %w", err)
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind consultation config snapshot: %w", err)
	}
	if _, err := s.file.Write([]byte(consultationConfigTOML(s.config, proxyURL))); err != nil {
		return fmt.Errorf("write consultation proxy config: %w", err)
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind consultation proxy config: %w", err)
	}
	return nil
}

func readPrivateConsultationConfig(codexHome string) ([]byte, error) {
	if !filepath.IsAbs(codexHome) {
		return nil, errors.New("Codex home must be absolute")
	}
	rootFD, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open filesystem root: %w", err)
	}
	defer unix.Close(rootFD)
	homeFD, err := unix.Openat2(rootFD, strings.TrimPrefix(filepath.Clean(codexHome), "/"), &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, fmt.Errorf("open Codex home without symlinks: %w", err)
	}
	defer unix.Close(homeFD)
	var homeStat unix.Stat_t
	if err := unix.Fstat(homeFD, &homeStat); err != nil {
		return nil, fmt.Errorf("stat Codex home: %w", err)
	}
	if homeStat.Uid != uint32(os.Getuid()) || homeStat.Mode&0o077 != 0 {
		return nil, errors.New("Codex home is not private")
	}
	fd, err := unix.Openat2(homeFD, "config.toml", &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, fmt.Errorf("open Codex config.toml without symlinks: %w", err)
	}
	file := os.NewFile(uintptr(fd), "config.toml")
	defer file.Close()
	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return nil, fmt.Errorf("stat Codex config.toml: %w", err)
	}
	if before.Uid != uint32(os.Getuid()) || before.Mode&unix.S_IFMT != unix.S_IFREG || before.Nlink != 1 || before.Mode&0o077 != 0 || before.Size < 0 || before.Size > maxConsultationConfigBytes {
		return nil, errors.New("Codex config.toml is not a private regular file")
	}
	data := make([]byte, before.Size)
	if _, err := io.ReadFull(file, data); err != nil {
		return nil, fmt.Errorf("read Codex config.toml: %w", err)
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		return nil, fmt.Errorf("restat Codex config.toml: %w", err)
	}
	if before.Dev != after.Dev || before.Ino != after.Ino || before.Size != after.Size || before.Mtim != after.Mtim || before.Ctim != after.Ctim || before.Nlink != after.Nlink {
		return nil, errors.New("Codex config.toml changed while reading")
	}
	return data, nil
}

func parseConsultationConfig(data []byte) (consultationConfig, error) {
	var root map[string]any
	if err := toml.Unmarshal(data, &root); err != nil {
		return consultationConfig{}, fmt.Errorf("decode Codex config.toml: %w", err)
	}
	model, ok := root["model"].(string)
	if !ok || model == "" {
		return consultationConfig{}, errors.New("Codex config.toml requires model")
	}
	providerName, ok := root["model_provider"].(string)
	if !ok || providerName == "" {
		return consultationConfig{}, errors.New("Codex config.toml requires model_provider")
	}
	reasoning, ok := root["model_reasoning_effort"].(string)
	if !ok || reasoning == "" {
		return consultationConfig{}, errors.New("Codex config.toml requires model_reasoning_effort")
	}
	disableStorage, ok := root["disable_response_storage"].(bool)
	if !ok {
		return consultationConfig{}, errors.New("Codex config.toml requires disable_response_storage")
	}
	providers, ok := root["model_providers"].(map[string]any)
	if !ok {
		return consultationConfig{}, errors.New("Codex config.toml requires model_providers")
	}
	provider, ok := providers[providerName].(map[string]any)
	if !ok {
		return consultationConfig{}, errors.New("Codex config.toml active provider is missing")
	}
	for key := range provider {
		switch key {
		case "name", "wire_api", "base_url", "requires_openai_auth":
		default:
			return consultationConfig{}, fmt.Errorf("Codex provider field %q is not allowed for consultation", key)
		}
	}
	if wire, ok := provider["wire_api"].(string); !ok || wire != "responses" {
		return consultationConfig{}, errors.New("Codex provider must use responses wire API")
	}
	if auth, ok := provider["requires_openai_auth"].(bool); !ok || !auth {
		return consultationConfig{}, errors.New("Codex provider must require OpenAI authentication")
	}
	base, ok := provider["base_url"].(string)
	if !ok {
		return consultationConfig{}, errors.New("Codex provider requires base_url")
	}
	parsed, err := parseConsultationBaseURL(base)
	if err != nil {
		return consultationConfig{}, err
	}
	return consultationConfig{Model: model, ReasoningEffort: reasoning, DisableResponseStorage: disableStorage, BaseURL: parsed}, nil
}

func parseConsultationBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("Codex provider base_url must be an absolute HTTPS URL without userinfo, query, or fragment")
	}
	clean := pathpkg.Clean("/" + strings.TrimPrefix(u.Path, "/"))
	if clean == "/." {
		clean = "/"
	}
	if len(clean) > 512 || u.EscapedPath() != "" && u.Path != clean {
		return nil, errors.New("Codex provider base_url has an invalid path")
	}
	u.Path, u.RawPath = clean, ""
	return u, nil
}

func consultationConfigTOML(config consultationConfig, proxyURL string) string {
	return "model = " + strconv.Quote(config.Model) + "\n" +
		"model_provider = " + strconv.Quote(consultationProviderName) + "\n" +
		"model_reasoning_effort = " + strconv.Quote(config.ReasoningEffort) + "\n" +
		"disable_response_storage = " + strconv.FormatBool(config.DisableResponseStorage) + "\n\n" +
		"[model_providers." + consultationProviderName + "]\n" +
		"name = \"QQ consultation proxy\"\n" +
		"wire_api = \"responses\"\n" +
		"base_url = " + strconv.Quote(proxyURL) + "\n" +
		"requires_openai_auth = true\n"
}

type consultationProxy struct {
	mu       sync.RWMutex
	listener net.Listener
	server   *http.Server
	token    []byte
	baseURL  *url.URL
	apiKey   string
	client   *http.Client
}

func startConsultationProxy(ctx context.Context, baseURL *url.URL, apiKey string, token []byte, client *http.Client) (*consultationProxy, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for consultation proxy: %w", err)
	}
	if client == nil {
		client = &http.Client{Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: 30 * time.Second, TLSHandshakeTimeout: 10 * time.Second}}
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("consultation proxy redirects are not allowed")
	}
	client = &clientCopy
	p := &consultationProxy{listener: listener, token: append([]byte(nil), token...), baseURL: baseURL, apiKey: apiKey, client: client}
	p.server = &http.Server{Handler: http.HandlerFunc(p.serveHTTP), ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() { _ = p.server.Serve(listener) }()
	go func() { <-ctx.Done(); _ = p.Close() }()
	return p, nil
}

func (p *consultationProxy) URL() string { return "http://" + p.listener.Addr().String() }

func (p *consultationProxy) Close() error {
	p.mu.Lock()
	p.token = nil
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return p.server.Shutdown(ctx)
}

func (p *consultationProxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.RLock()
	token := append([]byte(nil), p.token...)
	p.mu.RUnlock()
	if r.Method != http.MethodPost || r.URL.Path != consultationResponsesPath || r.URL.RawQuery != "" || r.Host != p.listener.Addr().String() || !validConsultationBearer(r.Header.Get("Authorization"), token) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.ContentLength > maxConsultationBodyBytes {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxConsultationBodyBytes))
	if err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	upstream := *p.baseURL
	upstream.Path = pathpkg.Join(p.baseURL.Path, "responses")
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstream.String(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	request.Header.Set("Authorization", "Bearer "+p.apiKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if contentType := response.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(response.StatusCode)
	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			_, _ = w.Write(buffer[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

func validConsultationBearer(value string, token []byte) bool {
	provided, ok := strings.CutPrefix(value, "Bearer ")
	if !ok || len(provided) != len(token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), token) == 1
}

func newConsultationToken() ([]byte, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return nil, fmt.Errorf("generate consultation token: %w", err)
	}
	return []byte("qqc_" + base64.RawURLEncoding.EncodeToString(data)), nil
}
