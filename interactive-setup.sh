#!/usr/bin/env bash
set -euo pipefail

# ============================================================================
# dnsproxy-scheduler 交互式配置 + 部署脚本
# 通过交互引导用户填写配置，生成 config.yaml，申请证书，部署并启动服务。
#
# 用法（root 下执行）:
#   bash interactive-setup.sh [二进制路径]
#   默认二进制路径: ./dnsproxy-scheduler
# ============================================================================

BIN_SRC="${1:-$(dirname "$0")/dnsproxy-scheduler}"
INSTALL_DIR="/usr/local/bin"
CONF_DIR="/etc/dnsproxy"
CONF_FILE="${CONF_DIR}/config.yaml"
CERT_DIR="${CONF_DIR}/certs"
SERVICE="dnsproxy-scheduler"

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

# ask_yn <提示> [默认 y/n]
ask_yn() {
  local prompt="$1" default="${2:-n}" input
  read -rp "$(printf '%s [%s]: ' "$prompt" "$default")" input
  input="${input:-$default}"
  [[ "$input" =~ ^[Yy] ]]
}

# --- 0. 前置检查 ---
if [ "$(id -u)" -ne 0 ]; then
  warn "请用 root 执行（或 sudo）。"
  exit 1
fi

say ""
say "=========================================================="
say "  dnsproxy-scheduler 交互式配置向导"
say "=========================================================="
say ""

# --- 1. 基本配置 ---
info "【1/6】基本配置"

if ask_yn "是否使用域名（用于证书申请）" "y"; then
  USE_DOMAIN="true"
  DOMAIN="$(ask "域名" "example.com")"
else
  USE_DOMAIN="false"
  DOMAIN=""
fi

if ask_yn "使用标准端口 443（DoH 默认，推荐）" "y"; then
  PORT=443
else
  PORT="$(ask "自定义对外 DoH 监听端口" "")"
  # 简单校验端口
  if ! [[ "$PORT" =~ ^[0-9]+$ ]] || [ "$PORT" -lt 1 ] || [ "$PORT" -gt 65535 ]; then
    warn "非法端口，使用默认 443"
    PORT=443
  fi
fi

DOH_PATH="$(ask "DoH 端点路径（可自定义，支持多层，如 /dns/query/v1）" "/dns-query")"
# 简单校验路径：以 / 开头且不以 / 结尾（根路径 / 除外）
if [[ "$DOH_PATH" != /* ]]; then
  warn "路径必须以 / 开头，已改为默认 /dns-query"
  DOH_PATH="/dns-query"
elif [ "$DOH_PATH" != "/" ] && [[ "$DOH_PATH" == */ ]]; then
  warn "路径不能以 / 结尾，已去掉末尾 /"
  DOH_PATH="${DOH_PATH%/}"
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

# --- 2. 证书配置 ---
say ""
info "【2/6】证书配置"
say "  DoH 必须使用 HTTPS，因此需要一个 TLS 证书。"
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
  CERT_PROVIDER="$(ask "DNS API 提供商（acme DNS-01）" "cloudflare")"

  # Cloudflare 需要 API Token
  if [ "$CERT_PROVIDER" = "cloudflare" ]; then
    if [ -n "${CF_TOKEN:-}" ]; then
      say "  检测到环境变量 CF_TOKEN，将使用它。"
      CF_Token="$CF_TOKEN"
    else
      read -rsp "  Cloudflare API Token（Zone>DNS>Edit，输入不回显）: " CF_Token
      say ""
      [ -z "$CF_Token" ] && { warn "未提供 Token，稍后申请证书会失败。"; }
    fi
  fi
fi

# --- 3. 探测参数 ---
say ""
info "【3/6】探测参数（每周期对各家各模式测延迟，选每家最优）"
PROBE_INTERVAL="$(ask "探测周期（如 15m/300s）" "15m")"
PROBE_TIMEOUT="$(ask "单次探测超时（如 3s）" "3s")"
PROBE_COUNT="$(ask "每模式探测次数（取中位数）" "3")"
PROBE_DOMAIN="$(ask "探测用域名" "example.com.")"

# --- 4. 缓存与引导 ---
say ""
info "【4/6】缓存与引导 DNS"

# 响应缓存：先问是否开启，开启才问大小。
if ask_yn "开启 DNS 响应缓存（若客户端已做本地缓存，可关闭）" "y"; then
  CACHE_ENABLED="true"
  CACHE_SIZE="$(ask "响应缓存大小（字节）" "67108864")"
else
  CACHE_ENABLED="false"
  CACHE_SIZE="0"
fi

BOOTSTRAP_INPUT="$(ask "引导 DNS（逗号分隔，用于解析上游域名）" "1.1.1.1:53,8.8.8.8:53")"

