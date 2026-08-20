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
port: 443
doh_path: "/dns/query/v1"
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
	if c.DoHPath != "/dns/query/v1" {
		t.Fatalf("DoHPath 解析错误: %q", c.DoHPath)
	}
}

func TestLoadConfigECSOffDefault(t *testing.T) {
	y := `
port: 443
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
}
