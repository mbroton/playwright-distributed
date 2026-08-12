package config

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
)

const HTTPWriteTimeout = 15 * time.Second

type Config struct {
	RedisHost             string `mapstructure:"REDIS_HOST"`
	RedisPort             int    `mapstructure:"REDIS_PORT"`
	MaxConcurrentSessions int    `mapstructure:"MAX_CONCURRENT_SESSIONS"`
	MaxLifetimeSessions   int    `mapstructure:"MAX_LIFETIME_SESSIONS"`
	ReaperRunInterval     int    `mapstructure:"REAPER_RUN_INTERVAL"`
	ShutdownCommandTTL    int    `mapstructure:"SHUTDOWN_COMMAND_TTL"`
	WorkerSelectTimeout   int    `mapstructure:"WORKER_SELECT_TIMEOUT"`
	LogLevel              string `mapstructure:"LOG_LEVEL"`
	LogFormat             string `mapstructure:"LOG_FORMAT"`
	DefaultBrowserType    string `mapstructure:"DEFAULT_BROWSER_TYPE"`
}

func LoadConfig() (*Config, error) {
	viper.BindEnv("REDIS_HOST")
	viper.BindEnv("REDIS_PORT")
	viper.BindEnv("LOG_LEVEL")
	viper.BindEnv("LOG_FORMAT")
	viper.BindEnv("MAX_CONCURRENT_SESSIONS")
	viper.BindEnv("MAX_LIFETIME_SESSIONS")
	viper.BindEnv("REAPER_RUN_INTERVAL")
	viper.BindEnv("SHUTDOWN_COMMAND_TTL")
	viper.BindEnv("WORKER_SELECT_TIMEOUT")
	viper.BindEnv("DEFAULT_BROWSER_TYPE")

	viper.SetDefault("MAX_CONCURRENT_SESSIONS", 5)
	viper.SetDefault("MAX_LIFETIME_SESSIONS", 50)
	viper.SetDefault("REAPER_RUN_INTERVAL", 300)
	viper.SetDefault("SHUTDOWN_COMMAND_TTL", 60)
	viper.SetDefault("WORKER_SELECT_TIMEOUT", 5)
	viper.SetDefault("DEFAULT_BROWSER_TYPE", "chromium")

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, err
	}

	if err := validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func validate(cfg *Config) error {
	if cfg.RedisHost == "" {
		return fmt.Errorf("REDIS_HOST is required")
	}
	if cfg.RedisPort == 0 {
		return fmt.Errorf("REDIS_PORT is required")
	}
	if cfg.MaxConcurrentSessions <= 0 {
		return fmt.Errorf("MAX_CONCURRENT_SESSIONS must be greater than zero")
	}
	if cfg.MaxLifetimeSessions <= 0 {
		return fmt.Errorf("MAX_LIFETIME_SESSIONS must be greater than zero")
	}
	if cfg.ReaperRunInterval <= 0 {
		return fmt.Errorf("REAPER_RUN_INTERVAL must be greater than zero")
	}
	if cfg.ShutdownCommandTTL <= 0 {
		return fmt.Errorf("SHUTDOWN_COMMAND_TTL must be greater than zero")
	}
	if cfg.WorkerSelectTimeout <= 0 {
		return fmt.Errorf("WORKER_SELECT_TIMEOUT must be greater than zero")
	}

	workerSelectTimeout := time.Duration(cfg.WorkerSelectTimeout) * time.Second
	if workerSelectTimeout >= HTTPWriteTimeout {
		return fmt.Errorf("WORKER_SELECT_TIMEOUT must be less than the HTTP write timeout (%s)", HTTPWriteTimeout)
	}

	allowedBrowserTypes := map[string]struct{}{
		"chromium": {},
		"firefox":  {},
		"webkit":   {},
	}

	if _, ok := allowedBrowserTypes[cfg.DefaultBrowserType]; !ok {
		return fmt.Errorf("DEFAULT_BROWSER_TYPE must be one of: chromium, firefox, webkit")
	}

	return nil
}
