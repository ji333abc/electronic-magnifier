package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		if r.Header.Get("Range") != "bytes=0-3" {
			t.Error("missing range header")
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 0-3/100")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("test"))
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	server := &Server{config: Config{MainStream: "ipc-main"}, playbackURL: target, playbackClient: upstream.Client()}
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
		} else if response.Code != http.StatusPartialContent || response.Body.String() != "test" || response.Header().Get("Content-Range") != "bytes 0-3/100" || response.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatalf("unexpected playback response: %#v", response)
		}
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
