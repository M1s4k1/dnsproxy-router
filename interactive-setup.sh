#!/usr/bin/env bash
set -euo pipefail

# ============================================================================
# dnsproxy-router 交互式配置 + 部署脚本
# 通过交互引导用户填写配置，生成 config.yaml，申请证书，部署并启动服务。
#
# 用法（root 下执行，Linux + systemd）:
#   bash interactive-setup.sh               # 自动获取二进制（下载 release / 编译）
#   bash interactive-setup.sh /path/to/bin  # 使用本地二进制（跳过下载/编译）
#
# 自动获取二进制的优先级：
#   1) 有匹配架构的 GitHub Release 则直接下载
#   2) 无 release 则本地编译（自动 git clone / 下载源码 tarball）
#   3) 未安装 Go 则先安装官方 Go 工具链再编译
# ============================================================================

# 二进制来源：显式传本地路径则直接用；否则自动获取（下载 release / 编译）。
BIN_ARG="${1:-}"
INSTALL_DIR="/usr/local/bin"
CONF_DIR="/etc/dnsproxy"
CONF_FILE="${CONF_DIR}/config.yaml"
CERT_DIR="${CONF_DIR}/certs"
SERVICE="dnsproxy-router"
BIN_NAME="dnsproxy-router"

# 下载 release 用的 GitHub 仓库（fork 本项目后改成你自己的）。
REPO="M1s4k1/dnsproxy-router"
# 下载的 release 版本：latest 表示最新 release；也可指定如 v1.0.0。
VERSION="${VERSION:-latest}"
# 未安装 Go 时安装的版本（应与 go.mod 的 go 指令一致）。
GO_VERSION="1.26.6"

# 颜色（仅在交互终端启用）
if [ -t 1 ]; then
  C_RESET=$'\033[0m'; C_BOLD=$'\033[1m'
  C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'; C_CYAN=$'\033[36m'
else
  C_RESET=""; C_BOLD=""; C_GREEN=""; C_YELLOW=""; C_CYAN=""
fi

say()  { printf '%s\n' "$*"; }
info() { printf '%s%s%s\n' "$C_CYAN" "$*" "$C_RESET"; }
ok()   { printf '%s%s%s\n' "$C_GREEN" "$*" "$C_RESET"; }
warn() { printf '%s%s%s\n' "$C_YELLOW" "$*" "$C_RESET"; }

# ask <提示> <默认值> -> 打印用户输入或默认值
ask() {
  local prompt="$1" default="$2" input
  read -rp "$(printf '%s [%s]: ' "$prompt" "$default")" input
  printf '%s' "${input:-$default}"
}

# ask_yn <提示> [默认 y/n] -> 返回 0(yes)/1(no)，非法输入重问
# 提示固定显示 [y/n]，输入 y/Y 或 n/N（含 yes/no）均可识别。
ask_yn() {
  local prompt="$1" default="${2:-n}" input
  while :; do
    read -rp "$(printf '%s [y/n]: ' "$prompt")" input
    input="${input:-$default}"
    case "$input" in
      [yY]|[yY][eE][sS]) return 0 ;;
      [nN]|[nN][oO])     return 1 ;;
      *) warn_err "  请输入 y 或 n" ;;
    esac
  done
}

# warn_err：同 warn，但输出到 stderr（用于命令替换函数内部的报错，避免污染返回值）
warn_err() { printf '%s%s%s\n' "$C_YELLOW" "$*" "$C_RESET" >&2; }

# ask_int <提示> <默认值> [最小值] [最大值] -> 校验整数，非法重问，stdout 输出合法值
ask_int() {
  local prompt="$1" default="$2" min="${3:-0}" max="${4:-0}" input
  while :; do
    read -rp "$(printf '%s [%s]: ' "$prompt" "$default")" input
    input="${input:-$default}"
    if [[ "$input" =~ ^[0-9]+$ ]] && [ "$input" -ge "$min" ] && { [ "$max" -le 0 ] || [ "$input" -le "$max" ]; }; then
      printf '%s' "$input"
      return 0
    fi
    if [ "$max" -gt 0 ]; then
      warn_err "  非法输入「${input}」：需为 ${min}~${max} 的整数"
    else
      warn_err "  非法输入「${input}」：需为 ≥${min} 的整数"
    fi
  done
}

# ask_duration <提示> <默认值> -> 校验 Go time.Duration（数字+单位），非法重问
ask_duration() {
  local prompt="$1" default="$2" input
  while :; do
    read -rp "$(printf '%s [%s]: ' "$prompt" "$default")" input
    input="${input:-$default}"
    if [[ "$input" =~ ^[0-9]+(ns|us|ms|s|m|h)$ ]]; then
      printf '%s' "$input"
      return 0
    fi
    warn_err "  非法时长「${input}」：需为 数字+单位（ns/us/ms/s/m/h），如 15m、3s、1h"
  done
}

