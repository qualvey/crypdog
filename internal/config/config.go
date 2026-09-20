package config

import (
	"fmt"
	"os"

	"crypdog/internal/model"

	"github.com/ilyakaznacheev/cleanenv"
)

// Config 根配置结构体
type Config struct {
	Env      string         `yaml:"env" env:"APP_ENV" env-default:"development"`
	Server   ServerConfig   `yaml:"server"`
	Webhook  WebhookConfig  `yaml:"webhook"`
	Database DatabaseConfig `yaml:"database"`
	Monitor  MonitorConfig  `yaml:"monitor"`
	Log      LogConfig      `yaml:"log"`
	Metrics  MetricsConfig  `yaml:"metrics"`
	Pprof    PprofConfig    `yaml:"pprof"`
	//全大写
	Chains         map[model.Chain]ChainNodeConfig `yaml:"chains"`
	InitialWallets []InitialWallet                 `yaml:"initial_wallets"`
}

// ChainNodeConfig 节点配置
type ChainNodeConfig struct {
	Enabled       *bool    `yaml:"enabled"`
	Driver        string   `yaml:"driver"` // evm / tron / solana，便于工厂分发
	RPCURL        string   `yaml:"rpc_url"`
	BackupRPCURLs []string `yaml:"backup_rpc_urls"` // 容灾备用 RPC
	APIKey        string   `yaml:"api_key"`         // TronGrid 或 Alchemy/Infura 专用 Key

	ScanIntervalSec int `yaml:"scan_interval_sec" env-default:"3"`
	// 扫链核心参数（防丢单、防分叉）
	Confirmations uint64            `yaml:"confirmations" env-default:"12"` // 确认块数
	BlockDelay    uint64            `yaml:"block_delay" env-default:"10"`   // 安全滞后扫描，防软分叉
	BatchSize     uint64            `yaml:"batch_size" env-default:"500"`   // 单次批量获取块数
	StartBlock    uint64            `yaml:"start_block"`                    // 首次启动若无记录从哪个块开始
	Tokens        []model.TokenSpec `yaml:"tokens"`                         //

	// 可选的异构专有扩展（如果某条链有极其特殊的参数）
	Extra map[string]string `yaml:"extra"`
}

// InitialWallet 初始收款钱包
type InitialWallet struct {
	Chain   string `yaml:"chain"`
	Address string `yaml:"address"`
	Label   string `yaml:"label"`
}

// DatabaseConfig 数据库配置
type DatabaseConfig struct {
	Driver string `yaml:"driver" env:"DB_DRIVER" env-default:"sqlite"`
	DSN    string `yaml:"dsn" env:"DB_DSN" env-default:"crypdog.db"`
}

type ServerConfig struct {
	Port             string   `yaml:"port" env:"CRYPDOG_PORT" env-default:"8080"`
	Secret           string   `yaml:"secret" env:"CRYPDOG_SECRET" env-default:"crypdog-secret-key-123456"`
	AdminSecret      string   `yaml:"admin_secret" env:"CRYPDOG_ADMIN_SECRET"`
	AllowedOrigins   []string `yaml:"allowed_origins" env:"CRYPDOG_ALLOWED_ORIGINS"`
	RateLimitPerMin  int      `yaml:"rate_limit_per_minute" env:"CRYPDOG_RATE_LIMIT_PER_MINUTE" env-default:"300"`
	EnableSimulation bool     `yaml:"enable_simulation" env:"CRYPDOG_ENABLE_SIMULATION" env-default:"false"`
}
type WebhookConfig struct {
	Secret     string `yaml:"webhook_secret" env:"CRYPDOG_WEBHOOK_SECRET" env-default:"crypdog-webhook-secret-987654"`
	MaxRetries int    `yaml:"max_retries" env-default:"3"`
	TimeoutSec int    `yaml:"timeout_sec" env-default:"5"`
	AllowLocal bool   `yaml:"allow_local" env:"WEBHOOK_ALLOW_LOCAL" env-default:"false"`
}

