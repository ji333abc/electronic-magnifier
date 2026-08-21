package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestValidateControlMessage(t *testing.T) {
	allowed := map[int]struct{}{1: {}, 2: {}}
	valid := controlMessage{Motor: 1, Action: "move", Direction: "cw", Speed: 200, Mode: "half", Steps: 10}
	if err := validateControlMessage(valid, allowed); err != nil {
		t.Fatalf("valid command rejected: %v", err)
	}
	valid.Steps = 10001
	if err := validateControlMessage(valid, allowed); err == nil {
		t.Fatal("unsafe step count accepted")
	}
	if err := validateControlMessage(controlMessage{Motor: 2, Action: "stop"}, allowed); err != nil {
		t.Fatalf("emergency stop rejected: %v", err)
	}
	if err := validateControlMessage(controlMessage{Motor: 3, Action: "stop"}, allowed); err == nil {
		t.Fatal("unconfigured motor was accepted")
	}
}

func TestSystemLeaseCancellation(t *testing.T) {
	hub := &controlHub{motors: []int{1}, allowed: map[int]struct{}{1: {}}, leases: make(map[int]motorLease), clients: make(map[string]*wsClient)}
	ctx, cancel := context.WithCancel(context.Background())
	if !hub.acquireSystem(1, "autofocus", "自动精调@autofocus", time.Second, cancel) {
		t.Fatal("system lease was not acquired")
	}
	if !hub.systemOwns(1, "autofocus") {
		t.Fatal("system lease ownership was not recorded")
	}
	hub.forceDropLease(1)
	select {
	case <-ctx.Done():
	default:
		t.Fatal("dropping the system lease did not cancel its work")
	}
}

func TestPerMotorLease(t *testing.T) {
	hub := &controlHub{motors: []int{1, 2}, allowed: map[int]struct{}{1: {}, 2: {}}, leases: make(map[int]motorLease), clients: make(map[string]*wsClient)}
	first := &wsClient{id: "one", user: "admin"}
	second := &wsClient{id: "two", user: "admin"}
	command := MotorCommand{Motor: 1, Action: "jog", Direction: "cw", Speed: 100, Mode: "half"}
	if !hub.acquire(first, command, time.Second, false) {
		t.Fatal("first client could not acquire motor")
	}
	if hub.acquire(second, command, time.Second, false) {
		t.Fatal("second client acquired an occupied motor")
	}
	command.Motor = 2
	if !hub.acquire(second, command, time.Second, false) {
		t.Fatal("different motor should allow concurrent control")
	}
}

func TestLeaseTimeoutStopsMotor(t *testing.T) {
	var mu sync.Mutex
	var actions []string
	espServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		actions = append(actions, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer espServer.Close()
	hub := newControlHub(newESPClient(espServer.URL, "key"), []int{1, 2})
	defer hub.close()
	client := &wsClient{id: "one", user: "admin"}
	hub.acquire(client, MotorCommand{Motor: 1, Action: "jog"}, 80*time.Millisecond, false)
	time.Sleep(250 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(actions) == 0 || actions[0] != "/api/v1/motors/1/stop" {
		t.Fatalf("expired lease did not stop motor: %#v", actions)
	}
}