# normalize_upstream_addr <模式> <地址> -> stdout 输出规范化后的上游地址
# 规则：无 scheme 按模式补（DoH→https:// DoT→tls:// DoQ→quic:// Plain→udp://）；
#       无端口按 scheme 补默认（DoH=443 DoT=853 DoQ=853 Plain=53）；
#       DoH 无路径补 /dns-query。已有前缀/端口/路径则原样保留。
normalize_upstream_addr() {
  local mode="$1" addr="$2"
  local scheme="" default_port=""
  case "$mode" in
    DNS-over-HTTPS) scheme="https"; default_port=443 ;;
    DNS-over-TLS)   scheme="tls";   default_port=853 ;;
    DNS-over-QUIC)  scheme="quic";  default_port=853 ;;
    "Plain DNS")    scheme="udp";   default_port=53  ;;
  esac

  local rest="$addr" host="" path=""
  if [[ "$addr" == *"://"* ]]; then
    scheme="${addr%%://*}"
    rest="${addr#*://}"
    case "$scheme" in
      https|h3)      default_port=443 ;;
      tls|quic)      default_port=853 ;;
      udp|tcp)       default_port=53 ;;
      *)             default_port="" ;;
    esac
  fi

  # 分离 host 与 path（第一个 / 之后为 path）
  if [[ "$rest" == *"/"* ]]; then
    host="${rest%%/*}"
    path="/${rest#*/}"
  else
    host="$rest"
    path=""
  fi

  # 裸 IPv6（多个冒号、无方括号）→ 加方括号
  if [[ "$host" != \[* ]] && [[ "$host" == *:*:* ]]; then
    host="[${host}]"
  fi

  # host 是否已含端口
  local has_port=false
  case "$host" in
    *"]:"*) has_port=true ;;   # [::1]:853
    \[*)     ;;                # [::1]（IPv6 无端口）
    *:*)     has_port=true ;;  # host:853
  esac

  # 补端口
  if [ "$has_port" = false ] && [ -n "$default_port" ]; then
    host="${host}:${default_port}"
  fi

  # DoH 无路径补 /dns-query
  if [ "$scheme" = "https" ] && [ -z "$path" ]; then
    path="/dns-query"
  fi

  printf '%s' "${scheme}://${host}${path}"
}

# ============================================================================
# DNS 提供商（acme.sh DNS-01）
# 脚本只负责「选 provider + 收集凭证 + 调 acme.sh」，签发/续期逻辑全部复用
# acme.sh 内置的 dnsapi（dns_<provider>.sh），因此支持 acme.sh 的全部提供商。
# 下面仅预置常用站到菜单；任何其它 provider 可用「手动输入」覆盖。
# ============================================================================

# provider_vars <provider> -> 该 provider 需要的凭证变量（空格分隔）
provider_vars() {
  case "$1" in
    cloudflare)  printf 'CF_Token' ;;
    ali)         printf 'Ali_Key Ali_Secret' ;;
    dp)          printf 'DP_Id DP_Key' ;;
    dpi)         printf 'DPI_Id DPI_Key' ;;
    tencent)     printf 'Tencent_SecretId Tencent_SecretKey' ;;
    huaweicloud) printf 'HUAWEICLOUD_Username HUAWEICLOUD_Password HUAWEICLOUD_DomainName' ;;
    gd)          printf 'GD_Key GD_Secret' ;;
    namecheap)   printf 'NAMECHEAP_USERNAME NAMECHEAP_API_KEY' ;;
    *)           printf '' ;;
  esac
}

# var_label <变量名> -> 可读提示
var_label() {
  case "$1" in
    CF_Token)               printf 'Cloudflare API Token' ;;
    Ali_Key)                printf '阿里云 AccessKey ID' ;;
    Ali_Secret)             printf '阿里云 AccessKey Secret' ;;
    DP_Id)                  printf 'DNSPod.cn ID' ;;
    DP_Key)                 printf 'DNSPod.cn Token' ;;
    DPI_Id)                 printf 'DNSPod.com ID' ;;
    DPI_Key)                printf 'DNSPod.com Token' ;;
    Tencent_SecretId)       printf '腾讯云 SecretId' ;;
    Tencent_SecretKey)      printf '腾讯云 SecretKey' ;;
    HUAWEICLOUD_Username)   printf '华为云 IAM 用户名' ;;
    HUAWEICLOUD_Password)   printf '华为云 IAM 密码' ;;
    HUAWEICLOUD_DomainName) printf '华为云 DNS 域名' ;;
    GD_Key)                 printf 'GoDaddy API Key' ;;
    GD_Secret)              printf 'GoDaddy API Secret' ;;
    NAMECHEAP_USERNAME)     printf 'Namecheap 用户名' ;;
    NAMECHEAP_API_KEY)      printf 'Namecheap API Key' ;;
    *)                      printf '%s' "$1" ;;
  esac
}