# 引导解析缓存：先问是否开启，开启才问缓存时长。
if ask_yn "开启引导解析缓存（缓存上游域名解析结果）" "y"; then
  BOOTSTRAP_CACHE_TTL="$(ask "引导解析缓存时长（如 5s/1m）" "5s")"
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

# --- 5. 上游 DNS（循环添加）---
say ""
info "【5/6】上游 DNS 服务商配置"
say "  项目不内置任何上游端点，请在此添加你自己的 DNS 服务商。"
say "  每个服务商可配置多种查询模式：DNS-over-HTTPS / DNS-over-TLS / DNS-over-QUIC"

DNS_BLOCK=""
while :; do
  say ""
  read -rp "服务商名（留空结束）: " pname
  [ -z "$pname" ] && break
  DNS_BLOCK+="  ${pname}:"$'\n'
  while :; do
    say "    为 ${pname} 添加模式："
    say "      1) DNS-over-HTTPS   2) DNS-over-TLS   3) DNS-over-QUIC   0) 完成"
    read -rp "    选择 [0]: " mchoice
    case "${mchoice:-0}" in
      1) mode="DNS-over-HTTPS" ;;
      2) mode="DNS-over-TLS" ;;
      3) mode="DNS-over-QUIC" ;;
      *) break ;;
    esac
    read -rp "    ${mode} 地址: " maddr
    [ -z "$maddr" ] && continue
    DNS_BLOCK+="    ${mode}: \"${maddr}\""$'\n'
  done
done

[ -z "$DNS_BLOCK" ] && { warn "未配置任何上游，退出。"; exit 1; }

# --- 6. 生成 config.yaml ---
say ""
info "【6/6】生成配置并部署"

mkdir -p "$CONF_DIR" "$CERT_DIR"

cat > "$CONF_FILE" <<EOF
use_domain: ${USE_DOMAIN}
domain: "${DOMAIN}"
port: ${PORT}
doh_path: "${DOH_PATH}"

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

bootstrap:
${BOOTSTRAP_LIST}
bootstrap_cache_ttl: ${BOOTSTRAP_CACHE_TTL}

dns:
${DNS_BLOCK}
EOF

ok "已生成配置文件: ${CONF_FILE}"

# --- 7. 安装二进制 ---
if [ ! -f "$BIN_SRC" ]; then
  warn "找不到二进制 $BIN_SRC"
  say "  请先构建并上传："
  say "    GOOS=linux GOARCH=amd64 go build -o dnsproxy-scheduler ./cmd/dnsproxy"
  say "    scp dnsproxy-scheduler root@VPS:/tmp/"
  say "  然后重新运行: bash $0 /tmp/dnsproxy-scheduler"
  exit 1
fi
install -m 0755 "$BIN_SRC" "${INSTALL_DIR}/${SERVICE}"
ok "已安装二进制: ${INSTALL_DIR}/${SERVICE}"

# --- 8. 证书申请（acme 模式）---
if [ "$CERT_MODE" = "acme" ]; then
  say ""
  info "申请证书（acme.sh + DNS-01）..."

  if [ ! -x "${HOME}/.acme.sh/acme.sh" ]; then
    curl -fsSL https://get.acme.sh | sh -s email="$CERT_EMAIL"
  fi
  ACME="${HOME}/.acme.sh/acme.sh"

  if [ "$CERT_PROVIDER" = "cloudflare" ]; then
    export CF_Token="${CF_Token:-${CF_TOKEN:-}}"
    "$ACME" --issue --dns dns_cf -d "$DOMAIN" --server letsencrypt

    # --issue 成功后把证书安装到 config.yaml 指定的目标路径。
    # 注意：--issue 签出的证书在 ~/.acme.sh/<domain>/，并不在目标路径，
    # 必须靠 --install-cert 复制过去，否则 Go 程序加载证书会失败。
    "$ACME" --install-cert -d "$DOMAIN" \
      --key-file       "$KEY_PATH" \
      --fullchain-file "$CERT_PATH" \
      --reloadcmd      "systemctl restart ${SERVICE}"
    ok "证书已安装，续期后自动重启服务。"
  else
    warn "非 cloudflare 的 provider 请手动签发证书后，把 cert.mode 改为 existing。"
    warn "跳过自动签发。"
  fi
fi

# --- 9. systemd 单元 + 启动 ---
cat > "/etc/systemd/system/${SERVICE}.service" <<EOF
[Unit]
Description=dnsproxy-scheduler (DoH proxy with racing upstreams)
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
if [ "$USE_DOMAIN" = "true" ] && [ -n "$DOMAIN" ]; then
  say "  验证:     curl -H 'accept: application/dns-message' \\"
  say "              'https://${DOMAIN}:${PORT}${DOH_PATH}?dns=AAABAAABAAAAAAAAB2V4YW1wbGUDY29tAAABAAE' | xxd | head"
fi
say ""
say "  注意：甲骨文安全列表需放行 ${PORT}/TCP。"
