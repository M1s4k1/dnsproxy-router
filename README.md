# dnsproxy-scheduler

一个对外只提供 DoH 的 DNS 代理，内部对多家上游服务商做「动态择优 + 并发赛马」。

基于 [AdguardTeam/dnsproxy](https://github.com/AdguardTeam/dnsproxy) 库构建，只额外实现调度层与 ECS 策略，DNS 编解码、DoH 前端、缓存、去重均由成熟库承担。

## 特性

- **动态择优**：周期性探测每家上游各模式的延迟，自动为每家选出延迟最低的模式。
- **并发赛马**：每次查询向各家「当前最优线路」并发发出，谁先成功返回就用谁。
- **连接与缓存复用**：选路未变化时跨周期复用热连接池与缓存，无冷启动。
- **ECS 策略**：支持 `off`（移除）/ `pass`（透传）/ `override`（覆写）三种 EDNS Client Subnet 处理。
- **可自定义 DoH 路径**：DoH 端点路径可任意定制，支持多层子路径。
- **多协议上游**：上游支持 DNS-over-HTTPS / DNS-over-TLS / DNS-over-QUIC。

## 目录结构

```text
dnsproxy/
├── cmd/dnsproxy/main.go        # 程序入口（组装各包、启动服务）
├── internal/
│   ├── config/                 # YAML 配置的加载与校验
│   ├── ecs/                    # EDNS Client Subnet 策略
│   ├── scheduler/              # 周期性探测 + 选路 + 连接复用
│   └── handler/                # 把选路注入每个 DoH 请求（赛马）
├── config.example.yaml         # 配置示例（复制为 config.yaml 后使用）
├── interactive-setup.sh        # 交互式部署脚本（服务器上运行）
└── go.mod / go.sum
```

## 工作原理

```text
                 ┌─────────────────────────────────────────────┐
                 │               周期性探路                     │
                 │  对每家 × 每种模式 发 A 记录查询测 RTT        │
                 │  每家选出延迟最低的模式                       │
                 │  （例如 provider-a→DoH, provider-b→DoQ）      │
                 └─────────────────────────────────────────────┘
                                    │ 更新选路
                                    ▼
  客户端 ──DoH──▶ {port}/{doh_path} ──▶ 多家并发赛马 ──▶ 最快者返回
```

- **外层（scheduler）**：每 `probe_interval`（默认 15m）探测，每家各模式探测 `probe_count` 次取中位数 RTT，选出每家延迟最低的模式。
- **内层（赛马）**：每次收到 DoH 查询，向各家「当前最优线路」并发查询，谁先成功返回就用谁（`proxy.UpstreamModeParallel`）。

## 快速开始

### ① 本地编译出 Linux 二进制

在**本项目根目录**（`dnsproxy/`）执行，按目标服务器架构选择：

```bash
GOOS=linux GOARCH=amd64 go build -o dnsproxy-scheduler ./cmd/dnsproxy  # x86_64
GOOS=linux GOARCH=arm64 go build -o dnsproxy-scheduler ./cmd/dnsproxy  # arm64
```

> 需要 Go 1.26+。本地没装那么新也没关系——`GOTOOLCHAIN=auto`（默认）会自动下载对应工具链，首次构建需联网。

### ② 上传二进制和脚本到服务器

```bash
scp dnsproxy-scheduler interactive-setup.sh root@SERVER_IP:/tmp/
```

### ③ 在服务器上跑交互脚本（完成部署）

```bash
ssh root@SERVER_IP
bash /tmp/interactive-setup.sh /tmp/dnsproxy-scheduler
```

脚本会一步步引导你：**域名/端口 → DoH 路径 → ECS 策略 → 证书方式 → 探测参数 → 上游 DNS**，然后自动完成：生成 `config.yaml` → 安装二进制到 `/usr/local/bin/` → 申请证书（acme.sh + DNS-01）→ 写 systemd 单元 → 启动服务。

> 脚本**不做编译**，只做「填配置 + 部署」。所以必须先完成第 ① 步拿到二进制。

### 验证

```bash
curl -H 'accept: application/dns-message' \
  'https://你的域名/dns-query?dns=AAABAAABAAAAAAAAB2V4YW1wbGUDY29tAAABAAE' | xxd | head
```

返回二进制 DNS 报文即为正常。`dns-query` 换成你配置的 `doh_path` 即可；该路径支持多层子路径（如 `/dns/query/v1`）。若配置了非 443 端口，URL 需补上 `:端口`。

## 配置

配置文件为 YAML（见 [config.example.yaml](config.example.yaml)），复制为 `config.yaml` 后编辑。关键字段：

| 字段 | 说明 | 默认 |
| --- | --- | --- |
| `use_domain` | 是否使用域名（证书申请） | `true` |
| `domain` | 服务域名 | `example.com` |
| `port` | 对外 DoH 监听端口（DoH 标准 443） | `443` |
| `doh_path` | DoH 端点路径（可自定义，支持多层） | `/dns-query` |
| `ecs.mode` | ECS 策略：`off`（移除）\| `pass`（透传）\| `override`（覆写） | `off` |
| `ecs.address` | `override` 模式的固定前缀（如 `223.5.5.0/24`） | — |
| `cert.mode` | `acme`（申请+续期）\| `existing`（已有证书） | `acme` |
| `cert.renew` | acme 模式是否自动续期 | `true` |
| `cert.cert_path` / `cert.key_path` | 证书/私钥路径 | — |
| `cert.email` | acme 注册邮箱 | — |
| `cert.provider` | DNS API 提供商（acme DNS-01） | `cloudflare` |
| `probe_interval` | 探测周期 | `15m` |
| `probe_timeout` | 单次探测超时 | `3s` |
| `probe_count` | 每模式探测次数（取中位数） | `3` |
| `probe_domain` | 探测用域名 | `example.com.` |
| `cache_enabled` | 是否开启响应缓存 | `true` |
| `cache_size_bytes` | 响应缓存大小（仅开启时生效） | `67108864` |
| `bootstrap` | 引导 DNS（明文） | `1.1.1.1:53` |
| `bootstrap_cache_ttl` | 引导解析结果缓存时长（0 关闭） | `5s` |
| `dns` | 上游服务商 map（见下） | 无（必填） |

### `dns` 字段：服务商 → 模式 → 地址

项目**不内置任何上游端点**，请自行配置。示例（把地址替换成你自己的）：

```yaml
dns:
  provider-a:               # 服务商名（任意）
    DNS-over-HTTPS: "https://dns.example.com/dns-query"
    DNS-over-TLS:   "tls://dns.example.com:853"
    DNS-over-QUIC:  "quic://dns.example.com:853"
  provider-b:
    DNS-over-HTTPS: "https://dns.example.net/dns-query"
```

- 服务商数量、每家支持的模式种类均任意。
- 模式键固定为 `DNS-over-HTTPS` / `DNS-over-TLS` / `DNS-over-QUIC`。
- 地址格式遵循 dnsproxy 语法：`https://host/dns-query`、`tls://host:853`、`quic://host:853`。

### `ecs` 字段：EDNS Client Subnet 策略

控制发往上游的请求是否携带 EDNS Client Subnet（ECS，RFC 7871）：

| 模式 | 行为 |
| --- | --- |
| `off` | 不向上游传任何 EDNS 信息；客户端请求带 ECS 也会被移除 |
| `pass` | 透传客户端请求中的 ECS 原样（依赖库的 ECS 透传 + subnet 缓存） |
| `override` | 移除客户端 ECS，始终携带配置的固定前缀 `ecs.address` |

```yaml
ecs:
  mode: "override"
  address: "223.5.5.0/24"
```

- `off` / `override` 模式下 ECS 是确定性的，缓存对所有客户端一致，安全。
- `pass` 模式下 ECS 随客户端变化，会自动启用 subnet 缓存，避免不同地域客户端串缓存。

## 构建

```bash
go build -o dnsproxy-scheduler ./cmd/dnsproxy                          # 本机架构
GOOS=linux GOARCH=amd64 go build -o dnsproxy-scheduler ./cmd/dnsproxy  # x86_64
GOOS=linux GOARCH=arm64 go build -o dnsproxy-scheduler ./cmd/dnsproxy  # arm64
```

其他常用命令：

```bash
go test ./...   # 运行测试
go vet ./...    # 静态检查
gofmt -l ./cmd ./internal   # 检查格式
```

## 部署

### 方式一：交互式脚本（推荐）

见上文「快速开始」。脚本本身不编译，只负责在服务器上装二进制、生成配置、申请证书、写 systemd 并启动。

### 方式二：手动

- 本地先 `cp config.example.yaml config.yaml` 并填入真实值，再把二进制 + `config.yaml` 传到服务器，证书放到 `cert.cert_path` 指定位置。
- 用 `cert.mode: existing` 时，证书需提前备好；systemd 运行：

```ini
[Service]
ExecStart=/usr/local/bin/dnsproxy-scheduler -config /etc/dnsproxy/config.yaml
Restart=always
```

- 在服务器防火墙 / 云安全组放行 `${port}/TCP`（默认 443）。

### 运维命令

```bash
systemctl status dnsproxy-scheduler      # 查看状态
journalctl -u dnsproxy-scheduler -f      # 实时日志
systemctl restart dnsproxy-scheduler     # 重启（改配置后）
```

## 说明

- 每轮探测后，若各家最优线路未变化，则**复用**既有连接（热 TLS/QUIC/TCP 池）与缓存，无冷启动；仅当某家线路切换时才对退役线路重建并延迟关闭旧连接。
- acme 模式下证书由 acme.sh 自动续期，续期后通过 `--reloadcmd` 重启服务加载新证书。
- 默认监听 443（DoH 标准端口），属于特权端口（< 1024），需 root 运行；若改用非特权端口可非 root 运行。