# collect_credentials <provider> -> 逐个读取并 export 该 provider 的凭证变量
collect_credentials() {
  local p="$1" vars var val
  vars="$(provider_vars "$p")"
  if [ -z "$vars" ]; then
    say "  该 provider 无预置凭证变量，请确保已自行 export 对应环境变量。"
    return 0
  fi
  for var in $vars; do
    if [ -n "${!var:-}" ]; then
      say "  检测到环境变量 ${var}，将使用它。"
      continue
    fi
    read -rsp "  $(var_label "$var")（${var}，输入不回显）: " val
    say ""
    if [ -n "$val" ]; then
      export "${var}=${val}"
    else
      warn "  ${var} 未提供，稍后签发可能失败。"
    fi
  done
}

# --- 0. 前置检查：root + 系统类型 + CPU 架构 ---
if [ "$(id -u)" -ne 0 ]; then
  warn "请用 root 执行（或 sudo）。"
  exit 1
fi

# 系统类型：本脚本面向 Linux + systemd 部署。
if [ "$(uname -s)" != "Linux" ]; then
  warn "本脚本仅支持 Linux（依赖 systemd 部署服务）。当前系统: $(uname -s)"
  exit 1
fi
if ! command -v systemctl >/dev/null 2>&1; then
  warn "未检测到 systemctl，无法部署 systemd 服务。请在本机 systemd 环境运行。"
  exit 1
fi

# detect_arch -> 输出产物标签（linux-amd64 / linux-arm64 / linux-armv7）。
detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)       printf 'linux-amd64' ;;
    aarch64|arm64)      printf 'linux-arm64' ;;
    armv7l|armv7|armhf) printf 'linux-armv7' ;;
    *)                  printf '' ;;
  esac
}
ARCH_TAG="$(detect_arch)"
if [ -z "$ARCH_TAG" ]; then
  warn "不支持的 CPU 架构: $(uname -m)。目前仅支持 amd64 / arm64 / armv7。"
  exit 1
fi

# ----------------------------------------------------------------------------
# 获取二进制：优先级 = 显式传入 > 下载 release > 编译（无 Go 则先装 Go）。
# 结果写入 BIN_SRC（本地文件路径），BIN_TMP=1 表示临时产物（安装后清理）。
# ----------------------------------------------------------------------------

# goarch_of <arch_tag> -> 输出 "GOARCH [GOARM]"（用于编译分支）
goarch_of() {
  case "$1" in
    linux-amd64) printf 'amd64' ;;
    linux-arm64) printf 'arm64' ;;
    linux-armv7) printf 'arm 7' ;;
  esac
}

# go_dist_arch <arch_tag> -> 输出 Go 官方 tarball 的架构名（装 Go 用）
go_dist_arch() {
  case "$1" in
    linux-amd64) printf 'amd64' ;;
    linux-arm64) printf 'arm64' ;;
    linux-armv7) printf 'armv6l' ;;
  esac
}

# ensure_go 确保 go 可用：已装则返回；否则下载官方 tarball 装到 /usr/local/go。
ensure_go() {
  if command -v go >/dev/null 2>&1; then
    return 0
  fi
  info "未检测到 Go，正在安装 Go ${GO_VERSION} ..."
  local dist_arch tarball
  dist_arch="$(go_dist_arch "$ARCH_TAG")"
  tarball="go${GO_VERSION}.linux-${dist_arch}.tar.gz"
  # 已存在 /usr/local/go/bin/go 则复用，否则下载解包。
  if [ ! -x /usr/local/go/bin/go ]; then
    curl -fL --retry 3 "https://go.dev/dl/${tarball}" -o "/tmp/${tarball}"
    tar -C /usr/local -xzf "/tmp/${tarball}"
    rm -f "/tmp/${tarball}"
  fi
  export PATH="/usr/local/go/bin:${PATH}"
  command -v go >/dev/null 2>&1 || { warn "Go 安装失败，请手动安装后重试。"; exit 1; }
}

# build_from_source <arch_tag> <out> 拉取源码并交叉编译出二进制。
build_from_source() {
  local out="$2" goarch goarm workdir
  read -r goarch goarm <<< "$(goarch_of "$1")"
  workdir="$(mktemp -d)"

  info "拉取源码并编译（GOOS=linux GOARCH=${goarch}${goarm:+ / GOARM=${goarm}}）..."
  # 优先 git 浅克隆；git 缺失或克隆失败时回退到下载源码 tarball。
  if command -v git >/dev/null 2>&1 && \
     git clone --depth 1 "https://github.com/${REPO}.git" "$workdir/src" 2>/dev/null; then
    :
  else
    mkdir -p "$workdir/src"
    curl -fL "https://github.com/${REPO}/archive/refs/heads/main.tar.gz" \
      | tar -xz -C "$workdir/src" --strip-components=1
  fi
  (
    cd "$workdir/src" || exit 1
    export GOOS=linux GOARCH="$goarch" CGO_ENABLED=0
    if [ -n "$goarm" ]; then export GOARM="$goarm"; fi
    go build -trimpath -o "$out" ./cmd/dnsproxy
  )
  rm -rf "$workdir"
}

