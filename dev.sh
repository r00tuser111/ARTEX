#!/usr/bin/env bash
# ARTEX development entrypoint. Run `./dev.sh help` for all commands.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT_DIR"

COMPOSE=(docker compose -p artex-dev -f "$ROOT_DIR/docker-compose.dev.yml")
BACKEND_PID=""
FRONTEND_PID=""

info() { printf '\033[36m[dev]\033[0m %s\n' "$*"; }
ok() { printf '\033[32m[dev]\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[dev]\033[0m %s\n' "$*"; }
die() { printf '\033[31m[dev]\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
用法：./dev.sh [命令]

命令：
  local    在宿主机运行 Go 后端和 Next.js；未配置数据库时用 Docker 启动 PostgreSQL（默认）
  docker   在 Docker 中运行 PostgreSQL、Go 后端和 Next.js 开发服务器
  db       只启动开发 PostgreSQL，供本地运行或测试使用
  logs     跟随 Docker 开发环境日志；可追加服务名，如 ./dev.sh logs backend
  ps       查看 Docker 开发环境状态
  down     停止 Docker 开发环境，保留数据库和依赖缓存
  clean    停止 Docker 开发环境并删除开发数据库、运行状态和依赖缓存卷
  help     显示本帮助

常用环境变量：
  ARTEX_PG_DSN                 使用已有 PostgreSQL；local 模式不会再启动开发数据库
  ARTEX_DEV_NO_DOCKER_DB=1     local 模式禁止自动启动 Docker PostgreSQL
  ARTEX_DEV_WEB_PORT=5173      前端端口
  ARTEX_DEV_API_PORT=8787      后端端口
  ARTEX_DEV_PROXY_PORT=8788    流量代理端口
  ARTEX_DEV_DB_PORT=5432       开发 PostgreSQL 宿主机端口
  ARTEX_DEV_DB_USER=artex      开发数据库账号
  ARTEX_DEV_DB_PASSWORD=artex-dev
  ARTEX_DEV_DB_NAME=artex
  ARTEX_DEV_SKIP_INSTALL=1     跳过本地前端依赖检查 / npm ci

示例：
  ./dev.sh                     # 等同 ./dev.sh local
  ./dev.sh docker
  ARTEX_PG_DSN='postgres://user:pass@localhost:5432/artex?sslmode=disable' ./dev.sh local
EOF
}

