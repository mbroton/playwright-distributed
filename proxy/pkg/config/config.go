package config

import (
	"fmt"
	"proxy/pkg/browser"

	"github.com/spf13/viper"
)

type Config struct {
	RedisHost                   string `mapstructure:"REDIS_HOST"`
	RedisPort                   int    `mapstructure:"REDIS_PORT"`
	MaxConcurrentSessions       int    `mapstructure:"MAX_CONCURRENT_SESSIONS"`
	MaxLifetimeSessions         int    `mapstructure:"MAX_LIFETIME_SESSIONS"`
	ReaperRunInterval           int    `mapstructure:"REAPER_RUN_INTERVAL"`
	ShutdownCommandTTL          int    `mapstructure:"SHUTDOWN_COMMAND_TTL"`
	ProxyReadHeaderTimeout      int    `mapstructure:"PROXY_READ_HEADER_TIMEOUT"`
	ProxyWorkerSelectionTimeout int    `mapstructure:"PROXY_WORKER_SELECTION_TIMEOUT"`
	ProxyConnectTimeout         int    `mapstructure:"PROXY_CONNECT_TIMEOUT"`
	LogLevel                    string `mapstructure:"LOG_LEVEL"`
	LogFormat                   string `mapstructure:"LOG_FORMAT"`
	DefaultBrowserType          string `mapstructure:"DEFAULT_BROWSER_TYPE"`
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
	viper.BindEnv("PROXY_READ_HEADER_TIMEOUT")
	viper.BindEnv("PROXY_WORKER_SELECTION_TIMEOUT")
	viper.BindEnv("PROXY_CONNECT_TIMEOUT")
	viper.BindEnv("DEFAULT_BROWSER_TYPE")

	viper.SetDefault("MAX_CONCURRENT_SESSIONS", 5)
	viper.SetDefault("MAX_LIFETIME_SESSIONS", 50)
	viper.SetDefault("REAPER_RUN_INTERVAL", 300)
	viper.SetDefault("SHUTDOWN_COMMAND_TTL", 60)
	viper.SetDefault("PROXY_READ_HEADER_TIMEOUT", 5)
	viper.SetDefault("PROXY_WORKER_SELECTION_TIMEOUT", 5)
	viper.SetDefault("PROXY_CONNECT_TIMEOUT", 5)
	viper.SetDefault("DEFAULT_BROWSER_TYPE", browser.Chromium)

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, err
	}

	// TODO: use validator
	if cfg.RedisHost == "" {
		return nil, fmt.Errorf("REDIS_HOST is required")
	}
	if cfg.RedisPort == 0 {
		return nil, fmt.Errorf("REDIS_PORT is required")
	}
	if cfg.ProxyReadHeaderTimeout <= 0 {
		return nil, fmt.Errorf("PROXY_READ_HEADER_TIMEOUT must be greater than 0")
	}
	if cfg.ProxyWorkerSelectionTimeout <= 0 {
		return nil, fmt.Errorf("PROXY_WORKER_SELECTION_TIMEOUT must be greater than 0")
	}
	if cfg.ProxyConnectTimeout <= 0 {
		return nil, fmt.Errorf("PROXY_CONNECT_TIMEOUT must be greater than 0")
	}

	if !browser.IsSupportedType(cfg.DefaultBrowserType) {
		return nil, fmt.Errorf("DEFAULT_BROWSER_TYPE must be one of: %s", browser.AllowedValuesText)
	}

	return &cfg, nil
}