# obtain_binary 确定 BIN_SRC：显式传入 > 下载 release > 编译。
obtain_binary() {
  local url
  if [ -n "$BIN_ARG" ]; then
    if [ ! -f "$BIN_ARG" ]; then
      warn "指定的二进制不存在: $BIN_ARG"
      exit 1
    fi
    BIN_SRC="$BIN_ARG"
    BIN_TMP=0
    ok "使用本地二进制: $BIN_SRC"
    return 0
  fi

  if ! command -v curl >/dev/null 2>&1; then
    warn "自动获取二进制需要 curl，请先安装（如 apt install curl）。"
    exit 1
  fi

  BIN_SRC="$(mktemp "/tmp/${BIN_NAME}.XXXXXX")"
  BIN_TMP=1
  url="https://github.com/${REPO}/releases/${VERSION}/download/${BIN_NAME}-${ARCH_TAG}"

  info "检测到架构: ${ARCH_TAG}"
  say "  尝试下载预编译 release ..."
  if curl -fL --retry 3 -o "$BIN_SRC" "$url"; then
    chmod +x "$BIN_SRC"
    ok "已下载 release 二进制（${VERSION}）。"
    return 0
  fi
  warn "未找到匹配的 release（${url}），回退到源码编译。"

  ensure_go
  build_from_source "$ARCH_TAG" "$BIN_SRC"
  ok "源码编译完成。"
}

say ""
say "=========================================================="
say "  dnsproxy-router 交互式配置向导"
say "=========================================================="
say ""

# --- 1. 入站监听 ---
say ""
info "【1/6】入站监听（对外提供的 DNS 协议）"
say "  至少开启一种。加密协议（DoH/DoT/DoQ）需要 TLS 证书。"

if ask_yn "开启 DoH（DNS-over-HTTPS）" "y"; then
  DOH_ENABLED="true"
  DOH_PORT="$(ask_int "  DoH 监听端口" "443" 1 65535)"
else
  DOH_ENABLED="false"
  DOH_PORT="443"
fi

if ask_yn "开启 DoT（DNS-over-TLS）" "n"; then
  DOT_ENABLED="true"
  DOT_PORT="$(ask_int "  DoT 监听端口" "853" 1 65535)"
else
  DOT_ENABLED="false"
  DOT_PORT="853"
fi

if ask_yn "开启 DoQ（DNS-over-QUIC）" "n"; then
  DOQ_ENABLED="true"
  DOQ_PORT="$(ask_int "  DoQ 监听端口" "853" 1 65535)"
else
  DOQ_ENABLED="false"
  DOQ_PORT="853"
fi

if ask_yn "开启明文 DNS（UDP+TCP 同端口）" "n"; then
  PLAIN_ENABLED="true"
  PLAIN_PORT="$(ask_int "  明文 DNS 监听端口" "53" 1 65535)"
else
  PLAIN_ENABLED="false"
  PLAIN_PORT="53"
fi

if [ "$DOH_ENABLED" = "false" ] && [ "$DOT_ENABLED" = "false" ] && \
   [ "$DOQ_ENABLED" = "false" ] && [ "$PLAIN_ENABLED" = "false" ]; then
  warn "至少需开启一种入站监听，已默认开启 DoH。"
  DOH_ENABLED="true"
  DOH_PORT="443"
fi

DOH_PATH="/dns-query"
if [ "$DOH_ENABLED" = "true" ]; then
  DOH_PATH="$(ask "DoH 端点路径（可自定义，支持多层，如 /dns/query/v1）" "/dns-query")"
  # 校验路径：以 / 开头且不以 / 结尾（根路径 / 除外）
  if [[ "$DOH_PATH" != /* ]]; then
    warn "路径必须以 / 开头，已改为默认 /dns-query"
    DOH_PATH="/dns-query"
  elif [ "$DOH_PATH" != "/" ] && [[ "$DOH_PATH" == */ ]]; then
    warn "路径不能以 / 结尾，已去掉末尾 /"
    DOH_PATH="${DOH_PATH%/}"
  fi
fi

# 是否任一加密协议开启（决定是否需要 TLS 证书）。
if [ "$DOH_ENABLED" = "true" ] || [ "$DOT_ENABLED" = "true" ] || [ "$DOQ_ENABLED" = "true" ]; then
  NEEDS_TLS="true"
else
  NEEDS_TLS="false"
fi

