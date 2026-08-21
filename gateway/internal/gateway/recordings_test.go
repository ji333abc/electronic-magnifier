package gateway

import (
	"net/url"
	"testing"
	"time"
)

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
