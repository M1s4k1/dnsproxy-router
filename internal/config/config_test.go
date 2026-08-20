package config

import (
	"os"
	"testing"
)

func writeTemp(s string) error {
	return os.WriteFile("/tmp/ecs_test_config.yaml", []byte(s), 0o644)
}

func TestLoadConfigWithECS(t *testing.T) {
	y := `
use_domain: true
domain: "example.com"
listeners:
  doh:
    enabled: true
    port: 443
    path: "/dns/query/v1"
ecs:
  mode: "override"
  address: "223.5.5.0/24"
cert:
  mode: "acme"
  cert_path: "/etc/dnsproxy/certs/fullchain.pem"
  key_path: "/etc/dnsproxy/certs/privkey.pem"
probe_interval: 15m
probe_timeout: 3s
probe_count: 3
dns:
  cf:
    DNS-over-HTTPS: "https://example.com/dns-query"
`
	// 写临时文件
	if err := writeTemp(y); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig("/tmp/ecs_test_config.yaml")
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if c.ECS.Mode != "override" || c.ECS.Address != "223.5.5.0/24" {
		t.Fatalf("ECS 解析错误: %+v", c.ECS)
	}
	if c.Listeners.DoH.Path != "/dns/query/v1" {
		t.Fatalf("DoH Path 解析错误: %q", c.Listeners.DoH.Path)
	}
	if !c.Listeners.DoH.Enabled {
		t.Fatalf("DoH 应已开启")
	}
}

func TestLoadConfigECSOffDefault(t *testing.T) {
	y := `
listeners:
  doh:
    enabled: true
cert:
  mode: "acme"
  cert_path: "/tmp/a.pem"
  key_path: "/tmp/b.pem"
dns:
  cf:
    DNS-over-HTTPS: "https://example.com/dns-query"
`
	if err := writeTemp(y); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig("/tmp/ecs_test_config.yaml")
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if c.ECS.Mode != "off" {
		t.Fatalf("ECS 默认应为 off，得到 %q", c.ECS.Mode)
	}
	if c.Listeners.DoH.Port != 443 {
		t.Fatalf("DoH 默认端口应为 443，得到 %d", c.Listeners.DoH.Port)
	}
}

func TestLoadConfigPlainOnlyNoTLS(t *testing.T) {
	y := `
listeners:
  plain_dns:
    enabled: true
    port: 53
dns:
  cf:
    Plain DNS: "udp://8.8.8.8:53"
`
	if err := writeTemp(y); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig("/tmp/ecs_test_config.yaml")
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if c.Listeners.NeedsTLS() {
		t.Fatalf("仅明文 DNS 不应需要 TLS")
	}
	if !c.Listeners.PlainDNS.Enabled {
		t.Fatalf("明文 DNS 应已开启")
	}
}

func TestLoadConfigNoListener(t *testing.T) {
	y := `
cert:
  mode: "acme"
  cert_path: "/tmp/a.pem"
  key_path: "/tmp/b.pem"
dns:
  cf:
    DNS-over-HTTPS: "https://example.com/dns-query"
`
	if err := writeTemp(y); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig("/tmp/ecs_test_config.yaml"); err == nil {
		t.Fatalf("未开启任何监听时应报错")
	}
}

func TestLoadConfigCacheFields(t *testing.T) {
	y := `
listeners:
  doh:
    enabled: true
cert:
  mode: "acme"
  cert_path: "/tmp/a.pem"
  key_path: "/tmp/b.pem"
cache_enabled: true
cache_size_bytes: 1024
cache_ttl: 5m
cache_eviction: lfu
dns:
  cf:
    DNS-over-HTTPS: "https://example.com/dns-query"
`
	if err := writeTemp(y); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig("/tmp/ecs_test_config.yaml")
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if c.CacheTTL == nil || c.CacheTTL.String() != "5m0s" {
		t.Fatalf("CacheTTL 解析错误: %v", c.CacheTTL)
	}
	if c.CacheEviction != "lfu" {
		t.Fatalf("CacheEviction 解析错误: %q", c.CacheEviction)
	}
}

