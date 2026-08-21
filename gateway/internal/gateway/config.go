package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

type MotorConfig struct {
	ID            int    `json:"id"`
	Role          string `json:"role,omitempty"`
	Name          string `json:"name"`
	Negative      string `json:"negative"`
	Positive      string `json:"positive"`
	MinLimitLabel string `json:"minLimitLabel,omitempty"`
	MaxLimitLabel string `json:"maxLimitLabel,omitempty"`
	DefaultSpeed  int    `json:"defaultSpeed"`
	DefaultMode   string `json:"defaultMode"`
}

type Config struct {
	Listen          string        `json:"listen"`
	ESPBaseURL      string        `json:"espBaseUrl"`
	IPCBaseURL      string        `json:"ipcBaseUrl"`
	IPCSnapshotPath string        `json:"ipcSnapshotPath"`
	Go2RTCURL       string        `json:"go2rtcUrl"`
	PlaybackURL     string        `json:"playbackUrl"`
	MainStream      string        `json:"mainStream"`
	SubStream       string        `json:"subStream"`
	SessionHours    int           `json:"sessionHours"`
	Motors          []MotorConfig `json:"motors"`
}

type Secrets struct {
	AdminUser         string
	AdminPasswordHash string
	SessionSecret     string
	ESPAPIKey         string
	IPCUser           string
	IPCPassword       string
}

func LoadConfig(path string) (Config, Secrets, error) {
	config := Config{
		Listen:          "0.0.0.0:80",
		ESPBaseURL:      "http://192.168.1.123",
		IPCBaseURL:      "http://192.168.1.122",
		IPCSnapshotPath: "/webcapture.jpg?command=snap&channel=0",
		Go2RTCURL:       "http://127.0.0.1:1984",
		PlaybackURL:     "http://127.0.0.1:9996",
		MainStream:      "ipc-main",
		SubStream:       "ipc-sub",
		SessionHours:    12,
		Motors: []MotorConfig{
			{ID: 1, Role: "focus", Name: "对焦", Negative: "近焦", Positive: "远焦", MinLimitLabel: "近端限位", MaxLimitLabel: "远端限位", DefaultSpeed: 120, DefaultMode: "half"},
			{ID: 2, Role: "zoom", Name: "变焦", Negative: "广角", Positive: "长焦", MinLimitLabel: "广角限位", MaxLimitLabel: "长焦限位", DefaultSpeed: 200, DefaultMode: "full"},
		},
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, Secrets{}, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(data, &config); err != nil {
			return Config{}, Secrets{}, fmt.Errorf("parse config: %w", err)
		}
	}

	secrets := Secrets{
		AdminUser:         envOr("GATEWAY_ADMIN_USER", "admin"),
		AdminPasswordHash: os.Getenv("GATEWAY_ADMIN_PASSWORD_HASH"),
		SessionSecret:     os.Getenv("GATEWAY_SESSION_SECRET"),
		ESPAPIKey:         os.Getenv("ESP_API_KEY"),
		IPCUser:           os.Getenv("IPC_USER"),
		IPCPassword:       os.Getenv("IPC_PASSWORD"),
	}
	if err := validateConfig(config, secrets); err != nil {
		return Config{}, Secrets{}, err
	}
	return config, secrets, nil
}

func validateConfig(config Config, secrets Secrets) error {
	if config.Listen == "" || config.ESPBaseURL == "" || config.IPCBaseURL == "" || config.IPCSnapshotPath == "" || config.Go2RTCURL == "" || config.PlaybackURL == "" {
		return errors.New("listen, device URLs, and IPC snapshot path are required")
	}
	if config.MainStream == "" || config.SubStream == "" {
		return errors.New("mainStream and subStream are required")
	}
	if config.SessionHours < 1 || config.SessionHours > 168 {
		return errors.New("sessionHours must be between 1 and 168")
	}
	if len(config.Motors) < 1 || len(config.Motors) > 8 {
		return errors.New("between one and eight motors must be configured")
	}
	seen := map[int]bool{}
	for _, motor := range config.Motors {
		if motor.ID < 1 || motor.ID > 8 || seen[motor.ID] || strings.TrimSpace(motor.Name) == "" {
			return errors.New("motors must have unique IDs between 1 and 8 and non-empty names")
		}
		seen[motor.ID] = true
		if motor.DefaultSpeed < 10 || motor.DefaultSpeed > 1000 {
			return fmt.Errorf("motor %d defaultSpeed must be between 10 and 1000", motor.ID)
		}
		if motor.DefaultMode != "full" && motor.DefaultMode != "half" {
			return fmt.Errorf("motor %d defaultMode must be full or half", motor.ID)
		}
	}
	if secrets.AdminPasswordHash == "" || len(secrets.SessionSecret) < 32 || secrets.ESPAPIKey == "" {
		return errors.New("GATEWAY_ADMIN_PASSWORD_HASH, ESP_API_KEY and a 32+ character GATEWAY_SESSION_SECRET are required")
	}
	return nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func (c Config) SessionDuration() time.Duration {
	return time.Duration(c.SessionHours) * time.Hour
}
