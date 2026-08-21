package gateway

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var webFiles embed.FS

type streamHealth struct {
	Online bool `json:"online"`
}

type publicStatus struct {
	Gateway bool                    `json:"gateway"`
	IPC     bool                    `json:"ipc"`
	ESP     bool                    `json:"esp"`
	Go2RTC  bool                    `json:"go2rtc"`
	Streams map[string]streamHealth `json:"streams"`
	Motors  []ESPMotorStatus        `json:"motors"`
	Leases  map[int]string          `json:"leases"`
	Updated time.Time               `json:"updated"`
}

type healthCache struct {
	mu     sync.RWMutex
	status publicStatus
}

type Server struct {
	config         Config
	secrets        Secrets
	sessions       *sessionStore
	esp            *espClient
	hub            *controlHub
	health         healthCache
	http           *http.Server
	proxy          *httputil.ReverseProxy
	go2rtcURL      *url.URL
	playbackURL    *url.URL
	playbackClient *http.Client
	attemptMu      sync.Mutex
	attempts       map[string][]time.Time
}

func NewServer(config Config, secrets Secrets) (*Server, error) {
	target, err := url.Parse(config.Go2RTCURL)
	if err != nil {
		return nil, fmt.Errorf("parse go2rtc URL: %w", err)
	}
	playbackTarget, err := url.Parse(config.PlaybackURL)
	if err != nil {
		return nil, fmt.Errorf("parse playback URL: %w", err)
	}
	esp := newESPClient(config.ESPBaseURL, secrets.ESPAPIKey)
	server := &Server{
		config: config, secrets: secrets, sessions: newSessionStore(secrets.SessionSecret, config.SessionDuration()),
		esp: esp, attempts: make(map[string][]time.Time), go2rtcURL: target,
		playbackURL:    playbackTarget,
		playbackClient: &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 5 * time.Second}},
	}
	motorIDs := make([]int, 0, len(config.Motors))
	for _, motor := range config.Motors {
		motorIDs = append(motorIDs, motor.ID)
	}
	server.hub = newControlHub(esp, motorIDs)
	server.proxy = httputil.NewSingleHostReverseProxy(target)
	server.proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "视频网关暂时不可用", http.StatusBadGateway)
	}
	server.http = &http.Server{
		Addr: config.Listen, Handler: server.routes(), ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	server.health.status = publicStatus{Gateway: true, Streams: map[string]streamHealth{"main": {}, "sub": {}}, Updated: time.Now()}
	go server.monitor()
	return server, nil
}

func (s *Server) ListenAndServe() error {
	return s.http.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.hub.close()
	return s.http.Shutdown(ctx)
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.requireSession(s.handleLogout))
	mux.HandleFunc("GET /api/config", s.requireSession(s.handleConfig))
	mux.HandleFunc("GET /api/status", s.requireSession(s.handleStatus))
	mux.HandleFunc("GET /api/snapshot", s.requireSession(s.handleSnapshot))
	mux.HandleFunc("GET /api/recordings", s.requireSession(s.handleRecordings))
	mux.HandleFunc("GET /api/recordings/play", s.requireSession(s.handleRecordingPlayback))
	mux.HandleFunc("GET /api/control", s.requireSession(s.handleControlWS))
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) })
	mux.HandleFunc("/stream/", s.requireSession(s.handleStreamProxy))
	webRoot, _ := fs.Sub(webFiles, "web")
	mux.Handle("/", http.FileServer(http.FS(webRoot)))
	return securityHeaders(mux)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "origin_not_allowed"})
		return
	}
	ip := clientIP(r)
	if s.loginRateLimited(ip) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too_many_attempts"})
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if input.Username != s.secrets.AdminUser || !passwordMatches(s.secrets.AdminPasswordHash, input.Password) {
		s.recordLoginFailure(ip)
		time.Sleep(250 * time.Millisecond)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_credentials"})
		return
	}
	if err := s.sessions.create(w, r, input.Username); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "session_failed"})
		return
	}
	s.clearLoginFailures(ip)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, _ string) {
	if !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "origin_not_allowed"})
		return
	}
	s.sessions.remove(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request, _ string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"motors":  s.config.Motors,
		"streams": map[string]string{"main": s.config.MainStream, "sub": s.config.SubStream},
		"capabilities": map[string]bool{
			"motorLimits": true, "motorPosition": true, "autoFocus": false,
		},
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request, _ string) {
	s.health.mu.RLock()
	status := s.health.status
	s.health.mu.RUnlock()
	status.Leases = s.hub.leaseSnapshot()
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleControlWS(w http.ResponseWriter, r *http.Request, user string) {
	s.hub.serveWS(w, r, user)
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request, _ string) {
	target := strings.TrimRight(s.config.IPCBaseURL, "/") + "/cgi-bin/snapshot.cgi?stream=2"
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		http.Error(w, "快照地址无效", http.StatusInternalServerError)
		return
	}
	if s.secrets.IPCUser != "" {
		request.SetBasicAuth(s.secrets.IPCUser, s.secrets.IPCPassword)
	}
	client := &http.Client{Timeout: 4 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		http.Error(w, "IPC 快照暂时不可用", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		http.Error(w, "IPC 快照返回错误", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, io.LimitReader(response.Body, 5<<20))
}

func (s *Server) handleStreamProxy(w http.ResponseWriter, r *http.Request, _ string) {
	allowed := map[string]bool{
		"/stream/stream.html": true, "/stream/video-stream.js": true,
		"/stream/video-rtc.js": true, "/stream/api/ws": true,
		"/stream/api/frame.jpeg": true,
	}
	if !allowed[r.URL.Path] {
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/stream/api/") || r.URL.Path == "/stream/stream.html" {
		source := r.URL.Query().Get("src")
		if source != s.config.MainStream && source != s.config.SubStream {
			http.Error(w, "未知视频流", http.StatusBadRequest)
			return
		}
	}
	s.proxy.ServeHTTP(w, r)
}

func (s *Server) requireSession(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := s.sessions.user(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication_required"})
			return
		}
		next(w, r, user)
	}
}

func (s *Server) monitor() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		s.refreshHealth()
		select {
		case <-ticker.C:
		case <-s.hub.stop:
			return
		}
	}
}

