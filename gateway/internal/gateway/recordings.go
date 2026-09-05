package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const recordingClipDuration = 10 * time.Minute

type recordingSpan struct {
	Start    string  `json:"start"`
	Duration float64 `json:"duration"`
}

type recordingClip struct {
	Start    string  `json:"start"`
	Duration float64 `json:"duration"`
}

func (s *Server) handleRecordings(w http.ResponseWriter, r *http.Request, _ string) {
	start, end, err := recordingWindow(r.URL.Query())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	target := playbackEndpoint(s.playbackURL, "/list")
	query := target.Query()
	query.Set("path", s.config.MainStream)
	query.Set("start", start.Format(time.RFC3339))
	query.Set("end", end.Format(time.RFC3339))
	target.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		http.Error(w, "录像查询地址无效", http.StatusInternalServerError)
		return
	}
	response, err := s.playbackClient.Do(request)
	if err != nil {
		http.Error(w, "录像服务暂时不可用", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		writeJSON(w, http.StatusOK, map[string]any{"recordings": []recordingClip{}})
		return
	}
	if response.StatusCode/100 != 2 {
		http.Error(w, "录像服务返回错误", http.StatusBadGateway)
		return
	}

	var spans []recordingSpan
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&spans); err != nil {
		http.Error(w, "录像列表格式错误", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"recordings": splitRecordingSpans(spans, start, end)})
}

func (s *Server) handleRecordingPlayback(w http.ResponseWriter, r *http.Request, _ string) {
	start, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("start"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_start"})
		return
	}
	duration, err := strconv.ParseFloat(r.URL.Query().Get("duration"), 64)
	if err != nil || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 || duration > recordingClipDuration.Seconds()+1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_duration"})
		return
	}

	target := playbackEndpoint(s.playbackURL, "/get")
	query := target.Query()
	query.Set("path", s.config.MainStream)
	query.Set("start", start.Format(time.RFC3339Nano))
	query.Set("duration", strconv.FormatFloat(duration, 'f', 3, 64))
	query.Set("format", "mp4")
	target.RawQuery = query.Encode()

	// MediaMTX generates MP4 dynamically and returns Accept-Ranges: none.
	// Keep one stable representation per clip, then let ServeContent implement
	// byte ranges (including suffix requests and HEAD) for native video seeking.
	file, release, err := s.playbackCache.acquire(r.Context(), target.String(), func() (string, error) {
		return s.prepareRecordingPlayback(r.Context(), target.String())
	})
	if err != nil {
		status := http.StatusBadGateway
		var playbackErr *recordingPlaybackError
		if errors.As(err, &playbackErr) {
			status = playbackErr.status
		}
		if status == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "2")
		}
		http.Error(w, "录像暂时无法播放，请稍后重试", status)
		return
	}
	defer release()
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "private, no-store")
	// A regenerated clip must not satisfy If-Range for an older representation.
	identity := sha256.Sum256([]byte(file.Name()))
	w.Header().Set("ETag", fmt.Sprintf("\"%x\"", identity))
	http.ServeContent(recordingPlaybackWriter{w}, r, "recording.mp4", time.Time{}, file)
}

type recordingPlaybackWriter struct {
	http.ResponseWriter
}

func (w recordingPlaybackWriter) WriteHeader(status int) {
	// ServeContent clears cache headers on errors such as an invalid range.
	w.Header().Set("Cache-Control", "private, no-store")
	w.ResponseWriter.WriteHeader(status)
}

func (s *Server) prepareRecordingPlayback(parent context.Context, target string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	response, err := s.playbackClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		status := http.StatusBadGateway
		if response.StatusCode == http.StatusNotFound {
			status = http.StatusNotFound
		}
		return "", &recordingPlaybackError{status: status}
	}
	if response.ContentLength > recordingPlaybackMaxBytes {
		return "", fmt.Errorf("recording exceeds playback size limit")
	}
	file, err := os.CreateTemp(s.playbackCache.tempDir(), "lens-playback-*.mp4")
	if err != nil {
		return "", err
	}
	keep := false
	defer func() {
		file.Close()
		if !keep {
			os.Remove(file.Name())
		}
	}()
	size, err := io.Copy(file, io.LimitReader(response.Body, recordingPlaybackMaxBytes+1))
	if err != nil {
		return "", err
	}
	if size == 0 || size > recordingPlaybackMaxBytes || (response.ContentLength >= 0 && size != response.ContentLength) {
		return "", fmt.Errorf("incomplete or oversized recording")
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	keep = true
	return file.Name(), nil
}

func recordingWindow(query url.Values) (time.Time, time.Time, error) {
	start, err := time.Parse(time.RFC3339Nano, query.Get("start"))
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid_start")
	}
	end, err := time.Parse(time.RFC3339Nano, query.Get("end"))
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid_end")
	}
	if !end.After(start) || end.Sub(start) > 26*time.Hour {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid_window")
	}
	return start, end, nil
}

func splitRecordingSpans(spans []recordingSpan, windowStart, windowEnd time.Time) []recordingClip {
	clips := make([]recordingClip, 0)
	for _, span := range spans {
		start, err := time.Parse(time.RFC3339Nano, span.Start)
		if err != nil || span.Duration <= 0 {
			continue
		}
		end := start.Add(time.Duration(span.Duration * float64(time.Second)))
		if start.Before(windowStart) {
			start = windowStart
		}
		if end.After(windowEnd) {
			end = windowEnd
		}
		for cursor := start; cursor.Before(end); cursor = cursor.Add(recordingClipDuration) {
			clipEnd := cursor.Add(recordingClipDuration)
			if clipEnd.After(end) {
				clipEnd = end
			}
			clips = append(clips, recordingClip{
				Start: cursor.Format(time.RFC3339Nano), Duration: clipEnd.Sub(cursor).Seconds(),
			})
		}
	}
	sort.Slice(clips, func(i, j int) bool {
		left, _ := time.Parse(time.RFC3339Nano, clips[i].Start)
		right, _ := time.Parse(time.RFC3339Nano, clips[j].Start)
		return left.After(right)
	})
	return clips
}

func playbackEndpoint(base *url.URL, path string) url.URL {
	target := *base
	target.Path = strings.TrimRight(target.Path, "/") + path
	target.RawQuery = ""
	return target
}
