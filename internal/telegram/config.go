// Package telegram sends the alarm's alerts to Telegram.
package telegram

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// Config is the Telegram setup, read from a JSON file on the camera.
// The bot token is a secret: it must never appear in an error or a log.
type Config struct {
	BotToken string  `json:"bot_token"`
	ChatIDs  []int64 `json:"chat_ids,omitempty"`
	ChatID   int64   `json:"chat_id,omitempty"` // legacy, read only: merged into ChatIDs by LoadConfig
}

// LoadConfig reads path. A missing file is not an error: ok is false.
func LoadConfig(path string) (cfg Config, ok bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, false, nil
	}
	if err != nil {
		return Config{}, false, fmt.Errorf("read telegram config: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, false, fmt.Errorf("parse telegram config: %w", err)
	}
	if cfg.BotToken == "" {
		return Config{}, false, errors.New("telegram config: bot_token is empty")
	}
	ids := cfg.ChatIDs
	if cfg.ChatID != 0 {
		ids = append([]int64{cfg.ChatID}, ids...)
	}
	cfg.ChatIDs, cfg.ChatID = nil, 0
	if len(ids) == 0 {
		return Config{}, false, errors.New("telegram config: no chat id")
	}
	seen := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 {
			return Config{}, false, errors.New("telegram config: chat ids must be private chat ids (positive)")
		}
		if !seen[id] {
			seen[id] = true
			cfg.ChatIDs = append(cfg.ChatIDs, id)
		}
	}
	return cfg, true, nil
}
