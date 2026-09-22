// Package config loads the suite's JSON configuration and validates it at
// startup — a misconfigured suite refuses to start rather than failing per
// request.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Config is the whole configuration file.
type Config struct {
	Listen string `json:"listen"`
}

// Load reads and validates the file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	if strings.TrimSpace(c.Listen) == "" {
		return nil, fmt.Errorf("listen is required")
	}
	return &c, nil
}
