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
