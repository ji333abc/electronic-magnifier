package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type MotorCommand struct {
	Motor     int    `json:"motor"`
	Action    string `json:"action"`
	Direction string `json:"direction,omitempty"`
	Speed     int    `json:"speed,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Steps     int    `json:"steps,omitempty"`
	Hold      bool   `json:"hold,omitempty"`
	CommandID string `json:"commandId,omitempty"`
}

type ESPMotorStatus struct {
	Motor          int    `json:"motor"`
	Running        bool   `json:"running"`
	Continuous     bool   `json:"continuous"`
	CoilsHeld      bool   `json:"coilsHeld"`
	Direction      string `json:"direction"`
	RemainingSteps int    `json:"remainingSteps"`
	Speed          int    `json:"speed"`
	Mode           string `json:"mode"`
	LastCommandID  string `json:"lastCommandId"`
	Position       *int64 `json:"position"`
	Homed          bool   `json:"homed"`
	MinLimit       bool   `json:"minLimit"`
	MaxLimit       bool   `json:"maxLimit"`
}

type ESPStatus struct {
	OK         bool             `json:"ok"`
	APIVersion int              `json:"apiVersion"`
	Motors     []ESPMotorStatus `json:"motors"`
}

type espClient struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func newESPClient(baseURL, apiKey string) *espClient {
	return &espClient{
		baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey,
		client: &http.Client{Timeout: 650 * time.Millisecond},
	}
}

func (e *espClient) command(ctx context.Context, command MotorCommand) error {
	payload, err := json.Marshal(command)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/api/v1/motors/%d/%s", e.baseURL, command.Motor, command.Action)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", e.apiKey)
	response, err := e.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("ESP command returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func (e *espClient) status(ctx context.Context) (ESPStatus, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, e.baseURL+"/api/v1/status", nil)
	if err != nil {
		return ESPStatus{}, err
	}
	request.Header.Set("X-API-Key", e.apiKey)
	response, err := e.client.Do(request)
	if err != nil {
		return ESPStatus{}, err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return ESPStatus{}, fmt.Errorf("ESP status returned %s", response.Status)
	}
	var status ESPStatus
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&status); err != nil {
		return ESPStatus{}, err
	}
	return status, nil
}