# ECS（EDNS Client Subnet）策略
say ""
info "EDNS Client Subnet（ECS）策略"
say "  0) 关闭：不向上游传任何 EDNS 信息（请求带 ECS 也会被移除）"
say "  1) 透传：保留客户端请求中的 ECS 原样转发"
say "  2) 覆写：始终用下方配置的固定 ECS 前缀转发"
ECS_CHOICE="$(ask "ECS 模式（0/1/2）" "0")"

case "${ECS_CHOICE:-0}" in
  1)
    ECS_MODE="pass"
    ECS_ADDRESS=""
    ;;
  2)
    ECS_MODE="override"
    ECS_ADDRESS="$(ask "固定 ECS 前缀（如 223.5.5.0/24）" "223.5.5.0/24")"
    # 简单校验：IPv4 前缀或 IPv6 前缀格式
    if ! [[ "$ECS_ADDRESS" =~ ^[0-9.]+/[0-9]+$ ]] && ! [[ "$ECS_ADDRESS" =~ ^[0-9a-fA-F:]+/[0-9]+$ ]]; then
      warn "ECS 前缀格式非法，已改为默认 223.5.5.0/24"
      ECS_ADDRESS="223.5.5.0/24"
    fi
    ;;
  *)
    ECS_MODE="off"
    ECS_ADDRESS=""
    ;;
esac

# --- 2. 域名与证书配置 ---
say ""
info "【2/6】域名与证书配置"

if [ "$NEEDS_TLS" = "true" ]; then
  if ask_yn "是否使用域名（用于证书申请）" "y"; then
    USE_DOMAIN="true"
    DOMAIN="$(ask "域名" "example.com")"
  else
    USE_DOMAIN="false"
    DOMAIN=""
  fi

  say "  加密协议（DoH/DoT/DoQ）需要 TLS 证书。"
  say "  1) acme.sh 申请并自动续期（推荐，DNS-01 无需开放 80）"
  say "  2) 使用已有证书（指定文件路径）"
  CERT_CHOICE="$(ask "证书方式" "1")"

  if [ "$CERT_CHOICE" = "2" ]; then
    CERT_MODE="existing"
    CERT_RENEW="false"
    CERT_PATH="$(ask "证书（fullchain）路径" "/etc/dnsproxy/certs/fullchain.pem")"
    KEY_PATH="$(ask "私钥路径" "/etc/dnsproxy/certs/privkey.pem")"
    CERT_EMAIL=""
    CERT_PROVIDER=""
  else
    CERT_MODE="acme"
    CERT_RENEW="true"
    CERT_PATH="${CERT_DIR}/fullchain.pem"
    KEY_PATH="${CERT_DIR}/privkey.pem"
    CERT_EMAIL="$(ask "注册邮箱（证书到期提醒）" "you@example.com")"

    say "  DNS API 提供商（用于 DNS-01 验证）："
    say "    1) cloudflare    2) 阿里云 ali      3) DNSPod.cn dp"
    say "    4) DNSPod.com dpi  5) 腾讯云 tencent  6) 华为云 huaweicloud"
    say "    7) GoDaddy gd    8) Namecheap      9) 手动输入 provider 名"
    CERT_PROVIDER="$(ask "  选择提供商" "1")"
    case "${CERT_PROVIDER:-1}" in
      1) CERT_PROVIDER="cloudflare" ;;
      2) CERT_PROVIDER="ali" ;;
      3) CERT_PROVIDER="dp" ;;
      4) CERT_PROVIDER="dpi" ;;
      5) CERT_PROVIDER="tencent" ;;
      6) CERT_PROVIDER="huaweicloud" ;;
      7) CERT_PROVIDER="gd" ;;
      8) CERT_PROVIDER="namecheap" ;;
      9) CERT_PROVIDER="$(ask "    手动输入 acme.sh provider 名（如 dns_xxx 中的 xxx）" "")" ;;
      *) CERT_PROVIDER="cloudflare" ;;
    esac

    # 按所选 provider 收集凭证（复用 acme.sh 约定的环境变量名）。
    collect_credentials "$CERT_PROVIDER"
  fi
else
  # 仅明文 DNS，无需域名与证书。
  USE_DOMAIN="false"
  DOMAIN=""
  CERT_MODE="existing"
  CERT_RENEW="false"
  CERT_PATH=""
  KEY_PATH=""
  CERT_EMAIL=""
  CERT_PROVIDER=""
  say "  仅开启明文 DNS，无需 TLS 证书，跳过域名与证书配置。"
fi

# --- 3. 探测参数 ---
say ""
info "【3/6】探测参数（每周期对各家各模式测延迟，选每家最优）"
PROBE_INTERVAL="$(ask_duration "探测周期（如 15m/300s）" "15m")"
PROBE_TIMEOUT="$(ask_duration "单次探测超时（如 3s）" "3s")"
PROBE_COUNT="$(ask_int "每模式探测次数（取中位数）" "3" 1)"
PROBE_DOMAIN="$(ask "探测用域名" "example.com.")"

