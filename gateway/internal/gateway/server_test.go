package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoginAndProtectedConfig(t *testing.T) {
	espServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/status" {
			_ = json.NewEncoder(w).Encode(ESPStatus{OK: true, APIVersion: 1})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer espServer.Close()
	videoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"main": map[string]any{"producers": []any{}}})
	}))
	defer videoServer.Close()

	config := testConfig()
	config.ESPBaseURL = espServer.URL
	config.IPCBaseURL = espServer.URL
	config.Go2RTCURL = videoServer.URL
	server, err := NewServer(config, testSecrets(t))
	if err != nil {
		t.Fatal(err)
	}
	defer server.hub.close()
	handler := server.routes()

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "http://device.local/api/config", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("protected route returned %d", unauthorized.Code)
	}

	body := bytes.NewBufferString(`{"username":"admin","password":"correct horse battery staple"}`)
	loginRequest := httptest.NewRequest(http.MethodPost, "http://device.local/api/login", body)
	loginRequest.Host = "device.local"
	loginRequest.Header.Set("Origin", "http://device.local")
	loginRequest.Header.Set("Content-Type", "application/json")
	login := httptest.NewRecorder()
	handler.ServeHTTP(login, loginRequest)
	if login.Code != http.StatusOK {
		t.Fatalf("login returned %d: %s", login.Code, login.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "http://device.local/api/config", nil)
	request.AddCookie(login.Result().Cookies()[0])
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte("motors")) {
		t.Fatalf("authenticated config failed: %d %s", response.Code, response.Body.String())
	}
	if !bytes.Contains(response.Body.Bytes(), []byte(`"autoFocus":true`)) {
		t.Fatalf("autofocus capability was not exposed: %s", response.Body.String())
	}
}
