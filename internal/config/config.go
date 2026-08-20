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

// ListenerConfig 描述一种入站监听协议。
type ListenerConfig struct {
	// Enabled: 是否开启该入站监听。
	Enabled bool `yaml:"enabled"`
	// Port: 监听端口（各协议有各自默认值）。
	Port int `yaml:"port"`
	// Path: 仅 DoH 使用，为 DoH 端点请求路径。
	Path string `yaml:"path"`
}

// ListenersConfig 描述入站监听：DoH / DoT / DoQ / 明文 DNS（UDP+TCP）。
type ListenersConfig struct {
	DoH      ListenerConfig `yaml:"doh"`
	DoT      ListenerConfig `yaml:"dot"`
	DoQ      ListenerConfig `yaml:"doq"`
	PlainDNS ListenerConfig `yaml:"plain_dns"`
}

// NeedsTLS 报告是否需要 TLS 证书：DoH/DoT/DoQ 任一开启即需要。
func (l ListenersConfig) NeedsTLS() bool {
	return l.DoH.Enabled || l.DoT.Enabled || l.DoQ.Enabled
}

// Config 是服务的完整配置，同时被 Go 程序与交互式脚本消费。
type Config struct {
	// Domain: 服务域名（用于证书申请与验证）。
	Domain string `yaml:"domain"`
	// UseDomain: 是否使用域名（true 时用域名签发证书）。
	UseDomain bool `yaml:"use_domain"`
	// Listeners: 入站监听配置。
	Listeners ListenersConfig `yaml:"listeners"`
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
	// CacheTTL: 缓存固定过期时间（如 30m）。<=0 表示跟随记录自身的 TTL。
	CacheTTL time.Duration `yaml:"cache_ttl"`
	// CacheEviction: 缓存逐出策略：fifo / lru / lfu。
	CacheEviction string `yaml:"cache_eviction"`
	// Bootstrap: 解析上游主机名用的引导 DNS（明文）。
	Bootstrap []string `yaml:"bootstrap"`
	// BootstrapCacheTTL: 引导解析结果的缓存时长（0 表示不缓存）。
	BootstrapCacheTTL time.Duration `yaml:"bootstrap_cache_ttl"`
	// DNS: 上游服务商 → 查询模式 → 地址。
	// 模式键形如 "DNS-over-HTTPS" / "DNS-over-TLS" / "DNS-over-QUIC" / "Plain DNS"。
	DNS map[string]map[string]string `yaml:"dns"`
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
		Listeners: ListenersConfig{
			DoH:      ListenerConfig{Enabled: true, Port: 443, Path: "/dns-query"},
			DoT:      ListenerConfig{Enabled: false, Port: 853},
			DoQ:      ListenerConfig{Enabled: false, Port: 853},
			PlainDNS: ListenerConfig{Enabled: false, Port: 53},
		},
		ECS: ECSConfig{Mode: "off", Address: ""},
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
		CacheTTL:       30 * time.Minute,
		CacheEviction:  "lru",
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

// defaultPort 返回入站监听协议的默认端口。
func defaultPort(l ListenerConfig, def int) int {
	if l.Port <= 0 {
		return def
	}
	return l.Port
}

// validateListeners 校验入站监听配置：至少开启一种，端口合法。
func (c *Config) validateListeners() error {
	c.Listeners.DoH.Port = defaultPort(c.Listeners.DoH, 443)
	c.Listeners.DoT.Port = defaultPort(c.Listeners.DoT, 853)
	c.Listeners.DoQ.Port = defaultPort(c.Listeners.DoQ, 853)
	c.Listeners.PlainDNS.Port = defaultPort(c.Listeners.PlainDNS, 53)

	if !c.Listeners.DoH.Enabled && !c.Listeners.DoT.Enabled &&
		!c.Listeners.DoQ.Enabled && !c.Listeners.PlainDNS.Enabled {
		return fmt.Errorf("至少开启一种入站监听（doh/dot/doq/plain_dns）")
	}

	validatePort := func(name string, l ListenerConfig) error {
		if l.Enabled && (l.Port < 1 || l.Port > 65535) {
			return fmt.Errorf("%s 端口非法：%d", name, l.Port)
		}
		return nil
	}
	for name, l := range map[string]ListenerConfig{
		"listeners.doh":       c.Listeners.DoH,
		"listeners.dot":       c.Listeners.DoT,
		"listeners.doq":       c.Listeners.DoQ,
		"listeners.plain_dns": c.Listeners.PlainDNS,
	} {
		if err := validatePort(name, l); err != nil {
			return err
		}
	}

	if c.Listeners.DoH.Enabled {
		if c.Listeners.DoH.Path == "" {
			c.Listeners.DoH.Path = "/dns-query"
		}
		if err := validateDoHPath(c.Listeners.DoH.Path); err != nil {
			return err
		}
	}
	return nil
}

// normalize 校验并补全默认值。
func (c *Config) normalize() error {
	if err := c.validateListeners(); err != nil {
		return err
	}
	if c.ECS.Mode == "" {
		c.ECS.Mode = "off"
	}
	if _, err := ecs.New(c.ECS.Mode, c.ECS.Address); err != nil {
		return err
	}
	// 仅当需要 TLS（DoH/DoT/DoQ 任一开启）时才校验证书路径。
	if c.Listeners.NeedsTLS() {
		if c.Cert.CertPath == "" || c.Cert.KeyPath == "" {
			return fmt.Errorf("cert.cert_path 和 cert.key_path 不能为空")
		}
		if c.Cert.Mode != "acme" && c.Cert.Mode != "existing" {
			return fmt.Errorf("cert.mode 必须为 acme 或 existing，当前为 %q", c.Cert.Mode)
		}
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
	if c.CacheTTL <= 0 {
		c.CacheTTL = 30 * time.Minute
	}
	if c.CacheEviction == "" {
		c.CacheEviction = "lru"
	}
	switch c.CacheEviction {
	case "fifo", "lru", "lfu":
	default:
		return fmt.Errorf("cache_eviction 必须为 fifo/lru/lfu，当前为 %q", c.CacheEviction)
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