# Read simple KEY=VALUE entries without evaluating shell expressions. Existing
# exported variables win over .env so CI/shell overrides remain predictable.
load_dotenv() {
  [[ -f .env ]] || return 0
  local line key value first last
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line#"${line%%[![:space:]]*}"}"
    line="${line%"${line##*[![:space:]]}"}"
    [[ -z "$line" || "${line:0:1}" == "#" ]] && continue
    [[ "$line" == export\ * ]] && line="${line#export }"
    [[ "$line" == *=* ]] || continue
    key="${line%%=*}"
    value="${line#*=}"
    key="${key%"${key##*[![:space:]]}"}"
    [[ "$key" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || continue
    [[ -n "${!key+x}" ]] && continue
    value="${value#"${value%%[![:space:]]*}"}"
    value="${value%"${value##*[![:space:]]}"}"
    if (( ${#value} >= 2 )); then
      first="${value:0:1}"
      last="${value: -1}"
      if [[ "$first" == '"' && "$last" == '"' ]] || [[ "$first" == "'" && "$last" == "'" ]]; then
        value="${value:1:${#value}-2}"
      fi
    fi
    export "$key=$value"
  done < .env
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "缺少 $1。$2"
}

require_docker() {
  require_command docker "请安装 Docker Desktop 或 Docker Engine。"
  docker compose version >/dev/null 2>&1 || die "需要 Docker Compose v2（docker compose）。"
  docker info >/dev/null 2>&1 || die "Docker daemon 未运行，请先启动 Docker。"
}

urlencode() {
  node -e 'process.stdout.write(encodeURIComponent(process.argv[1]))' "$1"
}

dev_db_values() {
  DEV_DB_USER="${ARTEX_DEV_DB_USER:-artex}"
  DEV_DB_PASSWORD="${ARTEX_DEV_DB_PASSWORD:-artex-dev}"
  DEV_DB_NAME="${ARTEX_DEV_DB_NAME:-artex}"
  DEV_DB_PORT="${ARTEX_DEV_DB_PORT:-5432}"
}

wait_for_dev_db() {
  local attempt
  dev_db_values
  for attempt in $(seq 1 60); do
    if "${COMPOSE[@]}" exec -T postgres pg_isready -U "$DEV_DB_USER" -d "$DEV_DB_NAME" >/dev/null 2>&1; then
      ok "PostgreSQL 已就绪（127.0.0.1:${DEV_DB_PORT}）"
      return 0
    fi
    sleep 1
  done
  "${COMPOSE[@]}" logs postgres >&2 || true
  die "等待 PostgreSQL 就绪超时。"
}

start_dev_db() {
  require_docker
  dev_db_values
  info "启动隔离的开发 PostgreSQL…"
  "${COMPOSE[@]}" up -d postgres
  wait_for_dev_db
}

prepare_local_database() {
  if [[ -n "${ARTEX_PG_DSN:-}" ]]; then
    info "使用 ARTEX_PG_DSN 指定的 PostgreSQL。"
    return 0
  fi
  if [[ -f config.json ]]; then
    info "使用 config.json 中的 PostgreSQL 配置。"
    return 0
  fi
  [[ "${ARTEX_DEV_NO_DOCKER_DB:-0}" != "1" ]] || \
    die "未设置 ARTEX_PG_DSN/config.json，且已禁止自动启动 Docker 数据库。"
  start_dev_db
  export ARTEX_PG_DSN="postgres://$(urlencode "$DEV_DB_USER"):$(urlencode "$DEV_DB_PASSWORD")@127.0.0.1:${DEV_DB_PORT}/$(urlencode "$DEV_DB_NAME")?sslmode=disable"
}

prepare_local_dependencies() {
  require_command go "请安装 Go；项目版本见 go.mod。"
  require_command node "请安装 Node.js 22 LTS。"
  require_command npm "npm 应随 Node.js 一起安装。"

  if [[ "${ARTEX_DEV_SKIP_INSTALL:-0}" == "1" ]]; then
    warn "已跳过依赖安装检查。"
    return 0
  fi

  if [[ ! -x web/node_modules/.bin/next || web/package-lock.json -nt web/node_modules/.package-lock.json ]]; then
    info "安装前端依赖（npm ci）…"
    (cd web && npm ci)
  else
    info "前端依赖已就绪。"
  fi
}

check_local_ports() {
  local pair port label
  for pair in \
    "${ARTEX_DEV_WEB_PORT:-5173}:前端" \
    "${ARTEX_DEV_API_PORT:-8787}:后端" \
    "${ARTEX_DEV_PROXY_PORT:-8788}:代理"; do
    port="${pair%%:*}"
    label="${pair#*:}"
    if command -v lsof >/dev/null 2>&1 && lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
      die "$label 端口 $port 已被占用。可设置对应 ARTEX_DEV_*_PORT 后重试。"
    fi
  done
}

cleanup_local() {
  trap - EXIT INT TERM
  local pid
  for pid in "$FRONTEND_PID" "$BACKEND_PID"; do
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
    fi
  done
  for pid in "$FRONTEND_PID" "$BACKEND_PID"; do
    [[ -n "$pid" ]] && wait "$pid" 2>/dev/null || true
  done
}

wait_for_local_children() {
  local status=0
  while kill -0 "$BACKEND_PID" 2>/dev/null && kill -0 "$FRONTEND_PID" 2>/dev/null; do
    sleep 1
  done
  set +e
  if ! kill -0 "$BACKEND_PID" 2>/dev/null; then
    wait "$BACKEND_PID"
    status=$?
  else
    wait "$FRONTEND_PID"
    status=$?
  fi
  set -e
  return "$status"
}

run_local() {
  prepare_local_dependencies
  prepare_local_database
  check_local_ports

  local web_port="${ARTEX_DEV_WEB_PORT:-5173}"
  local api_port="${ARTEX_DEV_API_PORT:-8787}"
  local proxy_port="${ARTEX_DEV_PROXY_PORT:-8788}"

  trap cleanup_local EXIT INT TERM

  info "启动 Go 后端…"
  go run ./cmd/artex -addr ":$api_port" -proxy "127.0.0.1:$proxy_port" -data "$ROOT_DIR/data" &
  BACKEND_PID=$!

  info "启动 Next.js 开发服务器…"
  (
    cd web
    exec env AUTOPENTEST_API="http://127.0.0.1:$api_port" \
      NEXT_PUBLIC_SSE_BASE="http://localhost:$api_port" \
      npm run dev -- --hostname 0.0.0.0 --port "$web_port"
  ) &
  FRONTEND_PID=$!

  ok "开发环境已启动：http://localhost:$web_port"
  info "后端：http://localhost:$api_port · 流量代理：127.0.0.1:$proxy_port · Ctrl-C 退出"
  wait_for_local_children
}

run_docker() {
  require_docker
  dev_db_values
  case "$DEV_DB_USER$DEV_DB_PASSWORD$DEV_DB_NAME" in
    *[:/@?#%]*) die "Docker 开发数据库账号、密码和库名需使用 URL 安全字符（不要包含 : / @ ? # %）。" ;;
  esac
  ok "Docker 开发环境将启动：http://localhost:${ARTEX_DEV_WEB_PORT:-5173}"
  info "首次构建会下载 Go、Node 和项目依赖；源码通过 bind mount 实时映射。"
  exec "${COMPOSE[@]}" up --build --remove-orphans
}

load_dotenv
COMMAND="${1:-local}"

case "$COMMAND" in
  local) run_local ;;
  docker) run_docker ;;
  db) start_dev_db ;;
  logs) require_docker; exec "${COMPOSE[@]}" logs -f "${@:2}" ;;
  ps|status) require_docker; exec "${COMPOSE[@]}" ps ;;
  down|stop) require_docker; exec "${COMPOSE[@]}" down --remove-orphans ;;
  clean)
    require_docker
    warn "将删除 artex-dev 的 PostgreSQL、运行状态、data 和 Go/npm 缓存卷。"
    exec "${COMPOSE[@]}" down -v --remove-orphans
    ;;
  help|-h|--help) usage ;;
  *) usage >&2; die "未知命令：$COMMAND" ;;
esac
