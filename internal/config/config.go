package config

import (
	"fmt"
	"os"
	"strings"
)

type Config struct {
	HTTPAddr              string
	DatabaseURL           string
	MockProviderURL       string
	ProviderWebhookSecret string
	DataDir               string
}

func Load() (Config, error) {
	db, err := requireEnv("DATABASE_URL")
	if err != nil {
		return Config{}, err
	}

	mock, err := requireEnv("MOCK_PROVIDER_URL")
	if err != nil {
		return Config{}, err
	}

	secret, err := requireEnv("PROVIDER_WEBHOOK_SECRET")
	if err != nil {
		return Config{}, err
	}

	return Config{
		HTTPAddr:              envOrDefault("HTTP_ADDR", ":8080"),
		DatabaseURL:           db,
		MockProviderURL:       mock,
		ProviderWebhookSecret: secret,
		DataDir:               envOrDefault("DATA_DIR", "data"),
	}, nil
}

func requireEnv(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("missing required environment variable: %s", name)
	}
	return value, nil
}

func envOrDefault(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}
