package gateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const recordingSegmentSeconds = 6.0

func playbackWindow(query url.Values, maximum float64) (time.Time, float64, error) {
	start, err := time.Parse(time.RFC3339Nano, query.Get("start"))
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("invalid_start")
	}
	duration, err := strconv.ParseFloat(query.Get("duration"), 64)
	if err != nil || math.IsNaN(duration) || math.IsInf(duration, 0) || duration < 0.000001 || duration > maximum {
		return time.Time{}, 0, fmt.Errorf("invalid_duration")
	}
	return start, duration, nil
}

func (s *Server) handleRecordingPlayback(w http.ResponseWriter, r *http.Request, _ string) {
	start, duration, err := playbackWindow(r.URL.Query(), recordingClipDuration.Seconds()+1)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var playlist strings.Builder
	playlist.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n")
	for offset := 0.0; offset < duration; offset += recordingSegmentSeconds {
		length := math.Min(recordingSegmentSeconds, duration-offset)
		query := url.Values{
			"start": {start.Add(time.Duration(offset * float64(time.Second))).Format(time.RFC3339Nano)},
			"duration": {strconv.FormatFloat(length, 'f', 6, 64)},
		}
		// Each MediaMTX request has its own zero-based media timeline. Mark the
		// boundary explicitly so both native HLS and hls.js remap timestamps.
		if offset > 0 {
			playlist.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		fmt.Fprintf(&playlist, "#EXT-X-MAP:URI=\"/api/recordings/segment?%s&kind=init\"\n", query.Encode())
		fmt.Fprintf(&playlist, "#EXTINF:%.6f,\n/api/recordings/segment?%s&kind=media\n", length, query.Encode())
	}
	playlist.WriteString("#EXT-X-ENDLIST\n")
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "private, no-store")
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, playlist.String())
	}
}

func (s *Server) handleRecordingSegment(w http.ResponseWriter, r *http.Request, _ string) {
	start, duration, err := playbackWindow(r.URL.Query(), recordingSegmentSeconds)
	kind := r.URL.Query().Get("kind")
	if err != nil || (kind != "init" && kind != "media") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_segment"})
		return
	}
	if s.playbackActive.Add(1) > 4 {
		s.playbackActive.Add(-1)
		w.Header().Set("Retry-After", "2")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "playback_busy"})
		return
	}
	defer s.playbackActive.Add(-1)
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	target := playbackEndpoint(s.playbackURL, "/get")
	query := target.Query()
	query.Set("path", s.config.MainStream)
	query.Set("start", start.Format(time.RFC3339Nano))
	query.Set("duration", strconv.FormatFloat(duration, 'f', 6, 64))
	query.Set("format", "fmp4")
	target.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "playback_unavailable"})
		return
	}
	response, err := s.playbackClient.Do(request)
	if err != nil {
		log.Printf("recording segment %s: %v", start.Format(time.RFC3339Nano), err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "playback_unavailable"})
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		status, message := http.StatusBadGateway, "playback_unavailable"
		if response.StatusCode == http.StatusNotFound {
			status, message = http.StatusNotFound, "recording_missing"
		}
		log.Printf("recording segment %s: upstream HTTP %d", start.Format(time.RFC3339Nano), response.StatusCode)
		writeJSON(w, status, map[string]string{"error": message})
		return
	}
	init, err := readRecordingInit(response.Body)
	if err != nil {
		log.Printf("recording segment %s: %v", start.Format(time.RFC3339Nano), err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "invalid_recording"})
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "private, no-store")
	if r.Method == http.MethodHead {
		return
	}
	if kind == "init" {
		_, _ = w.Write(init)
		return
	}
	// Stream moof/mdat as they arrive. No complete-clip buffering or disk
	// cache; bounded reads and request cancellation also bound upstream work.
	writer := recordingStreamWriter{w}
	_, err = io.CopyBuffer(writer, io.LimitReader(response.Body, 64<<20), make([]byte, 32<<10))
	if err == nil {
		var extra [1]byte
		if n, readErr := response.Body.Read(extra[:]); n != 0 || (readErr != nil && readErr != io.EOF) {
			err = fmt.Errorf("oversized or incomplete recording segment")
		}
	}
	if err != nil {
		log.Printf("recording segment %s interrupted: %v", start.Format(time.RFC3339Nano), err)
		// Do not finish a truncated chunked response successfully: the player
		// must see a network error and retry this small segment.
		panic(http.ErrAbortHandler)
	}
}

type recordingStreamWriter struct {
	http.ResponseWriter
}

func (w recordingStreamWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

// An HLS initialization section contains ftyp/moov, while media segments
// contain moof/mdat. Read only the small initialization boxes, never the clip.
func readRecordingInit(reader io.Reader) ([]byte, error) {
	var init bytes.Buffer
	seenFTYP := false
	for init.Len() < 1<<20 {
		var header [16]byte
		if _, err := io.ReadFull(reader, header[:8]); err != nil {
			return nil, err
		}
		size := uint64(binary.BigEndian.Uint32(header[:4]))
		headerSize := uint64(8)
		if size == 1 {
			if _, err := io.ReadFull(reader, header[8:]); err != nil {
				return nil, err
			}
			size, headerSize = binary.BigEndian.Uint64(header[8:]), 16
		}
		kind := string(header[4:8])
		if size < headerSize || size > uint64((1<<20)-init.Len()) || (kind != "ftyp" && kind != "moov" && kind != "free") {
			return nil, fmt.Errorf("invalid MP4 initialization box")
		}
		init.Write(header[:headerSize])
		if _, err := io.CopyN(&init, reader, int64(size-headerSize)); err != nil {
			return nil, err
		}
		if kind == "ftyp" {
			seenFTYP = true
		}
		if kind == "moov" && seenFTYP {
			return init.Bytes(), nil
		}
	}
	return nil, fmt.Errorf("MP4 initialization exceeds size limit")
}
