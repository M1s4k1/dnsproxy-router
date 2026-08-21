package config

import (
	"os"
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"15m", 15 * time.Minute, true},
		{"3s", 3 * time.Second, true},
		{"1h", time.Hour, true},
		{"100ms", 100 * time.Millisecond, true},
		{"2d", 48 * time.Hour, true},
		{"2d12h", 60 * time.Hour, true},
		{"2d12h30m", 60*time.Hour + 30*time.Minute, true},
		{"1d30m", 24*time.Hour + 30*time.Minute, true},
		{"0s", 0, true},
		{"1.5h", 90 * time.Minute, true},
		{"1x", 0, false},
		{"d", 0, false},
		{"2dh", 0, false},
		{"2d-3h", 0, false},
		{"2d+3h", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, err := ParseDuration(c.in)
		if c.ok && err != nil {
			t.Fatalf("ParseDuration(%q) 应成功，得到 err=%v", c.in, err)
		}
		if !c.ok && err == nil {
			t.Fatalf("ParseDuration(%q) 应失败，得到 %v", c.in, got)
		}
		if c.ok && got != c.want {
			t.Fatalf("ParseDuration(%q) = %v，期望 %v", c.in, got, c.want)
		}
	}
}

func TestLoadConfigDayDuration(t *testing.T) {
	y := `
listeners:
  doh:
    enabled: true
cert:
  mode: "acme"
  cert_path: "/tmp/a.pem"
  key_path: "/tmp/b.pem"
probe_interval: 2d
cache_ttl: 2d12h
ip_latency_interval: 1d
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
	if time.Duration(c.ProbeInterval) != 48*time.Hour {
		t.Fatalf("probe_interval 2d 应为 48h，得到 %v", c.ProbeInterval)
	}
	if c.CacheTTL == nil || time.Duration(*c.CacheTTL) != 60*time.Hour {
		t.Fatalf("cache_ttl 2d12h 应为 60h，得到 %v", c.CacheTTL)
	}
	if time.Duration(c.IPLatencyInterval) != 24*time.Hour {
		t.Fatalf("ip_latency_interval 1d 应为 24h，得到 %v", c.IPLatencyInterval)
	}
}

func TestLoadConfigBadDayDuration(t *testing.T) {
	y := `
listeners:
  doh:
    enabled: true
cert:
  mode: "acme"
  cert_path: "/tmp/a.pem"
  key_path: "/tmp/b.pem"
probe_interval: 1x
dns:
  cf:
    DNS-over-HTTPS: "https://example.com/dns-query"
`
	if err := writeTemp(y); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig("/tmp/ecs_test_config.yaml"); err == nil {
		t.Fatalf("非法时长应报错")
	}
}

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

func TestLoadConfigProviderIPPriority(t *testing.T) {
	y := `
listeners:
  doh:
    enabled: true
cert:
  mode: "acme"
  cert_path: "/tmp/a.pem"
  key_path: "/tmp/b.pem"
ip_priority: ipv4
provider_ip_priority:
  cf: latency
  adguard: ipv6
dns:
  cf:
    DNS-over-HTTPS: "https://example.com/dns-query"
  adguard:
    DNS-over-TLS: "tls://dns.adguard.com:853"
  plain:
    Plain DNS: "udp://8.8.8.8:53"
`
	if err := writeTemp(y); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig("/tmp/ecs_test_config.yaml")
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if c.IPPriorityFor("cf") != "latency" {
		t.Fatalf("cf 优先级应为 latency，得到 %q", c.IPPriorityFor("cf"))
	}
	if c.IPPriorityFor("adguard") != "ipv6" {
		t.Fatalf("adguard 优先级应为 ipv6，得到 %q", c.IPPriorityFor("adguard"))
	}
	if c.IPPriorityFor("plain") != "ipv4" {
		t.Fatalf("未覆盖的 plain 应回退全局 ipv4，得到 %q", c.IPPriorityFor("plain"))
	}
}

func TestLoadConfigBadProviderIPPriority(t *testing.T) {
	y := `
listeners:
  doh:
    enabled: true
cert:
  mode: "acme"
  cert_path: "/tmp/a.pem"
  key_path: "/tmp/b.pem"
provider_ip_priority:
  cf: foo
dns:
  cf:
    DNS-over-HTTPS: "https://example.com/dns-query"
`
	if err := writeTemp(y); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig("/tmp/ecs_test_config.yaml"); err == nil {
		t.Fatalf("非法 provider_ip_priority 值应报错")
	}
}

func TestIPPriorityFor(t *testing.T) {
	c := Config{
		IPPriority:         "ipv4",
		ProviderIPPriority: map[string]string{"cf": "latency"},
	}
	if c.IPPriorityFor("cf") != "latency" {
		t.Fatalf("命中的服务商应返回覆盖值")
	}
	if c.IPPriorityFor("unknown") != "ipv4" {
		t.Fatalf("未命中的服务商应回退全局")
	}
}

func TestUpstreamTargetsFor(t *testing.T) {
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

	got := make(map[string]uint16)
	for _, t := range c.UpstreamTargetsFor("p1") {
		got[t.Host] = t.Port
	}
	if got["doh.example.com"] != 443 || got["dot.example.com"] != 853 {
		t.Fatalf("p1 目标错误: %+v", got)
	}
	if len(got) != 2 {
		t.Fatalf("p1 应只有 2 个目标，得到 %d", len(got))
	}

	// p2 的端点全为 IP 字面量，不应有探测目标。
	if ts := c.UpstreamTargetsFor("p2"); len(ts) != 0 {
		t.Fatalf("纯 IP 服务商不应有探测目标，得到 %+v", ts)
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
