package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	if err != nil || duration <= 0 || duration > recordingClipDuration.Seconds()+1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_duration"})
		return
	}

	target := playbackEndpoint(s.playbackURL, "/get")
	query := target.Query()
	query.Set("path", s.config.MainStream)
	query.Set("start", start.Format(time.RFC3339Nano))
	query.Set("duration", strconv.FormatFloat(duration, 'f', 3, 64))
	target.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		http.Error(w, "录像播放地址无效", http.StatusInternalServerError)
		return
	}
	if value := r.Header.Get("Range"); value != "" {
		request.Header.Set("Range", value)
	}
	response, err := s.playbackClient.Do(request)
	if err != nil {
		http.Error(w, "录像暂时无法播放", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if value := response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
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
	sort.Slice(clips, func(i, j int) bool { return clips[i].Start > clips[j].Start })
	return clips
}

func playbackEndpoint(base *url.URL, path string) url.URL {
	target := *base
	target.Path = strings.TrimRight(target.Path, "/") + path
	target.RawQuery = ""
	return target
}