func TestLoadConfigCacheTTLZero(t *testing.T) {
	y := `
listeners:
  doh:
    enabled: true
cert:
  mode: "acme"
  cert_path: "/tmp/a.pem"
  key_path: "/tmp/b.pem"
cache_ttl: 0s
dns:
  cf:
    DNS-over-HTTPS: "https://example.com/dns-query"
`
	if err := writeTemp(y); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig("/tmp/ecs_test_config.yaml")
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if c.CacheTTL == nil || c.CacheTTL.String() != "0s" {
		t.Fatalf("cache_ttl: 0s 应被保留（跟随记录 TTL），得到 %v", c.CacheTTL)
	}
}

func TestLoadConfigBadEviction(t *testing.T) {
	y := `
listeners:
  doh:
    enabled: true
cert:
  mode: "acme"
  cert_path: "/tmp/a.pem"
  key_path: "/tmp/b.pem"
cache_eviction: mru
dns:
  cf:
    DNS-over-HTTPS: "https://example.com/dns-query"
`
	if err := writeTemp(y); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig("/tmp/ecs_test_config.yaml"); err == nil {
		t.Fatalf("非法逐出策略应报错")
	}
}

func TestLoadConfigHosts(t *testing.T) {
	y := `
listeners:
  doh:
    enabled: true
cert:
  mode: "acme"
  cert_path: "/tmp/a.pem"
  key_path: "/tmp/b.pem"
hosts:
  dns.example.com:
    - "1.2.3.4"
    - "2001:db8::1"
ip_priority: ipv6
dns:
  cf:
    DNS-over-HTTPS: "https://example.com/dns-query"
`
	if err := writeTemp(y); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig("/tmp/ecs_test_config.yaml")
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if len(c.Hosts["dns.example.com"]) != 2 {
		t.Fatalf("Hosts 解析错误: %+v", c.Hosts)
	}
	if c.IPPriority != "ipv6" {
		t.Fatalf("IPPriority 应已开启，得到 %q", c.IPPriority)
	}
}

func TestLoadConfigBadHostsIP(t *testing.T) {
	y := `
listeners:
  doh:
    enabled: true
cert:
  mode: "acme"
  cert_path: "/tmp/a.pem"
  key_path: "/tmp/b.pem"
hosts:
  dns.example.com:
    - "not-an-ip"
dns:
  cf:
    DNS-over-HTTPS: "https://example.com/dns-query"
`
	if err := writeTemp(y); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig("/tmp/ecs_test_config.yaml"); err == nil {
		t.Fatalf("非法 hosts IP 应报错")
	}
}

func TestLoadConfigIPPriorityDefault(t *testing.T) {
	y := `
listeners:
  doh:
    enabled: true
cert:
  mode: "acme"
  cert_path: "/tmp/a.pem"
  key_path: "/tmp/b.pem"
dns:
  cf:
    DNS-over-HTTPS: "https://example.com/dns-query"
`
	if err := writeTemp(y); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig("/tmp/ecs_test_config.yaml")
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if c.IPPriority != "ipv4" {
		t.Fatalf("IPPriority 默认应为 ipv4，得到 %q", c.IPPriority)
	}
}

func TestLoadConfigBadIPPriority(t *testing.T) {
	y := `
listeners:
  doh:
    enabled: true
cert:
  mode: "acme"
  cert_path: "/tmp/a.pem"
  key_path: "/tmp/b.pem"
ip_priority: foo
dns:
  cf:
    DNS-over-HTTPS: "https://example.com/dns-query"
`
	if err := writeTemp(y); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig("/tmp/ecs_test_config.yaml"); err == nil {
		t.Fatalf("非法 ip_priority 应报错")
	}
}

