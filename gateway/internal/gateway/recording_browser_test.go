package gateway

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Run only in Actions with the official MediaMTX binary, FFmpeg and Playwright.
// Google Chrome supplies the camera's H.264 codec. This exercises actual
// muxing, HLS demuxing, decoding, seeking and CSP together.
func TestRecordingBrowser(t *testing.T) {
	mediamtx := os.Getenv("PLAYBACK_TEST_MEDIAMTX")
	if mediamtx == "" {
		t.Skip("real-media browser integration is enabled by GitHub Actions")
	}
	dir := t.TempDir()
	recordings := filepath.Join(dir, "recordings")
	day := filepath.Join(recordings, "ipc-main", "2026-09-05")
	if err := os.MkdirAll(day, 0700); err != nil { t.Fatal(err) }
	fixture := filepath.Join(day, "00-00-00-000000.mp4")
	// The changing color verifies that a seek displays the requested footage,
	// rather than merely moving currentTime over the wrong segment. A 1.7s GOP
	// deliberately does not align with the six-second playback boundaries.
	filter := "drawbox=x=0:y=0:w=160:h=180:color=red:t=fill:enable='lt(t,12)',drawbox=x=0:y=0:w=160:h=180:color=green:t=fill:enable='between(t,12,24)',drawbox=x=0:y=0:w=160:h=180:color=blue:t=fill:enable='gte(t,24)'"
	ffmpeg := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10", "-t", "48", "-vf", filter, "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "17", "-bf", "0", "-movflags", "empty_moov+default_base_moof+frag_keyframe", "-f", "mp4", fixture)
	if output, err := ffmpeg.CombinedOutput(); err != nil { t.Fatalf("fixture: %v\n%s", err, output) }
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil { t.Fatal(err) }
	address := listener.Addr().String()
	listener.Close()
	config := fmt.Sprintf("logLevel: warn\nrtsp: false\nrtmp: false\nhls: false\nwebrtc: false\nsrt: false\nmoq: false\nplayback: true\nplaybackAddress: %s\npathDefaults:\n  recordPath: %s/%%path/%%Y-%%m-%%d/%%H-%%M-%%S-%%f\n  recordFormat: fmp4\n  recordDeleteAfter: 0s\npaths:\n  ipc-main:\n", address, filepath.ToSlash(recordings))
	configPath := filepath.Join(dir, "mediamtx.yml")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil { t.Fatal(err) }
	logPath := filepath.Join(dir, "mediamtx.log")
	logFile, err := os.Create(logPath)
	if err != nil { t.Fatal(err) }
	cmd := exec.Command(mediamtx, configPath)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil { t.Fatal(err) }
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		logFile.Close()
		if t.Failed() { data, _ := os.ReadFile(logPath); t.Logf("MediaMTX:\n%s", data) }
	})
	playbackURL, _ := url.Parse("http://"+address)
	deadline := time.Now().Add(10*time.Second)
	for {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil { conn.Close(); break }
		if time.Now().After(deadline) { t.Fatal("MediaMTX did not start") }
		time.Sleep(50*time.Millisecond)
	}
	s := &Server{config: Config{MainStream: "ipc-main"}, playbackURL: playbackURL, playbackClient: &http.Client{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/recordings/play", func(w http.ResponseWriter, r *http.Request) { s.handleRecordingPlayback(w, r, "test") })
	mux.HandleFunc("GET /api/recordings/segment", func(w http.ResponseWriter, r *http.Request) { s.handleRecordingSegment(w, r, "test") })
	mux.HandleFunc("GET /fixture", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"></head><body><video id="video" controls muted playsinline></video><script src="/hls-1.7.2.min.js"></script><script src="/recording-player.js"></script></body></html>`)
	})
	webRoot, _ := fs.Sub(webFiles, "web")
	mux.Handle("/", http.FileServer(http.FS(webRoot)))
	gateway := httptest.NewServer(securityHeaders(mux))
	defer gateway.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	browser := exec.CommandContext(ctx, "node", "../../tests/recording-browser.mjs", gateway.URL)
	if output, err := browser.CombinedOutput(); err != nil { t.Fatalf("browser integration: %v\n%s", err, output) } else { t.Log(string(output)) }
}