# --- 4. 缓存与引导 ---
say ""
info "【4/6】缓存与引导 DNS"

# 响应缓存：先问是否开启，开启才问大小/过期时间/逐出策略。
if ask_yn "开启 DNS 响应缓存（若客户端已做本地缓存，可关闭）" "y"; then
  CACHE_ENABLED="true"
  CACHE_SIZE="$(ask_int "响应缓存大小（字节）" "67108864" 1)"
  CACHE_TTL="$(ask_duration "缓存固定过期时间（如 30m/1h；0s 表示跟随记录自身 TTL）" "30m")"
  say "  缓存逐出策略："
  say "    1) FIFO 先进先出（最早插入的先逐出）"
  say "    2) LRU  最近最少使用（最久未访问的先逐出，推荐）"
  say "    3) LFU  最不经常使用（访问次数最少的先逐出）"
  case "$(ask "  选择逐出策略（1/2/3）" "2")" in
    1) CACHE_EVICTION="fifo" ;;
    3) CACHE_EVICTION="lfu" ;;
    *) CACHE_EVICTION="lru" ;;
  esac
else
  CACHE_ENABLED="false"
  CACHE_SIZE="0"
  CACHE_TTL="0s"
  CACHE_EVICTION="lru"
fi

BOOTSTRAP_INPUT="$(ask "引导 DNS（逗号分隔，用于解析上游域名）" "1.1.1.1:53,8.8.8.8:53")"

# 引导解析缓存：先问是否开启，开启才问缓存时长。
if ask_yn "开启引导解析缓存（缓存上游域名解析结果）" "y"; then
  BOOTSTRAP_CACHE_TTL="$(ask_duration "引导解析缓存时长（如 5s/1m）" "5s")"
else
  BOOTSTRAP_CACHE_TTL="0s"
fi

BOOTSTRAP_LIST=""
IFS=',' read -ra BOOTSTRAP_ARR <<< "$BOOTSTRAP_INPUT"
for ip in "${BOOTSTRAP_ARR[@]}"; do
  ip="$(printf '%s' "$ip" | xargs)"   # 去空格
  [ -z "$ip" ] && continue
  BOOTSTRAP_LIST+="  - \"${ip}\""$'\n'
done

# 域名 → IP 静态映射：命中即直接用给定 IP，不再走引导 DNS。
say ""
say "  上游域名 → IP 静态映射（可选）：对指定域名固定其解析 IP，跳过引导 DNS。"
say "  例如上游主机名 dns.example.com 固定解析到 1.2.3.4。留空跳过。"
HOSTS_BLOCK=""
HOSTS_COUNT=0
HOSTS_DUALSTACK=0
while :; do
  say ""
  read -rp "  上游域名（留空结束）: " hname
  [ -z "$hname" ] && break
  hname="$(printf '%s' "$hname" | xargs)"
  read -rp "    该域名的 IP（IPv4/IPv6，逗号分隔多个）: " haddrs
  haddrs="$(printf '%s' "$haddrs" | xargs)"
  [ -z "$haddrs" ] && continue
  HOSTS_BLOCK+="  \"${hname}\":"$'\n'
  IFS=',' read -ra HADDR_ARR <<< "$haddrs"
  has_v4=0; has_v6=0
  for ha in "${HADDR_ARR[@]}"; do
    ha="$(printf '%s' "$ha" | xargs)"
    [ -z "$ha" ] && continue
    HOSTS_BLOCK+="    - \"${ha}\""$'\n'
    case "$ha" in
      *:*) has_v6=1 ;;
      *)   has_v4=1 ;;
    esac
  done
  [ "$has_v4" -eq 1 ] && [ "$has_v6" -eq 1 ] && HOSTS_DUALSTACK=1
  HOSTS_COUNT=$((HOSTS_COUNT + 1))
done

# 组装 hosts 段：空映射输出 hosts: {}，否则输出 hosts: + 映射行。
if [ "$HOSTS_COUNT" -gt 0 ]; then
  HOSTS_YAML="hosts:"$'\n'"${HOSTS_BLOCK}"
else
  HOSTS_YAML="hosts: {}"$'\n'
fi

# --- 5. 上游 DNS（循环添加）---
say ""
info "【5/6】上游 DNS 服务商配置"
say "  项目不内置任何上游端点，请在此添加你自己的 DNS 服务商。"
say "  每个服务商可配置多种查询模式：DNS-over-HTTPS / DNS-over-TLS / DNS-over-QUIC / Plain DNS"

