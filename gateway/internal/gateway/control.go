package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const leaseTimeout = 700 * time.Millisecond

type controlMessage struct {
	Motor     int    `json:"motor"`
	Action    string `json:"action"`
	Direction string `json:"direction,omitempty"`
	Speed     int    `json:"speed,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Steps     int    `json:"steps,omitempty"`
	Hold      bool   `json:"hold,omitempty"`
	CommandID string `json:"commandId,omitempty"`
}

type socketReply struct {
	Type      string         `json:"type"`
	OK        bool           `json:"ok"`
	Code      string         `json:"code,omitempty"`
	Message   string         `json:"message,omitempty"`
	Motor     int            `json:"motor,omitempty"`
	CommandID string         `json:"commandId,omitempty"`
	ClientID  string         `json:"clientId,omitempty"`
	Leases    map[int]string `json:"leases,omitempty"`
}

type motorLease struct {
	ownerID string
	owner   string
	until   time.Time
	command MotorCommand
	moving  bool
	cancel  context.CancelFunc
}

type wsClient struct {
	id      string
	user    string
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func (c *wsClient) write(value any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	return c.conn.WriteJSON(value)
}

func (c *wsClient) ping() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(2*time.Second))
}

type controlHub struct {
	mu      sync.Mutex
	esp     *espClient
	motors  []int
	allowed map[int]struct{}
	leases  map[int]motorLease
	clients map[string]*wsClient
	stop    chan struct{}
}

func newControlHub(esp *espClient, motors []int) *controlHub {
	allowed := make(map[int]struct{}, len(motors))
	for _, motor := range motors {
		allowed[motor] = struct{}{}
	}
	hub := &controlHub{
		esp: esp, motors: append([]int(nil), motors...), allowed: allowed,
		leases: make(map[int]motorLease), clients: make(map[string]*wsClient),
		stop: make(chan struct{}),
	}
	go hub.watchLeases()
	return hub
}

func (h *controlHub) close() {
	select {
	case <-h.stop:
		return
	default:
		close(h.stop)
	}
	h.releaseAll()
}

func (h *controlHub) serveWS(w http.ResponseWriter, r *http.Request, user string) {
	upgrader := websocket.Upgrader{
		HandshakeTimeout: 3 * time.Second,
		CheckOrigin:      func(r *http.Request) bool { return sameOrigin(r) },
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	client := &wsClient{id: randomID(), user: user, conn: conn}
	conn.SetReadLimit(4096)
	_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	})
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if client.ping() != nil {
					_ = conn.Close()
					return
				}
			case <-done:
				return
			}
		}
	}()
	h.mu.Lock()
	h.clients[client.id] = client
	h.mu.Unlock()
	_ = client.write(socketReply{Type: "ready", OK: true, ClientID: client.id, Leases: h.leaseSnapshot()})

	defer func() {
		close(done)
		h.removeClient(client)
		_ = conn.Close()
	}()
	for {
		var message controlMessage
		if err := conn.ReadJSON(&message); err != nil {
			return
		}
		h.handle(client, message)
	}
}

func (h *controlHub) handle(client *wsClient, message controlMessage) {
	if err := validateControlMessage(message, h.allowed); err != nil {
		_ = client.write(socketReply{Type: "error", OK: false, Code: "invalid_command", Message: err.Error(), Motor: message.Motor, CommandID: message.CommandID})
		return
	}

	command := MotorCommand{
		Motor: message.Motor, Action: message.Action, Direction: message.Direction,
		Speed: message.Speed, Mode: message.Mode, Steps: message.Steps,
		Hold: message.Hold, CommandID: message.CommandID,
	}
	switch message.Action {
	case "jog":
		if !h.acquire(client, command, leaseTimeout, false) {
			h.busy(client, message)
			return
		}
		if err := h.send(command); err != nil {
			h.dropLease(message.Motor, client.id)
			h.deviceError(client, message, err)
			return
		}
	case "heartbeat":
		stored, ok := h.renew(message.Motor, client.id)
		if !ok {
			h.busy(client, message)
			return
		}
		if stored.Action == "jog" {
			stored.CommandID = message.CommandID
			if err := h.send(stored); err != nil {
				h.dropLease(message.Motor, client.id)
				h.deviceError(client, message, err)
				return
			}
		}
	case "move":
		duration := time.Duration(float64(message.Steps)/float64(message.Speed)*float64(time.Second)) + time.Second
		if duration < 2*time.Second {
			duration = 2 * time.Second
		}
		if duration > 30*time.Second {
			duration = 30 * time.Second
		}
		if !h.acquire(client, command, duration, true) {
			h.busy(client, message)
			return
		}
		if err := h.send(command); err != nil {
			h.dropLease(message.Motor, client.id)
			h.deviceError(client, message, err)
			return
		}
	case "stop", "release":
		// Cancel an automated owner before sending the physical stop so it cannot
		// enqueue another movement after the operator's emergency command.
		h.forceDropLease(message.Motor)
		if err := h.send(command); err != nil {
			h.deviceError(client, message, err)
			return
		}
	}
	_ = client.write(socketReply{Type: "ack", OK: true, Motor: message.Motor, CommandID: message.CommandID})
	h.broadcastLeases()
}

func validateControlMessage(message controlMessage, allowed map[int]struct{}) error {
	if _, ok := allowed[message.Motor]; !ok {
		return fmt.Errorf("motor is not configured")
	}
	switch message.Action {
	case "jog", "move":
		if message.Direction != "cw" && message.Direction != "ccw" {
			return fmt.Errorf("direction must be cw or ccw")
		}
		if message.Speed < 10 || message.Speed > 1000 {
			return fmt.Errorf("speed must be between 10 and 1000")
		}
		if message.Mode != "full" && message.Mode != "half" {
			return fmt.Errorf("mode must be full or half")
		}
		if message.Action == "move" && (message.Steps < 1 || message.Steps > 10000) {
			return fmt.Errorf("steps must be between 1 and 10000")
		}
	case "heartbeat", "stop", "release":
	default:
		return fmt.Errorf("unsupported action")
	}
	if len(message.CommandID) > 64 {
		return fmt.Errorf("commandId is too long")
	}
	return nil
}

func (h *controlHub) acquire(client *wsClient, command MotorCommand, duration time.Duration, moving bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if existing, ok := h.leases[command.Motor]; ok && existing.ownerID != client.id && time.Now().Before(existing.until) {
		return false
	}
	if existing, ok := h.leases[command.Motor]; ok && existing.cancel != nil {
		existing.cancel()
	}
	h.leases[command.Motor] = motorLease{
		ownerID: client.id, owner: client.user + "@" + client.id,
		until: time.Now().Add(duration), command: command, moving: moving,
	}
	return true
}

func (h *controlHub) acquireSystem(motor int, ownerID, owner string, duration time.Duration, cancel context.CancelFunc) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.allowed[motor]; !ok {
		return false
	}
	if existing, ok := h.leases[motor]; ok && time.Now().Before(existing.until) {
		return false
	}
	if existing, ok := h.leases[motor]; ok && existing.cancel != nil {
		existing.cancel()
	}
	h.leases[motor] = motorLease{
		ownerID: ownerID, owner: owner, until: time.Now().Add(duration), moving: true,
		cancel: cancel,
	}
	return true
}

func (h *controlHub) systemOwns(motor int, ownerID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	lease, ok := h.leases[motor]
	return ok && lease.ownerID == ownerID && time.Now().Before(lease.until)
}

func (h *controlHub) releaseSystem(motor int, ownerID string) {
	h.mu.Lock()
	if lease, ok := h.leases[motor]; ok && lease.ownerID == ownerID {
		delete(h.leases, motor)
	}
	h.mu.Unlock()
	h.broadcastLeases()
}

func (h *controlHub) renew(motor int, ownerID string) (MotorCommand, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	lease, ok := h.leases[motor]
	if !ok || lease.ownerID != ownerID || time.Now().After(lease.until) {
		return MotorCommand{}, false
	}
	lease.until = time.Now().Add(leaseTimeout)
	h.leases[motor] = lease
	return lease.command, true
}

func (h *controlHub) dropLease(motor int, ownerID string) {
	h.mu.Lock()
	if lease, ok := h.leases[motor]; ok && lease.ownerID == ownerID {
		if lease.cancel != nil {
			lease.cancel()
		}
		delete(h.leases, motor)
	}
	h.mu.Unlock()
	h.broadcastLeases()
}

func (h *controlHub) forceDropLease(motor int) {
	h.mu.Lock()
	if lease, ok := h.leases[motor]; ok && lease.cancel != nil {
		lease.cancel()
	}
	delete(h.leases, motor)
	h.mu.Unlock()
}

func (h *controlHub) removeClient(client *wsClient) {
	var motors []int
	h.mu.Lock()
	delete(h.clients, client.id)
	for motor, lease := range h.leases {
		if lease.ownerID == client.id {
			motors = append(motors, motor)
			if lease.cancel != nil {
				lease.cancel()
			}
			delete(h.leases, motor)
		}
	}
	h.mu.Unlock()
	for _, motor := range motors {
		_ = h.send(MotorCommand{Motor: motor, Action: "stop", CommandID: "disconnect"})
	}
	h.broadcastLeases()
}

func (h *controlHub) watchLeases() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			var expired []int
			h.mu.Lock()
			for motor, lease := range h.leases {
				if time.Now().After(lease.until) {
					expired = append(expired, motor)
					if lease.cancel != nil {
						lease.cancel()
					}
					delete(h.leases, motor)
				}
			}
			h.mu.Unlock()
			for _, motor := range expired {
				_ = h.send(MotorCommand{Motor: motor, Action: "stop", CommandID: "lease-timeout"})
			}
			if len(expired) > 0 {
				h.broadcastLeases()
			}
		case <-h.stop:
			return
		}
	}
}

func (h *controlHub) releaseAll() {
	h.mu.Lock()
	for _, lease := range h.leases {
		if lease.cancel != nil {
			lease.cancel()
		}
	}
	h.leases = make(map[int]motorLease)
	h.mu.Unlock()
	for _, motor := range h.motors {
		_ = h.send(MotorCommand{Motor: motor, Action: "release", CommandID: "gateway-shutdown"})
	}
}

func (h *controlHub) send(command MotorCommand) error {
	ctx, cancel := context.WithTimeout(context.Background(), 650*time.Millisecond)
	defer cancel()
	return h.esp.command(ctx, command)
}

func (h *controlHub) busy(client *wsClient, message controlMessage) {
	_ = client.write(socketReply{Type: "error", OK: false, Code: "motor_busy", Message: "该电机正在被其他用户操作", Motor: message.Motor, CommandID: message.CommandID, Leases: h.leaseSnapshot()})
}

func (h *controlHub) deviceError(client *wsClient, message controlMessage, err error) {
	_ = client.write(socketReply{Type: "error", OK: false, Code: "esp_unavailable", Message: err.Error(), Motor: message.Motor, CommandID: message.CommandID})
}

func (h *controlHub) leaseSnapshot() map[int]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := make(map[int]string, len(h.leases))
	for motor, lease := range h.leases {
		if time.Now().Before(lease.until) {
			result[motor] = lease.owner
		}
	}
	return result
}

func (h *controlHub) broadcastLeases() {
	reply := socketReply{Type: "leases", OK: true, Leases: h.leaseSnapshot()}
	h.mu.Lock()
	clients := make([]*wsClient, 0, len(h.clients))
	for _, client := range h.clients {
		clients = append(clients, client)
	}
	h.mu.Unlock()
	for _, client := range clients {
		_ = client.write(reply)
	}
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := r.Host
	if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" {
		host = strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	return strings.EqualFold(parsed.Host, host)
}

func randomID() string {
	value := make([]byte, 4)
	if _, err := rand.Read(value); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano())
	}
	return hex.EncodeToString(value)
}
