package gateway

import (
	"bytes"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRecordingPlaylist(t *testing.T) {
	s := &Server{}
	for _, duration := range []string{"NaN", "+Inf", "0", "-1", "602", "13"} {
		w := httptest.NewRecorder()
		query := url.Values{"start": {"2026-09-05T00:00:00Z"}, "duration": {duration}}
		s.handleRecordingPlayback(w, httptest.NewRequest("GET", "/api/recordings/play?"+query.Encode(), nil), "test")
		if duration != "13" {
			if w.Code != http.StatusBadRequest {
				t.Fatalf("accepted invalid duration %s", duration)
			}
			continue
		}
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, "#EXT-X-ENDLIST") || strings.Count(body, "#EXTINF:") != 3 || strings.Count(body, "#EXT-X-DISCONTINUITY\n") != 2 || w.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatalf("incorrect VOD playlist: %d %s", w.Code, body)
		}
		var media []string
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "/api/") {
				media = append(media, line)
			}
		}
		for index, line := range media {
			u, err := url.Parse(line)
			if err != nil {
				t.Fatal(err)
			}
			start, length, err := playbackWindow(u.Query(), 6)
			wantLength := 6.0
			if index == 2 { wantLength = 1 }
			if err != nil || start.Second() != index*6 || length != wantLength || u.Query().Get("kind") != "media" {
				t.Fatalf("incorrect seek segment: %s", line)
			}
		}
	}
}

func mp4TestBox(kind, payload string) []byte {
	box := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(box, uint32(len(box)))
	copy(box[4:8], kind)
	copy(box[8:], payload)
	return box
}

func TestRecordingSegmentStreamsBeforeUpstreamCompletes(t *testing.T) {
	init := append(mp4TestBox("ftyp", "isom"), mp4TestBox("moov", "init")...)
	media := mp4TestBox("moof", "part")
	finish := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "fmp4" || r.URL.Query().Get("duration") != "6.000000" || r.URL.Query().Get("path") != "ipc-main" || r.Header.Get("Range") != "" {
			t.Errorf("unexpected segment request: %s %v", r.URL, r.Header)
		}
		w.Write(init)
		w.Write(media)
		w.(http.Flusher).Flush()
		select {
		case <-finish:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	s := &Server{config: Config{MainStream: "ipc-main"}, playbackURL: target, playbackClient: upstream.Client()}
	gateway := httptest.NewServer(securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleRecordingSegment(w, r, "test") })))
	defer gateway.Close()
	defer close(finish)
	client := &http.Client{Timeout: 2*time.Second}
	for _, kind := range []string{"init", "media"} {
		response, err := client.Get(gateway.URL+"?start=2026-09-05T00:00:00Z&duration=6&kind="+kind)
		if err != nil { t.Fatal(err) }
		want := init
		if kind == "media" { want = media }
		data := make([]byte, len(want))
		_, err = io.ReadFull(response.Body, data)
		response.Body.Close()
		if err != nil || !bytes.Equal(data, want) || response.StatusCode != 200 || response.Header.Get("Cache-Control") != "private, no-store" {
			t.Fatalf("gateway buffered the full response or mixed init/media: %q %v", data, err)
		}
	}
}

func TestRecordingSegmentErrorsAndLimits(t *testing.T) {
	for _, status := range []int{404, 500, 200} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status); w.Write([]byte("not mp4")) }))
		target, _ := url.Parse(upstream.URL)
		s := &Server{config: Config{MainStream: "ipc-main"}, playbackURL: target, playbackClient: upstream.Client()}
		w := httptest.NewRecorder()
		s.handleRecordingSegment(w, httptest.NewRequest("GET", "/?start=2026-09-05T00:00:00Z&duration=6&kind=media", nil), "test")
		upstream.Close()
		want := 502
		if status == 404 { want = 404 }
		if w.Code != want || s.playbackActive.Load() != 0 { t.Fatalf("incorrect failure handling: %d", w.Code) }
	}
	s := &Server{}
	for _, query := range []string{"duration=600&kind=media", "duration=6&kind=invalid", "duration=NaN&kind=init"} {
		w := httptest.NewRecorder()
		s.handleRecordingSegment(w, httptest.NewRequest("GET", "/?start=2026-09-05T00:00:00Z&"+query, nil), "test")
		if w.Code != 400 { t.Fatalf("accepted invalid segment: %s", query) }
	}
	s.playbackActive.Store(4)
	w := httptest.NewRecorder()
	s.handleRecordingSegment(w, httptest.NewRequest("GET", "/?start=2026-09-05T00:00:00Z&duration=6&kind=media", nil), "test")
	if w.Code != 503 || s.playbackActive.Load() != 4 { t.Fatal("concurrency limit not enforced") }
}

func TestRecordingInitRejectsInvalidBoxes(t *testing.T) {
	for _, data := range [][]byte{nil, {0,0,0,0,'f','t','y','p'}, {255,255,255,255,'m','o','o','v'}, mp4TestBox("moof", "invalid")} {
		if _, err := readRecordingInit(bytes.NewReader(data)); err == nil { t.Fatal("invalid init accepted") }
	}
}

func TestRecordingHLSRequiresSession(t *testing.T) {
	s := &Server{sessions: newSessionStore("test-session-secret", time.Hour)}
	for _, path := range []string{"/api/recordings/play", "/api/recordings/segment"} {
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusUnauthorized || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unauthenticated recording access: %s %d", path, w.Code)
		}
	}
}
