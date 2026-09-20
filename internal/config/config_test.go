package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConfig_ValidateProduction(t *testing.T) {
	t.Run("development allows default secrets and local webhook", func(t *testing.T) {
		cfg := Config{
			Env: "development",
			Server: ServerConfig{
				Secret:           "crypdog-secret-key-123456",
				EnableSimulation: true,
			},
			Webhook: WebhookConfig{
				Secret:     "crypdog-webhook-secret-987654",
				AllowLocal: true,
			},
		}
		err := cfg.ValidateProduction()
		assert.NoError(t, err)
	})

	t.Run("production rejects default service secret", func(t *testing.T) {
		cfg := Config{
			Env: "production",
			Server: ServerConfig{
				Secret: "crypdog-secret-key-123456",
			},
			Webhook: WebhookConfig{
				Secret:     "a-valid-production-secret-987654321",
				AllowLocal: false,
			},
		}
		err := cfg.ValidateProduction()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "service_secret 不能使用默认弱口令")
	})

	t.Run("production rejects short service secret", func(t *testing.T) {
		cfg := Config{
			Env: "production",
			Server: ServerConfig{
				Secret: "short-key",
			},
			Webhook: WebhookConfig{
				Secret:     "a-valid-production-secret-987654321",
				AllowLocal: false,
			},
		}
		err := cfg.ValidateProduction()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "service_secret 不能使用默认弱口令，且长度须 >= 16 字符")
	})

	t.Run("production rejects default webhook secret", func(t *testing.T) {
		cfg := Config{
			Env: "production",
			Server: ServerConfig{
				Secret:      "a-valid-production-service-secret-123",
				AdminSecret: "a-valid-production-admin-secret-987654321",
			},
			Webhook: WebhookConfig{
				Secret:     "crypdog-webhook-secret-987654",
				AllowLocal: false,
			},
		}
		err := cfg.ValidateProduction()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "webhook_secret 不能使用默认弱口令")
	})

	t.Run("production rejects allow_local", func(t *testing.T) {
		cfg := Config{
			Env: "production",
			Server: ServerConfig{
				Secret:      "a-valid-production-service-secret-123",
				AdminSecret: "a-valid-production-admin-secret-987654321",
			},
			Webhook: WebhookConfig{
				Secret:     "a-valid-production-webhook-secret-987",
				AllowLocal: true,
			},
		}
		err := cfg.ValidateProduction()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "webhook.allow_local 必须为 false")
	})

	t.Run("production rejects simulation enabled", func(t *testing.T) {
		cfg := Config{
			Env: "production",
			Server: ServerConfig{
				Secret:           "a-valid-production-service-secret-123",
				AdminSecret:      "a-valid-production-admin-secret-987654321",
				EnableSimulation: true,
			},
			Webhook: WebhookConfig{
				Secret:     "a-valid-production-webhook-secret-987",
				AllowLocal: false,
			},
		}
		err := cfg.ValidateProduction()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "server.enable_simulation 必须为 false")
	})

	t.Run("production valid config succeeds", func(t *testing.T) {
		cfg := Config{
			Env: "production",
			Server: ServerConfig{
				Secret:           "my-strong-production-service-key-xyz-123",
				AdminSecret:      "my-strong-production-admin-key-xyz-123",
				EnableSimulation: false,
			},
			Webhook: WebhookConfig{
				Secret:     "my-strong-production-webhook-key-abc-456",
				AllowLocal: false,
			},
			Metrics: MetricsConfig{
				Enabled: true,
				Secret:  "my-strong-production-metrics-key-abc-456",
			},
		}
		err := cfg.ValidateProduction()
		assert.NoError(t, err)
	})
}
func TestConfig_LoadFiles(t *testing.T) {
	t.Run("load config.yaml", func(t *testing.T) {
		cfg, err := LoadConfig("../../config.yaml")
		assert.NoError(t, err)
		assert.NotNil(t, cfg)
		assert.Equal(t, "8080", cfg.Server.Port)
		assert.Equal(t, "crypdog-secret-key-123456", cfg.Server.Secret)
		assert.NotEmpty(t, cfg.Chains)
	})

	t.Run("load config.example.yaml", func(t *testing.T) {
		cfg, err := LoadConfig("../../config.example.yaml")
		assert.NoError(t, err)
		assert.NotNil(t, cfg)
		assert.Equal(t, "8080", cfg.Server.Port)
		assert.Equal(t, "crypdog-secret-key-123456", cfg.Server.Secret)
		assert.NotEmpty(t, cfg.Chains)
	})
}