func TestLoadConfigUpstreamModeDefault(t *testing.T) {
	y := `
listeners:
  doh:
    enabled: true
cert:
  mode: "acme"
  cert_path: "/tmp/a.pem"
  key_path: "/tmp/b.pem"
dns:
  cf:
    DNS-over-HTTPS: "https://example.com/dns-query"
`
	if err := writeTemp(y); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig("/tmp/ecs_test_config.yaml")
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if c.UpstreamMode != "fastest" {
		t.Fatalf("UpstreamMode 默认应为 fastest，得到 %q", c.UpstreamMode)
	}
	if c.RaceWindow.String() != "50ms" {
		t.Fatalf("RaceWindow 默认应为 50ms，得到 %q", c.RaceWindow)
	}
	if c.Weight("cf") != 1 {
		t.Fatalf("未配置权重应默认 1，得到 %d", c.Weight("cf"))
	}
}

func TestLoadConfigWeighted(t *testing.T) {
	y := `
listeners:
  doh:
    enabled: true
cert:
  mode: "acme"
  cert_path: "/tmp/a.pem"
  key_path: "/tmp/b.pem"
upstream_mode: weighted
race_window: 80ms
upstream_weights:
  cf: 8
  adguard: 20
dns:
  cf:
    DNS-over-HTTPS: "https://example.com/dns-query"
  adguard:
    DNS-over-TLS: "tls://dns.adguard.com:853"
`
	if err := writeTemp(y); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig("/tmp/ecs_test_config.yaml")
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if c.UpstreamMode != "weighted" {
		t.Fatalf("UpstreamMode 解析错误: %q", c.UpstreamMode)
	}
	if c.RaceWindow.String() != "80ms" {
		t.Fatalf("RaceWindow 解析错误: %q", c.RaceWindow)
	}
	if c.Weight("cf") != 8 || c.Weight("adguard") != 20 {
		t.Fatalf("权重解析错误: cf=%d adguard=%d", c.Weight("cf"), c.Weight("adguard"))
	}
}

func TestLoadConfigBadUpstreamMode(t *testing.T) {
	y := `
listeners:
  doh:
    enabled: true
cert:
  mode: "acme"
  cert_path: "/tmp/a.pem"
  key_path: "/tmp/b.pem"
upstream_mode: foo
dns:
  cf:
    DNS-over-HTTPS: "https://example.com/dns-query"
`
	if err := writeTemp(y); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig("/tmp/ecs_test_config.yaml"); err == nil {
		t.Fatalf("非法 upstream_mode 应报错")
	}
}

func TestLoadConfigBadWeight(t *testing.T) {
	y := `
listeners:
  doh:
    enabled: true
cert:
  mode: "acme"
  cert_path: "/tmp/a.pem"
  key_path: "/tmp/b.pem"
upstream_mode: weighted
upstream_weights:
  cf: 150
dns:
  cf:
    DNS-over-HTTPS: "https://example.com/dns-query"
`
	if err := writeTemp(y); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig("/tmp/ecs_test_config.yaml"); err == nil {
		t.Fatalf("非法权重（>100）应报错")
	}
}

func TestUpstreamTargets(t *testing.T) {
	c := Config{
		DNS: map[string]map[string]string{
			"p1": {
				"DNS-over-HTTPS": "https://doh.example.com:443/dns-query",
				"DNS-over-TLS":   "tls://dot.example.com:853",
			},
			"p2": {
				"Plain DNS": "udp://1.1.1.1:53",
			},
		},
		Hosts: map[string][]string{
			"onlyhosts.example.com": {"1.2.3.4"},
		},
	}

	targets := c.UpstreamTargets()
	got := make(map[string]uint16, len(targets))
	for _, t := range targets {
		got[t.Host] = t.Port
	}

	if got["doh.example.com"] != 443 {
		t.Fatalf("DoH 端口应为 443，得到 %d", got["doh.example.com"])
	}
	if got["dot.example.com"] != 853 {
		t.Fatalf("DoT 端口应为 853，得到 %d", got["dot.example.com"])
	}
	if got["onlyhosts.example.com"] != 443 {
		t.Fatalf("hosts-only 域名默认端口应为 443，得到 %d", got["onlyhosts.example.com"])
	}
	if _, ok := got["1.1.1.1"]; ok {
		t.Fatalf("IP 主机不应出现在探测目标中")
	}
}
