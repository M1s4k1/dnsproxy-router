// Package config 定义并加载 dnsproxy-scheduler 的 YAML 配置。
package config

import (
	"fmt"
	"net/url"
	"os"
	"time"

	"dnsproxy-scheduler/internal/ecs"
	"gopkg.in/yaml.v3"
)

// CertConfig 描述 TLS 证书的获取方式。
type CertConfig struct {
	// Mode: "acme"（用 acme.sh 申请并自动续期）或 "existing"（使用已有证书）。
	Mode string `yaml:"mode"`
	// Renew: acme 模式下是否自动续期（默认 true）。
	Renew bool `yaml:"renew"`
	// CertPath / KeyPath: 证书与私钥路径。
	// acme 模式为「安装目标路径」，existing 模式为「已有证书源路径」。
	CertPath string `yaml:"cert_path"`
	KeyPath  string `yaml:"key_path"`
	// Email: acme 注册邮箱。
	Email string `yaml:"email"`
	// Provider: DNS API 提供商（acme DNS-01 用），如 cloudflare。
	Provider string `yaml:"provider"`
}

// ECSConfig 描述发往上游的 EDNS Client Subnet 处理策略。
type ECSConfig struct {
	// Mode: "off"（移除 EDNS）/ "pass"（透传）/ "override"（覆写）。
	Mode string `yaml:"mode"`
	// Address: override 模式的固定前缀，如 "223.5.5.0/24"。
	Address string `yaml:"address"`
}

// Config 是服务的完整配置，同时被 Go 程序与交互式脚本消费。
type Config struct {
	// Domain: 服务域名（用于证书申请与验证）。
	Domain string `yaml:"domain"`
	// UseDomain: 是否使用域名（true 时用域名签发证书）。
	UseDomain bool `yaml:"use_domain"`
	// Port: 对外 DoH 监听端口。
	Port int `yaml:"port"`
	// DoHPath: DoH 端点请求路径，可自定义为任意层级（如 /dns/query/v1）。
	DoHPath string `yaml:"doh_path"`
	// ECS: EDNS Client Subnet 处理策略。
	ECS ECSConfig `yaml:"ecs"`
	// Cert: 证书配置。
	Cert CertConfig `yaml:"cert"`
	// ProbeInterval: 探测周期。
	ProbeInterval time.Duration `yaml:"probe_interval"`
	// ProbeTimeout: 单次探测超时。
	ProbeTimeout time.Duration `yaml:"probe_timeout"`
	// ProbeCount: 每模式每轮探测次数（取中位数）。
	ProbeCount int `yaml:"probe_count"`
	// ProbeDomain: 探测用域名。
	ProbeDomain string `yaml:"probe_domain"`
	// CacheEnabled: 是否开启响应缓存（nil 视为 true，保持默认开启）。
	CacheEnabled *bool `yaml:"cache_enabled"`
	// CacheSizeBytes: 响应缓存大小（字节），仅在 CacheEnabled 时生效。
	CacheSizeBytes int `yaml:"cache_size_bytes"`
	// Bootstrap: 解析上游主机名用的引导 DNS（明文）。
	Bootstrap []string `yaml:"bootstrap"`
	// BootstrapCacheTTL: 引导解析结果的缓存时长（0 表示不缓存）。
	BootstrapCacheTTL time.Duration `yaml:"bootstrap_cache_ttl"`
	// DNS: 上游服务商 → 查询模式 → 地址。
	// 模式键形如 "DNS-over-HTTPS" / "DNS-over-TLS" / "DNS-over-QUIC"。
	DNS map[string]map[string]string `yaml:"dns"`
}

// ListenAddr 返回对外监听地址（始终监听所有网卡）。
func (c Config) ListenAddr() string {
	return fmt.Sprintf("0.0.0.0:%d", c.Port)
}

// boolPtr 返回指向 b 的指针，用于构造可选 bool 字段的默认值。
func boolPtr(b bool) *bool {
	return &b
}

