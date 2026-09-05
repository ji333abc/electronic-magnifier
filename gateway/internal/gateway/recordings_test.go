package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecordingsEmptyAndUnavailable(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/list" || r.URL.Query().Get("path") != "ipc-main" {
				t.Errorf("unexpected list request: %s", r.URL)
			}
			w.WriteHeader(status)
		}))
		target, _ := url.Parse(upstream.URL)
		server := &Server{config: Config{MainStream: "ipc-main"}, playbackURL: target, playbackClient: upstream.Client()}
		response := httptest.NewRecorder()
		server.handleRecordings(response, httptest.NewRequest(http.MethodGet,
			"/api/recordings?start=2026-08-15T00:00:00Z&end=2026-08-16T00:00:00Z", nil), "test")
		upstream.Close()
		if status == http.StatusNotFound {
			var result struct {
				Recordings []recordingClip `json:"recordings"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != http.StatusOK || result.Recordings == nil || len(result.Recordings) != 0 {
				t.Fatalf("expected empty list, got %d %s", response.Code, response.Body.String())
			}
		} else if response.Code != http.StatusBadGateway {
			t.Fatalf("upstream failure was hidden: %d", response.Code)
		}
	}
}

func TestRecordingPlaybackProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if r.URL.Path != "/get" || query.Get("format") != "mp4" || query.Get("path") != "ipc-main" || query.Get("duration") != "60.000" || query.Get("start") != "2026-08-15T00:00:00Z" {
			t.Errorf("unexpected playback request: %s", r.URL)
		}
		if r.Header.Get("Range") != "" {
			t.Error("range must be handled by the gateway, not forwarded to MediaMTX")
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Accept-Ranges", "none")
		_, _ = w.Write([]byte("test"))
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	server := &Server{config: Config{MainStream: "ipc-main"}, playbackURL: target, playbackClient: upstream.Client()}
	configurePlaybackTestCache(t, server)
	for _, duration := range []string{"NaN", "+Inf", "-1", "0", "602", "60"} {
		query := url.Values{"start": {"2026-08-15T00:00:00Z"}, "duration": {duration}}
		request := httptest.NewRequest(http.MethodGet, "/api/recordings/play?"+query.Encode(), nil)
		request.Header.Set("Range", "bytes=0-3")
		response := httptest.NewRecorder()
		server.handleRecordingPlayback(response, request, "test")
		if duration != "60" {
			if response.Code != http.StatusBadRequest {
				t.Errorf("accepted invalid duration %s: %d", duration, response.Code)
			}
		} else if response.Code != http.StatusPartialContent || response.Body.String() != "test" || response.Header().Get("Content-Range") != "bytes 0-3/4" || response.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatalf("unexpected playback response: %#v", response)
		}
	}
}

func configurePlaybackTestCache(t *testing.T, server *Server) {
	t.Helper()
	server.playbackCache.dir = t.TempDir()
	t.Cleanup(func() {
		cache := &server.playbackCache
		cache.mu.Lock()
		defer cache.mu.Unlock()
		for key, entry := range cache.entries {
			cache.removeLocked(key, entry)
		}
	})
}

func TestRecordingPlaybackSeeking(t *testing.T) {
	content := "0123456789abcdefghijklmnopqrstuvwxyz"
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.Header.Get("Range") != "" {
			t.Errorf("unexpected upstream request: %s %v", r.Method, r.Header)
		}
		// Match MediaMTX: no Content-Length and no byte-range support.
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Accept-Ranges", "none")
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte(content))
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	server := &Server{config: Config{MainStream: "ipc-main"}, playbackURL: target, playbackClient: upstream.Client()}
	configurePlaybackTestCache(t, server)
	for _, tc := range []struct {
		name, method, byteRange, body, contentRange string
		status                                    int
	}{
		{"initial", "GET", "bytes=0-1", "01", "bytes 0-1/36", 206},
		{"seek forward", "GET", "bytes=20-25", "klmnop", "bytes 20-25/36", 206},
		{"seek backward", "GET", "bytes=5-8", "5678", "bytes 5-8/36", 206},
		{"suffix", "GET", "bytes=-3", "xyz", "bytes 33-35/36", 206},
		{"open ended", "GET", "bytes=30-", "uvwxyz", "bytes 30-35/36", 206},
		{"whole clip", "GET", "", content, "", 200},
		{"head", "HEAD", "", "", "", 200},
		{"out of bounds", "GET", "bytes=100-", "", "bytes */36", 416},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/api/recordings/play?start=2026-08-15T00:00:00Z&duration=60", nil)
			r.Header.Set("Range", tc.byteRange)
			w := httptest.NewRecorder()
			server.handleRecordingPlayback(w, r, "test")
			if w.Code != tc.status || w.Header().Get("Content-Range") != tc.contentRange || w.Header().Get("Cache-Control") != "private, no-store" {
				t.Fatalf("unexpected response: %d %v", w.Code, w.Header())
			}
			if tc.status < 400 && (w.Body.String() != tc.body || w.Header().Get("Accept-Ranges") != "bytes" || w.Header().Get("Cache-Control") != "private, no-store") {
				t.Fatalf("incorrect range body or headers: %q %v", w.Body.String(), w.Header())
			}
		})
	}
	for _, stale := range []bool{false, true} {
		r := httptest.NewRequest("GET", "/api/recordings/play?start=2026-08-15T00:00:00Z&duration=60", nil)
		initial := httptest.NewRecorder()
		server.handleRecordingPlayback(initial, r, "test")
		r.Header.Set("Range", "bytes=20-25")
		r.Header.Set("If-Range", initial.Header().Get("ETag"))
		wantBody, wantStatus := "klmnop", http.StatusPartialContent
		if stale {
			r.Header.Set("If-Range", "\"older-representation\"")
			wantBody, wantStatus = content, http.StatusOK
		}
		w := httptest.NewRecorder()
		server.handleRecordingPlayback(w, r, "test")
		if w.Code != wantStatus || w.Body.String() != wantBody {
			t.Fatalf("If-Range mismatch handling is incorrect: %d %q", w.Code, w.Body.String())
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("seeking regenerated the clip %d times", calls.Load())
	}
}

func TestRecordingPlaybackFailedPreparation(t *testing.T) {
	for _, failure := range []string{"missing", "upstream error", "empty", "truncated", "oversized"} {
		t.Run(failure, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch failure {
				case "missing":
					w.WriteHeader(http.StatusNotFound)
				case "upstream error":
					w.WriteHeader(http.StatusInternalServerError)
				case "truncated":
					w.Header().Set("Content-Length", "100")
					_, _ = w.Write([]byte("short"))
				case "oversized":
					w.Header().Set("Content-Length", "536870913")
				}
			}))
			defer upstream.Close()
			target, _ := url.Parse(upstream.URL)
			server := &Server{config: Config{MainStream: "ipc-main"}, playbackURL: target, playbackClient: upstream.Client()}
			configurePlaybackTestCache(t, server)
			w := httptest.NewRecorder()
			server.handleRecordingPlayback(w, httptest.NewRequest("GET", "/api/recordings/play?start=2026-08-15T00:00:00Z&duration=60", nil), "test")
			want := http.StatusBadGateway
			if failure == "missing" {
				want = http.StatusNotFound
			}
			if w.Code != want || strings.HasPrefix(w.Header().Get("Content-Type"), "video/") {
				t.Fatalf("failed clip was served as video: %d %v", w.Code, w.Header())
			}
			files, err := os.ReadDir(server.playbackCache.dir)
			if err != nil || len(files) != 0 || len(server.playbackCache.entries) != 0 {
				t.Fatalf("failed preparation left temporary files or cache entries: %v %v", files, err)
			}
		})
	}
}

func TestRecordingClipsSortedByInstant(t *testing.T) {
	start := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	clips := splitRecordingSpans([]recordingSpan{
		{Start: "2026-08-15T09:00:00+08:00", Duration: 60},
		{Start: "2026-08-15T02:00:00Z", Duration: 60},
		{Start: "2026-08-14T23:59:30Z", Duration: 60},
	}, start, start.Add(24*time.Hour))
	if len(clips) != 3 || clips[0].Start != "2026-08-15T02:00:00Z" || clips[2].Duration != 30 {
		t.Fatalf("incorrect ordering or window clipping: %#v", clips)
	}
}

func TestRecordingWindow(t *testing.T) {
	values := url.Values{
		"start": {"2026-08-15T00:00:00+08:00"},
		"end":   {"2026-08-16T00:00:00+08:00"},
	}
	start, end, err := recordingWindow(values)
	if err != nil || end.Sub(start) != 24*time.Hour {
		t.Fatalf("unexpected window: %v %v %v", start, end, err)
	}
	values.Set("end", "2026-08-17T03:00:00+08:00")
	if _, _, err := recordingWindow(values); err == nil {
		t.Fatal("oversized window was accepted")
	}
}

func TestSplitRecordingSpans(t *testing.T) {
	windowStart := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	windowEnd := windowStart.Add(24 * time.Hour)
	clips := splitRecordingSpans([]recordingSpan{{
		Start:    windowStart.Add(5 * time.Minute).Format(time.RFC3339),
		Duration: (25 * time.Minute).Seconds(),
	}}, windowStart, windowEnd)
	if len(clips) != 3 || clips[0].Duration != 5*60 || clips[2].Duration != 10*60 {
		t.Fatalf("unexpected clips: %#v", clips)
	}
}