type MonitorConfig struct {
}

// LogConfig 日志配置
type LogConfig struct {
	Level    string `yaml:"level" env:"LOG_LEVEL" env-default:"info"`
	Format   string `yaml:"format" env:"LOG_FORMAT" env-default:"text"`   // text 或 json
	Output   string `yaml:"output" env:"LOG_OUTPUT" env-default:"stdout"` // stdout 或 file
	FilePath string `yaml:"file_path" env:"LOG_FILE_PATH"`                // 文件路径，如 logs/crypdog.log
}

// MetricsConfig 指标暴露配置
type MetricsConfig struct {
	Enabled bool   `yaml:"enabled" env:"METRICS_ENABLED" env-default:"true"`
	Path    string `yaml:"path" env:"METRICS_PATH" env-default:"/metrics"`
	Secret  string `yaml:"secret" env:"CRYPDOG_METRICS_SECRET"`
}

// PprofConfig 性能探针配置
type PprofConfig struct {
	Enabled bool `yaml:"enabled" env:"PPROF_ENABLED" env-default:"false"`
}

// ValidateProduction 校验生产环境配置，防御弱口令与不安全配置
func (c *Config) ValidateProduction() error {
	if c.Env == "production" || c.Env == "prod" {
		if c.Server.Secret == "crypdog-secret-key-123456" || len(c.Server.Secret) < 16 {
			return fmt.Errorf("生产环境安全阻断: service_secret 不能使用默认弱口令，且长度须 >= 16 字符")
		}
		if c.Server.AdminSecret == "" || len(c.Server.AdminSecret) < 16 || c.Server.AdminSecret == c.Server.Secret {
			return fmt.Errorf("生产环境安全阻断: admin_secret 必须配置长度 >= 16 且不能复用 service_secret")
		}
		if c.Webhook.Secret == "crypdog-webhook-secret-987654" || len(c.Webhook.Secret) < 16 {
			return fmt.Errorf("生产环境安全阻断: webhook_secret 不能使用默认弱口令，且长度须 >= 16 字符")
		}
		if c.Webhook.AllowLocal {
			return fmt.Errorf("生产环境安全阻断: webhook.allow_local 必须为 false 以严格防御 SSRF 攻击")
		}
		if c.Server.EnableSimulation {
			return fmt.Errorf("生产环境安全阻断: server.enable_simulation 必须为 false，严禁生产环境开启模拟入账接口")
		}
		if c.Metrics.Enabled && (c.Metrics.Secret == "" || len(c.Metrics.Secret) < 16) {
			return fmt.Errorf("生产环境安全阻断: metrics.secret 必须配置且长度须 >= 16 字符")
		}
	}
	return nil
}

// LoadConfig 最佳实践加载流程：优先读 YAML 文件，随后读取环境变量覆盖
func LoadConfig(configPath ...string) (*Config, error) {
	var cfg Config

	targetPath := "config.yaml"
	if len(configPath) > 0 && configPath[0] != "" {
		targetPath = configPath[0]
	}

	// 1. 如果文件存在，先读 YAML 配置文件
	if _, err := os.Stat(targetPath); err == nil {
		if err := cleanenv.ReadConfig(targetPath, &cfg); err != nil {
			return nil, fmt.Errorf("解析配置文件失败 (%s): %w", targetPath, err)
		}
	} else {
		// 2. 如果无配置文件（如 Pure Docker 容器环境），直接纯读取环境变量
		if err := cleanenv.ReadEnv(&cfg); err != nil {
			return nil, fmt.Errorf("解析环境变量配置失败: %w", err)
		}
	}

	// 3. 生产就绪校验
	if err := cfg.ValidateProduction(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// GetRequiredConfirmations 获取指定公链的安全确认数
func (c *Config) GetRequiredConfirmations(chain model.Chain) uint64 {
	if nodeCfg, exists := c.Chains[chain]; exists && nodeCfg.Confirmations > 0 {
		return nodeCfg.Confirmations
	}
	return 12 // 兜底安全值
}