func (s *Server) refreshHealth() {
	status := publicStatus{Gateway: true, Streams: map[string]streamHealth{"main": {}, "sub": {}}, Updated: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	espStatus, err := s.esp.status(ctx)
	cancel()
	if err == nil && espStatus.OK {
		status.ESP = true
		status.Motors = espStatus.Motors
	}

	if host := endpointHost(s.config.IPCBaseURL, "80"); host != "" {
		connection, err := net.DialTimeout("tcp", host, 500*time.Millisecond)
		if err == nil {
			status.IPC = true
			_ = connection.Close()
		}
	}

	client := &http.Client{Timeout: 700 * time.Millisecond}
	response, err := client.Get(strings.TrimRight(s.config.Go2RTCURL, "/") + "/stream/api/streams")
	if err == nil {
		defer response.Body.Close()
		if response.StatusCode/100 == 2 {
			status.Go2RTC = true
			var streams map[string]struct {
				Producers []json.RawMessage `json:"producers"`
			}
			if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&streams) == nil {
				if stream, ok := streams[s.config.MainStream]; ok {
					status.Streams["main"] = streamHealth{Online: len(stream.Producers) > 0}
				}
				if stream, ok := streams[s.config.SubStream]; ok {
					status.Streams["sub"] = streamHealth{Online: len(stream.Producers) > 0}
				}
			}
		}
	}
	s.health.mu.Lock()
	s.health.status = status
	s.health.mu.Unlock()
}

func endpointHost(rawURL, defaultPort string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	port := parsed.Port()
	if port == "" {
		port = defaultPort
	}
	return net.JoinHostPort(parsed.Hostname(), port)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/stream/") {
			w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; connect-src 'self' ws: wss:; media-src 'self' blob:; img-src 'self' data:; frame-ancestors 'self'")
		} else {
			w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self' ws: wss:; frame-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'self'")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if net.ParseIP(host).IsLoopback() {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			return strings.TrimSpace(strings.Split(forwarded, ",")[0])
		}
	}
	return host
}

func (s *Server) loginRateLimited(ip string) bool {
	s.attemptMu.Lock()
	defer s.attemptMu.Unlock()
	cutoff := time.Now().Add(-10 * time.Minute)
	recent := s.attempts[ip][:0]
	for _, attempt := range s.attempts[ip] {
		if attempt.After(cutoff) {
			recent = append(recent, attempt)
		}
	}
	s.attempts[ip] = recent
	return len(recent) >= 5
}

func (s *Server) recordLoginFailure(ip string) {
	s.attemptMu.Lock()
	s.attempts[ip] = append(s.attempts[ip], time.Now())
	s.attemptMu.Unlock()
}

func (s *Server) clearLoginFailures(ip string) {
	s.attemptMu.Lock()
	delete(s.attempts, ip)
	s.attemptMu.Unlock()
}