DNS_BLOCK=""
PROVIDERS=()
while :; do
  say ""
  read -rp "服务商名（留空结束）: " pname
  [ -z "$pname" ] && break
  PROVIDERS+=("$pname")
  DNS_BLOCK+="  ${pname}:"$'\n'
  while :; do
    say "    为 ${pname} 添加模式："
    say "      1) DNS-over-HTTPS   2) DNS-over-TLS   3) DNS-over-QUIC   4) Plain DNS   0) 完成"
    read -rp "    选择 [0]: " mchoice
    case "${mchoice:-0}" in
      1) mode="DNS-over-HTTPS" ;;
      2) mode="DNS-over-TLS" ;;
      3) mode="DNS-over-QUIC" ;;
      4) mode="Plain DNS" ;;
      *) break ;;
    esac
    read -rp "    ${mode} 地址（可不带前缀/端口，如 dns.example.com 或 1.1.1.1）: " maddr
    [ -z "$maddr" ] && continue
    maddr="$(normalize_upstream_addr "$mode" "$maddr")"
    say "      已规范化为: ${maddr}"
    DNS_BLOCK+="    ${mode}: \"${maddr}\""$'\n'
  done
done

[ -z "$DNS_BLOCK" ] && { warn "未配置任何上游，退出。"; exit 1; }

# 上游查询结果的赛马模式：最快返回 vs 加权 + 延时窗口。
say ""
info "上游查询结果赛马模式"
say "  1) 最快返回：并发查各家最优线路，谁先成功返回用谁"
say "  2) 加权 + 延时窗口：给每家设权重(1-100)，在首个响应后的窗口期内收集所有"
say "     响应，取权重最高者；权重相同时取响应最快者"
UPSTREAM_MODE="fastest"
WEIGHTS_YAML=""
RACE_WINDOW="50ms"
case "$(ask "选择赛马模式（1/2）" "1")" in
  2)
    UPSTREAM_MODE="weighted"
    RACE_WINDOW="$(ask_duration "延时窗口（首个响应后等待多久，如 50ms/80ms）" "50ms")"
    say ""
    say "  为每家服务商设置权重（1-100，默认 1，数字越大越优先）："
    WEIGHTS_YAML="upstream_weights:"$'\n'
    for pn in "${PROVIDERS[@]}"; do
      w="$(ask_int "    ${pn} 权重" "1" 1 100)"
      WEIGHTS_YAML+="  ${pn}: ${w}"$'\n'
    done
    ;;
  *)
    UPSTREAM_MODE="fastest"
    ;;
esac

# 地址族优先级：当上游域名存在 IPv4/IPv6 双栈时，让用户选择优先级。
# 触发条件有二：① hosts 映射中任一个域名同时配了 v4+v6；② 用引导 DNS 解析时
# 用户确认上游 DNS 支持双栈。
IP_PRIORITY="ipv4"
IP_LATENCY_INTERVAL="15m"
if [ "$HOSTS_DUALSTACK" -eq 1 ]; then
  DUALSTACK="true"
else
  # 未通过 hosts 固定双栈，则询问引导 DNS 解析的上游是否支持双栈。
  if ask_yn "  上游 DNS 域名是否支持双栈（同时解析出 IPv4 与 IPv6）" "n"; then
    DUALSTACK="true"
  else
    DUALSTACK="false"
  fi
fi

if [ "$DUALSTACK" = "true" ]; then
  say ""
  say "  上游域名存在 IPv4/IPv6 双栈，请选择地址族优先级："
  say "    1) IPv4 优先（v6 作连接失败回退）"
  say "    2) IPv6 优先（v4 作连接失败回退）"
  say "    3) 按延迟优先（周期探测两族延迟，选延迟低者）"
  case "$(ask "  选择优先级（1/2/3）" "1")" in
    2) IP_PRIORITY="ipv6" ;;
    3)
      IP_PRIORITY="latency"
      IP_LATENCY_INTERVAL="$(ask_duration "  延迟探测周期（如 15m/5m）" "15m")"
      ;;
    *) IP_PRIORITY="ipv4" ;;
  esac
fi

# --- 6. 生成 config.yaml ---
say ""
info "【6/6】生成配置并部署"

mkdir -p "$CONF_DIR" "$CERT_DIR"

cat > "$CONF_FILE" <<EOF
use_domain: ${USE_DOMAIN}
domain: "${DOMAIN}"

listeners:
  doh:
    enabled: ${DOH_ENABLED}
    port: ${DOH_PORT}
    path: "${DOH_PATH}"
  dot:
    enabled: ${DOT_ENABLED}
    port: ${DOT_PORT}
  doq:
    enabled: ${DOQ_ENABLED}
    port: ${DOQ_PORT}
  plain_dns:
    enabled: ${PLAIN_ENABLED}
    port: ${PLAIN_PORT}

ecs:
  mode: "${ECS_MODE}"
  address: "${ECS_ADDRESS}"