// validateDoHPath 校验 DoH 请求路径：必须以 / 开头、不含查询串/片段、无非法转义。
func validateDoHPath(p string) error {
	if p == "" {
		return nil
	}
	if p[0] != '/' {
		return fmt.Errorf("doh_path 必须以 / 开头，当前为 %q", p)
	}
	if _, err := url.ParseRequestURI(p); err != nil {
		return fmt.Errorf("doh_path 非法 %q: %w", p, err)
	}
	if p != "/" && p[len(p)-1] == '/' {
		return fmt.Errorf("doh_path 不能以 / 结尾（%q）", p)
	}
	return nil
}

// DefaultConfig 返回内置默认配置。
func DefaultConfig() Config {
	return Config{
		Domain:    "example.com",
		UseDomain: true,
		Port:      443,
		DoHPath:   "/dns-query",
		ECS:       ECSConfig{Mode: "off", Address: ""},
		Cert: CertConfig{
			Mode:     "acme",
			Renew:    true,
			CertPath: "/etc/dnsproxy/certs/fullchain.pem",
			KeyPath:  "/etc/dnsproxy/certs/privkey.pem",
			Email:    "you@example.com",
			Provider: "cloudflare",
		},
		ProbeInterval:  15 * time.Minute,
		ProbeTimeout:   3 * time.Second,
		ProbeCount:     3,
		ProbeDomain:    "example.com.",
		CacheEnabled:   boolPtr(true),
		CacheSizeBytes: 64 * 1024 * 1024,
		Bootstrap:      []string{"1.1.1.1:53", "8.8.8.8:53"},
		// BootstrapCacheTTL 默认 0（不缓存），需显式配置才缓存。
		// DNS 默认留空：上游端点属用户私密配置，请通过 config.yaml 或
		// interactive-setup.sh 自行填写（空值会被 LoadConfig 校验拒绝）。
		DNS: map[string]map[string]string{},
	}
}

// LoadConfig 从 YAML 文件加载配置。
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	if err := c.normalize(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// normalize 校验并补全默认值。
func (c *Config) normalize() error {
	if c.Port <= 0 {
		c.Port = 443
	}
	if c.DoHPath == "" {
		c.DoHPath = "/dns-query"
	}
	if err := validateDoHPath(c.DoHPath); err != nil {
		return err
	}
	if c.ECS.Mode == "" {
		c.ECS.Mode = "off"
	}
	if _, err := ecs.New(c.ECS.Mode, c.ECS.Address); err != nil {
		return err
	}
	if c.Cert.CertPath == "" || c.Cert.KeyPath == "" {
		return fmt.Errorf("cert.cert_path 和 cert.key_path 不能为空")
	}
	if c.Cert.Mode != "acme" && c.Cert.Mode != "existing" {
		return fmt.Errorf("cert.mode 必须为 acme 或 existing，当前为 %q", c.Cert.Mode)
	}
	if c.ProbeInterval <= 0 {
		c.ProbeInterval = 15 * time.Minute
	}
	if c.ProbeTimeout <= 0 {
		c.ProbeTimeout = 3 * time.Second
	}
	if c.ProbeCount <= 0 {
		c.ProbeCount = 3
	}
	if c.ProbeDomain == "" {
		c.ProbeDomain = "example.com."
	}
	if c.CacheEnabled == nil {
		c.CacheEnabled = boolPtr(true)
	}
	if c.CacheSizeBytes <= 0 {
		c.CacheSizeBytes = 64 * 1024 * 1024
	}
	if len(c.Bootstrap) == 0 {
		c.Bootstrap = []string{"1.1.1.1:53", "8.8.8.8:53"}
	}
	if len(c.DNS) == 0 {
		return fmt.Errorf("dns 不能为空")
	}
	for name, modes := range c.DNS {
		if name == "" {
			return fmt.Errorf("dns 键（服务商名）不能为空")
		}
		if len(modes) == 0 {
			return fmt.Errorf("服务商 %s 的查询模式不能为空", name)
		}
	}
	return nil
}
