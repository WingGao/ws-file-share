# 开发指南

面向修改源码、编译程序和排查实现问题的开发者。日常使用与部署见 [README](../readme.md)，消息格式见[传输协议](protocol.md)，需求与边界见[项目需求](plan.md)。

## 开发环境

| 工具 | 版本或要求 | 用途 |
| --- | --- | --- |
| Go | 1.25 或以上 | 构建、Go 测试和静态检查 |
| Node.js | 24 或以上 | 前端集成测试；运行服务和使用页面不需要 Node.js |
| 桌面 Chrome / Edge | 支持目录读写 API | 手动验证浏览器下载流程 |

项目支持 Windows、Linux、macOS。除特别注明外，以下命令在仓库根目录执行。

## 从源码编译

```sh
go build -o dist/ ./cmd/ws-downloader ./cmd/cli
```

生成两个程序：

| 程序 | Linux / macOS | Windows |
| --- | --- | --- |
| 服务端 | `dist/ws-downloader` | `dist/ws-downloader.exe` |
| 管理 CLI | `dist/cli` | `dist/cli.exe` |

进入 `dist` 目录后，可按 [README 快速开始](../readme.md#快速开始)初始化并运行。页面由 Go 嵌入服务端，无需额外的前端构建或复制静态资源。修改 `client/index.html` 后，需要重新编译服务端。

## 直接运行源码

首次初始化：

```sh
go run ./cmd/ws-downloader init
```

在一个终端启动服务：

```sh
go run ./cmd/ws-downloader serve
```

另一个终端添加测试资源：

```sh
go run ./cmd/cli add /path/to/photos
go run ./cmd/cli add /path/to/archive.zip
```

Windows 路径示例：

```powershell
go run ./cmd/cli add "D:\Photos"
```

打开 `http://127.0.0.1:8080`，使用返回的资源 ID 下载。也可以在启动前添加资源：CLI 会先保存配置，连接服务失败时提示下次启动生效。

默认的 `config.yaml` 相对于命令的工作目录解析。切换源码运行与 `dist` 程序时，注意配置文件的位置；必要时给服务端和 CLI 同时指定 `--config`。

## 运行检查

**Windows / PowerShell：**

```powershell
./scripts/check.ps1
```

**Linux / macOS：**

```sh
sh scripts/check.sh
```

脚本依次执行：

1. `go test ./... -count=1`。
2. `go vet ./...`。
3. 构建供前端测试使用的服务端程序。
4. `node --test client/client.test.cjs`。

模块缓存、构建缓存和测试程序放在仓库内的 `.cache/`，已由 Git 忽略。集成测试需要在本机监听临时 HTTP/WS 端口，执行环境应允许该操作。

需要单独验证 Go 代码时：

```sh
go test ./... -count=1
go vet ./...
```

需要单独运行前端集成测试时，先构建测试服务端：

```sh
# Linux / macOS
go build -o .cache/ws-test ./cmd/ws-downloader
node --test client/client.test.cjs
```

```powershell
# Windows / PowerShell
go build -o .cache/ws-test.exe ./cmd/ws-downloader
node --test client/client.test.cjs
```

Go 测试覆盖端口隔离、反向代理、配置重载、乱序分片请求、路径范围、软链接与容量预算。前端集成测试使用真实 HTTP/WS 服务和磁盘文件适配器，覆盖校验、暂停恢复、断线重连、覆盖、磁盘提交失败和 CLI 管理流程。

磁盘适配器无法代替真实浏览器的权限和文件类型策略。浏览器 API 限制的测试使用模拟错误；更改相关代码后，还需手动验证目录授权、HTTPS/WSS、HTTP 来源例外和 Windows DLL 文件限制。

## 代码布局

```text
cmd/ws-downloader/   服务端入口
cmd/cli/             管理 CLI 入口
internal/command/    命令解析与管理请求
internal/config/     YAML 配置、校验与持久化
server/              HTTP/WS 服务、目录树和分片读取
client/index.html    单文件浏览器客户端
client/embed.go      页面嵌入声明
client/client.test.cjs  前端集成测试
scripts/             Windows、Linux/macOS 检查脚本
docs/                使用之外的技术文档
```

## 配置与管理接口

下载服务默认监听 `127.0.0.1:8080`，提供页面、健康检查和 WebSocket。管理服务默认监听 `127.0.0.1:8081`，只提供 `POST /api/reload`。反向代理负责入口 TLS，Go 后端使用 HTTP/WS。

管理地址只配置 `admin_listen`。CLI 自动生成对应的 HTTP URL，`localhost` 固定转换为 IPv4 回环地址；可用 `--server` 临时覆盖 CLI 地址。旧 `server_url` 在读取时兼容忽略，不会再次写入配置。

管理监听器只接受回环地址。请求还会校验直接连接的来源 IP，忽略转发头，并拒绝带 `Origin` 的请求。WS 的 `allowed_origins` 不用于放行管理请求。下载服务拦截整个 `/api`、`/api/*` 路径范围并返回 404。

配置解析拒绝未知字段和多份 YAML 文档。CLI 修改使用临时文件提交，通过配置旁的空 `.lock` 目录互斥。确认没有 CLI 进程运行后，可以手动移除异常退出遗留的锁目录。

`add`、`remove` 先持久化资源列表，再通知重载。重载失败保留原活动配置；已有 WS 连接保留连接时的配置与目录树。更改下载或管理监听地址需要重启；两个监听器都成功绑定后才开始服务。

资源 ID 是规范化绝对路径的 MD5，作用是定位资源。当前没有 Token 认证。资源中的软链接会解析到真实目标，包括目录外目标；失效链接、递归环等记录为警告，其余条目继续处理。

## 前端配置与访问地址

服务端提供页面时，把 `client_ws_url`、`ws_path` 写入 HTML 的元数据，动态值经过 HTML 转义。未配置前端 URL 时，页面使用当前主机、对应的 WS/WSS 协议和 `ws_path`；只填 WS 主机时补上配置的路径。

页面 `?server=...` 参数可以覆盖默认地址，用户也可手动输入。直接部署原始 `client/index.html` 时不会收到 Go 服务的动态配置，需要使用完整服务地址，并在 WS 服务端配置允许的页面来源。

## 传输参数

完整配置示例见 [config.example.yaml](../config.example.yaml)。通常保持默认值即可；定位性能问题时可调整：

| 配置项 | 默认值 | 范围与作用 |
| --- | --- | --- |
| `chunk_bytes` | `4194304`（4 MiB） | 每块 1 字节至 64 MiB，最后一块可不足此大小 |
| `channel_bytes` | `33554432`（32 MiB） | 每条 WS 连接的数据预算，不小于块大小，最多 1 GiB |
| `client_concurrency` | `8` | 页面默认分块并发，1～64，用户可在页面调整 |
| `workers` | `8` | 每条连接的读取工作协程，1～64 |
| `pending_requests` | `64` | 每条连接未完成请求的上限，1～4096 |

客户端主动请求分片，接收并写入完成后补充请求。服务端读取前申请容量，单一发送循环写入 WS 后释放。容量预算覆盖应用层文件数据的读取、排队和发送，不等于进程总内存上限，也不包含系统网络缓冲。

## 校验与断点实现

目录树生成时计算完整文件 SHA-256。每个分片携带 CRC32C，下载接收、恢复和合并时核验；CRC32C 用于检测意外损坏，不抵抗恶意构造碰撞。客户端在合并过程中计算整文件 SHA-256，并在提交后复读校验。

分片与 JSON 记录直接保存在目标文件同级。成功关闭分片可写流后才记录进度；并发写入的记录提交串行执行。合并完成、提交并复读验证后才删除分片和记录，避免失败时丢失唯一的续传副本。

断点按资源 ID、文件 ID、相对路径、大小和分块大小匹配，不要求 WS 地址保持不变。旧 SHA-256 分片记录经过验证后转换为 CRC32C。源文件不提供版本快照，下载期间的内容变化可能在读取或最终校验时失败。

具体消息头、错误码、容量和连接约束见[协议文档](protocol.md)。