cert:
  mode: "${CERT_MODE}"
  renew: ${CERT_RENEW}
  cert_path: "${CERT_PATH}"
  key_path: "${KEY_PATH}"
  email: "${CERT_EMAIL}"
  provider: "${CERT_PROVIDER}"

probe_interval: ${PROBE_INTERVAL}
probe_timeout: ${PROBE_TIMEOUT}
probe_count: ${PROBE_COUNT}
probe_domain: "${PROBE_DOMAIN}"

cache_enabled: ${CACHE_ENABLED}
cache_size_bytes: ${CACHE_SIZE}
cache_ttl: ${CACHE_TTL}
cache_eviction: ${CACHE_EVICTION}

bootstrap:
${BOOTSTRAP_LIST}
bootstrap_cache_ttl: ${BOOTSTRAP_CACHE_TTL}

${HOSTS_YAML}ip_priority: ${IP_PRIORITY}
ip_latency_interval: ${IP_LATENCY_INTERVAL}

upstream_mode: ${UPSTREAM_MODE}
${WEIGHTS_YAML}race_window: ${RACE_WINDOW}

dns:
${DNS_BLOCK}
EOF

ok "已生成配置文件: ${CONF_FILE}"

# --- 7. 安装二进制 ---
obtain_binary
install -m 0755 "$BIN_SRC" "${INSTALL_DIR}/${SERVICE}"
ok "已安装二进制: ${INSTALL_DIR}/${SERVICE}"
if [ "${BIN_TMP:-0}" = "1" ]; then
  rm -f "$BIN_SRC"
fi

# --- 8. 证书申请（acme 模式）---
if [ "$NEEDS_TLS" = "true" ] && [ "$CERT_MODE" = "acme" ]; then
  say ""
  info "申请证书（acme.sh + DNS-01）..."

  if [ ! -x "${HOME}/.acme.sh/acme.sh" ]; then
    curl -fsSL https://get.acme.sh | sh -s email="$CERT_EMAIL"
  fi
  ACME="${HOME}/.acme.sh/acme.sh"

  # 签发：dns_<provider> 对应 acme.sh 内置的 dnsapi 脚本，凭证已 export 进环境。
  "$ACME" --issue --dns "dns_${CERT_PROVIDER}" -d "$DOMAIN" --server letsencrypt

  # --issue 成功后把证书安装到 config.yaml 指定的目标路径。
  # 注意：--issue 签出的证书在 ~/.acme.sh/<domain>/，并不在目标路径，
  # 必须靠 --install-cert 复制过去，否则 Go 程序加载证书会失败。
  "$ACME" --install-cert -d "$DOMAIN" \
    --key-file       "$KEY_PATH" \
    --fullchain-file "$CERT_PATH" \
    --reloadcmd      "systemctl restart ${SERVICE}"
  ok "证书已安装，续期后自动重启服务。"
fi

# --- 9. systemd 单元 + 启动 ---
cat > "/etc/systemd/system/${SERVICE}.service" <<EOF
[Unit]
Description=dnsproxy-router (DNS proxy with racing upstreams)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
Restart=always
RestartSec=3
ExecStart=${INSTALL_DIR}/${SERVICE} -config ${CONF_FILE}

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now "$SERVICE" 2>/dev/null || warn "systemctl 启动失败，请手动检查。"

say ""
ok "部署完成。"
say ""
say "  查看状态: systemctl status ${SERVICE}"
say "  查看日志: journalctl -u ${SERVICE} -f"

# 按开启的协议给出对应验证命令与放行端口。
firewall_ports=""
if [ "$DOH_ENABLED" = "true" ]; then
  if [ "$USE_DOMAIN" = "true" ] && [ -n "$DOMAIN" ]; then
    say "  验证 DoH:  curl -H 'accept: application/dns-message' \\"
    say "              'https://${DOMAIN}:${DOH_PORT}${DOH_PATH}?dns=AAABAAABAAAAAAAAB2V4YW1wbGUDY29tAAABAAE' | xxd | head"
  fi
  firewall_ports+="${DOH_PORT}/tcp "
fi
if [ "$DOT_ENABLED" = "true" ]; then
  say "  验证 DoT:  kdig -d @${DOMAIN} +tls-ca +tls-host=${DOMAIN} example.com A"
  firewall_ports+="${DOT_PORT}/tcp "
fi
if [ "$DOQ_ENABLED" = "true" ]; then
  say "  验证 DoQ:  kdig -d @${DOMAIN} +quic example.com A"
  firewall_ports+="${DOQ_PORT}/udp "
fi
if [ "$PLAIN_ENABLED" = "true" ]; then
  say "  验证明文:  dig @${DOMAIN} example.com A"
  firewall_ports+="${PLAIN_PORT}/udp ${PLAIN_PORT}/tcp "
fi

if [ -n "$firewall_ports" ]; then
  say ""
  say "  注意：甲骨文安全列表需放行：${firewall_ports}"
fi
