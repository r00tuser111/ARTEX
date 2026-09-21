# ARTEX 开发环境

`dev.sh` 是源码开发的统一入口，支持宿主机运行和全 Docker 运行。两种模式都启动：

- Next.js 开发服务器：`http://localhost:5173`
- Go API：`http://localhost:8787`
- 流量录制代理：`127.0.0.1:8788`
- PostgreSQL：`127.0.0.1:5432`

生产部署仍使用 `docker-compose.yml` 和 `Dockerfile`；开发环境使用独立的 `artex-dev` Compose 项目、`docker-compose.dev.yml` 和 `Dockerfile.dev`，不会复用生产数据库卷。

## 快速开始

### 本地运行 Go 和 Next.js

需要本机安装 Go、Node.js 22/npm，以及 Docker（仅用于零配置启动 PostgreSQL）：

```bash
./dev.sh
# 或显式写：./dev.sh local
```

脚本会依次执行：

1. 读取 `.env` 中简单的 `KEY=VALUE` 配置；已经 export 的同名变量优先。
2. 检查前端依赖，缺少依赖或 lockfile 更新时运行 `npm ci`；Go 在编译时按需下载缺少的模块。
3. 如果没有 `ARTEX_PG_DSN` 和 `config.json`，自动启动隔离的 PostgreSQL 16 容器并等待健康检查通过。
4. 同时运行 Go 后端和 Next.js 开发服务器。前端源码支持热更新；修改 Go 代码后重新运行脚本即可重启后端。

按 `Ctrl-C` 会停止 Go 和 Next.js，不会删除开发数据库。数据库可由 `./dev.sh down` 停止。

如果不想使用 Docker 数据库，可连接已有 PostgreSQL：

```bash
ARTEX_PG_DSN='postgres://user:pass@127.0.0.1:5432/artex?sslmode=disable' ./dev.sh local
```

也可以使用项目根目录的 `config.json`。设置 `ARTEX_DEV_NO_DOCKER_DB=1` 后，缺少这两种配置时脚本会直接报错。

### 全 Docker 开发

本机只需要 Docker Desktop 或 Docker Engine + Compose v2：

```bash
./dev.sh docker
```

源码会挂载到 `/workspace`。前端使用独立的 `node_modules` 和 `.next` 卷，后端使用独立的运行状态、Go 模块和构建缓存卷，容器重建时不需要重新下载全部依赖。后端健康检查通过后才会启动前端。首次运行需要构建开发镜像，耗时会更长。

修改前端源码会自动热更新。修改 Go 源码后停止并重新执行 `./dev.sh docker`；Go 构建缓存仍会保留。

## 管理命令

```bash
./dev.sh db                 # 只启动开发 PostgreSQL
./dev.sh ps                 # 查看开发容器状态
./dev.sh logs               # 查看全部日志
./dev.sh logs backend       # 只看后端日志
./dev.sh down               # 停止开发容器，保留所有卷
./dev.sh clean              # 停止并删除开发数据库、data 和依赖缓存卷
./dev.sh help               # 查看完整帮助
```

`clean` 会永久删除 `artex-dev` Compose 项目的 PostgreSQL、ARTEX data、运行状态、Go 和 npm 缓存卷，不影响生产 Compose 项目的卷。

## 配置与端口

`.env` 可继续保存 LLM 配置；`dev.sh` 和开发 Compose 都会读取它。以下变量只影响开发环境：

| 变量 | 默认值 | 用途 |
| --- | --- | --- |
| `ARTEX_DEV_WEB_PORT` | `5173` | Next.js 宿主机端口 |
| `ARTEX_DEV_API_PORT` | `8787` | Go API 宿主机端口 |
| `ARTEX_DEV_PROXY_PORT` | `8788` | 流量代理宿主机端口 |
| `ARTEX_DEV_DB_PORT` | `5432` | PostgreSQL 宿主机端口 |
| `ARTEX_DEV_DB_USER` | `artex` | 开发数据库用户 |
| `ARTEX_DEV_DB_PASSWORD` | `artex-dev` | 开发数据库密码 |
| `ARTEX_DEV_DB_NAME` | `artex` | 开发数据库名 |
| `ARTEX_DEV_SKIP_INSTALL` | `0` | 设为 `1` 跳过本地依赖准备 |
| `WATCHPACK_POLLING` | `true` | Docker 内使用轮询发现前端文件变化 |

Docker 模式会把开发数据库端口只绑定到 `127.0.0.1`。自定义 Docker 开发数据库账号、密码和库名时，请使用 URL 安全字符，避免 `: / @ ? # %`。

## 测试与质量检查

先启动测试数据库，再让测试进程使用同一 DSN：

```bash
./dev.sh db
export ARTEX_PG_DSN='postgres://artex:artex-dev@127.0.0.1:5432/artex?sslmode=disable'
go test ./...

cd web
npm run check
npm run build
```

需要完全隔离测试数据时，建议通过 `ARTEX_DEV_DB_NAME` 使用单独数据库，或为测试单独创建 PostgreSQL 数据库，不要把测试指向生产库。

## 常见问题

### 端口已占用

通过环境变量换端口，例如：

```bash
ARTEX_DEV_WEB_PORT=5174 ARTEX_DEV_API_PORT=8789 ARTEX_DEV_PROXY_PORT=8790 ./dev.sh local
```

前端会自动使用新的 API 和 SSE 地址。Docker 模式使用相同变量。

### Docker 文件变化未触发前端刷新

开发 Compose 默认启用 `WATCHPACK_POLLING=true`。如果宿主机文件系统支持原生事件且希望降低轮询开销，可在 `.env` 中设为 `false`。

### 重置开发环境

```bash
./dev.sh clean
./dev.sh docker   # 或 ./dev.sh local
```

这会重新创建开发数据库和依赖缓存；其中的开发数据无法恢复。
