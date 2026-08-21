package gateway

import (
	"net/http/httptest"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{
		Listen: "127.0.0.1:0", ESPBaseURL: "http://127.0.0.1:1",
		IPCBaseURL: "http://127.0.0.1:2", IPCSnapshotPath: "/webcapture.jpg?command=snap&channel=0",
		Go2RTCURL: "http://127.0.0.1:3",
		PlaybackURL: "http://127.0.0.1:4",
		MainStream:  "main", SubStream: "sub", SessionHours: 12,
		Motors: []MotorConfig{
			{ID: 1, Role: "focus", Name: "A", DefaultSpeed: 100, DefaultMode: "half"},
			{ID: 2, Role: "zoom", Name: "B", DefaultSpeed: 200, DefaultMode: "full"},
		},
	}
}

func testSecrets(t *testing.T) Secrets {
	t.Helper()
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	return Secrets{AdminUser: "admin", AdminPasswordHash: hash, SessionSecret: "01234567890123456789012345678901", ESPAPIKey: "esp-secret"}
}

func TestValidateConfig(t *testing.T) {
	config := testConfig()
	if err := validateConfig(config, testSecrets(t)); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	config.Motors[1].ID = 1
	if err := validateConfig(config, testSecrets(t)); err == nil {
		t.Fatal("duplicate motor ID was accepted")
	}
}

func TestSessionRoundTrip(t *testing.T) {
	store := newSessionStore("01234567890123456789012345678901", time.Hour)
	request := httptest.NewRequest("GET", "http://device.local/", nil)
	recorder := httptest.NewRecorder()
	if err := store.create(recorder, request, "operator"); err != nil {
		t.Fatal(err)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly {
		t.Fatal("secure session cookie was not created")
	}
	followup := httptest.NewRequest("GET", "http://device.local/api/status", nil)
	followup.AddCookie(cookies[0])
	if user, ok := store.user(followup); !ok || user != "operator" {
		t.Fatalf("session lookup failed: user=%q ok=%v", user, ok)
	}
}

func TestEndpointHost(t *testing.T) {
	if got := endpointHost("http://192.168.1.122", "80"); got != "192.168.1.122:80" {
		t.Fatalf("unexpected host: %s", got)
	}
}
