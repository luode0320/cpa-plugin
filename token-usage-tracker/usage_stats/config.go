package usagestats

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultRetentionDays               = 365
	defaultFlushInterval               = 5 * time.Second
	defaultFlushMaxRecords             = 100
	defaultResponseCompression         = true
	defaultResponseCompressionMinBytes = 1024
	maxResponseCompressionMinBytes     = 16 << 20
)

type Config struct {
	DataPath                    string
	RetentionDays               int
	FlushInterval               time.Duration
	FlushMaxRecords             int
	SyncOnRecord                bool
	APIKeySecret                string
	ResponseCompression         bool
	ResponseCompressionMinBytes int
	// DerivedSessionEnabled controls whether RecordUsageRecord falls back to
	// a derived pseudo session_key (auth:<id>:<provider>:<alias>:<bucket>) when
	// the host-delivered SessionKey is empty. Real SessionKey values always win;
	// the derived key only fills the dashboard "会话" column when the host
	// broadcast carries no session information at all.
	DerivedSessionEnabled bool
	// DerivedSessionWindow bounds the time bucket used by the derivation. A
	// non-positive value is clamped to DefaultDerivedSessionWindow.
	DerivedSessionWindow time.Duration
}

type configYAML struct {
	DataPath                    string  `yaml:"data_path"`
	RetentionDays               *int    `yaml:"retention_days"`
	FlushInterval               string  `yaml:"flush_interval"`
	FlushMaxRecords             *int    `yaml:"flush_max_records"`
	SyncOnRecord                *bool   `yaml:"sync_on_record"`
	APIKeySecret                *string `yaml:"api_key_secret"`
	ResponseCompression         *bool   `yaml:"response_compression"`
	ResponseCompressionMinBytes *int    `yaml:"response_compression_min_bytes"`
}

func defaultConfig() Config {
	return Config{
		DataPath:                    resolvedDefaultDataPath(),
		RetentionDays:               defaultRetentionDays,
		FlushInterval:               defaultFlushInterval,
		FlushMaxRecords:             defaultFlushMaxRecords,
		SyncOnRecord:                true,
		APIKeySecret:                defaultAPIKeySecret,
		ResponseCompression:         defaultResponseCompression,
		ResponseCompressionMinBytes: defaultResponseCompressionMinBytes,
		DerivedSessionEnabled:       true,
		DerivedSessionWindow:        DefaultDerivedSessionWindow,
	}
}

func parseConfig(raw []byte) (Config, error) {
	cfg := defaultConfig()
	if len(raw) == 0 {
		return normalizeConfig(cfg)
	}

	var input configYAML
	if err := yaml.Unmarshal(raw, &input); err != nil {
		return Config{}, fmt.Errorf("parse config YAML: %w", err)
	}
	if strings.TrimSpace(input.DataPath) != "" {
		cfg.DataPath = strings.TrimSpace(input.DataPath)
	}
	if input.RetentionDays != nil {
		cfg.RetentionDays = *input.RetentionDays
	}
	if strings.TrimSpace(input.FlushInterval) != "" {
		interval, err := time.ParseDuration(strings.TrimSpace(input.FlushInterval))
		if err != nil {
			return Config{}, fmt.Errorf("parse flush_interval: %w", err)
		}
		cfg.FlushInterval = interval
	}
	if input.FlushMaxRecords != nil {
		cfg.FlushMaxRecords = *input.FlushMaxRecords
	}
	if input.SyncOnRecord != nil {
		cfg.SyncOnRecord = *input.SyncOnRecord
	}
	if input.APIKeySecret != nil {
		cfg.APIKeySecret = *input.APIKeySecret
	}
	if input.ResponseCompression != nil {
		cfg.ResponseCompression = *input.ResponseCompression
	}
	if input.ResponseCompressionMinBytes != nil {
		cfg.ResponseCompressionMinBytes = *input.ResponseCompressionMinBytes
	}
	cfg.DerivedSessionEnabled = true
	cfg.DerivedSessionWindow = DefaultDerivedSessionWindow
	return normalizeConfig(cfg)
}

func normalizeConfig(cfg Config) (Config, error) {
	if strings.TrimSpace(cfg.DataPath) == "" {
		return Config{}, fmt.Errorf("data_path must not be empty")
	}
	if cfg.RetentionDays < 1 || cfg.RetentionDays > 3650 {
		return Config{}, fmt.Errorf("retention_days must be between 1 and 3650")
	}
	if cfg.FlushInterval < time.Second || cfg.FlushInterval > time.Hour {
		return Config{}, fmt.Errorf("flush_interval must be between 1s and 1h")
	}
	if cfg.FlushMaxRecords < 1 || cfg.FlushMaxRecords > 1_000_000 {
		return Config{}, fmt.Errorf("flush_max_records must be between 1 and 1000000")
	}
	if cfg.APIKeySecret != "" && cfg.APIKeySecret != defaultAPIKeySecret && len([]byte(cfg.APIKeySecret)) < 32 {
		return Config{}, fmt.Errorf("api_key_secret must be empty, 123456, or at least 32 bytes")
	}
	if cfg.ResponseCompressionMinBytes < 0 || cfg.ResponseCompressionMinBytes > maxResponseCompressionMinBytes {
		return Config{}, fmt.Errorf("response_compression_min_bytes must be between 0 and %d", maxResponseCompressionMinBytes)
	}
	cfg.DerivedSessionWindow = NormalizeDerivedSessionWindow(cfg.DerivedSessionWindow)
	absolute, err := filepath.Abs(filepath.Clean(cfg.DataPath))
	if err != nil {
		return Config{}, fmt.Errorf("resolve data_path: %w", err)
	}
	cfg.DataPath = absolute
	return cfg, nil
}
