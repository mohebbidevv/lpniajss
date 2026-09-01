package config

import (
	"fmt"
	"golaunch/internal/infrastructure/utils"
	"os"
	"path/filepath"
)

type AppConfig struct {
	Server ServerConfig
	DB     DBConfig
}

type ServerConfig struct {
	Port string `json:"port"`
}

type DBConfig struct {
	Host     string
	Port     int    `json:"port"`
	Name     string `json:"name"`
	User     string `json:"user"`
	Password string `json:"password"`
	SSLMode  string `json:"ssl_mode"` // disable/require
}

func LoadConfig() (AppConfig, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return AppConfig{}, fmt.Errorf("failed to get current working directory: %w", err)
	}

	configPath := filepath.Join(cwd, "cmd/configuration.json")
	
	cfg, err := utils.OpenJSON[AppConfig](configPath)
	if err != nil {
		return AppConfig{}, err
	}

	if cfg.Server.Port == "" {
		cfg.Server.Port = "8080"
	}
	// FUCKED UP REGEX PATTERN
	// hostnamePattern := `^(?=.{1,253})(?!-)[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*[A-Za-z0-9]$`
	// re := regexp.MustCompile(hostnamePattern)

	if cfg.DB.Host == "" || cfg.DB.User == "" || cfg.DB.Password == "" {
		return AppConfig{}, fmt.Errorf("Please Fill The Required Fields")
	}

	// FUCKED UP REGEX PATTERN
	// if !re.MatchString(cfg.DB.Host) {
	// 	return AppConfig{}, fmt.Errorf("Invalid hostname format: %s", cfg.DB.Host)
	// }

	return cfg, nil
}
