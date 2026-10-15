# ws-file-share

Go 服务端 + 单 HTML 客户端的文件下载工具。程序提供 HTTP/WS，由 Caddy 等反向代理提供对外 HTTPS/WSS。文件内容、目录树和下载控制全部通过 WebSocket 传输；CLI 管理使用 HTTP API。

支持 Windows、Linux、macOS。客户端使用桌面 Chrome / Edge，通过选择器授权保存目录，无需安装 Python、uv 或浏览器扩展。

## 快速开始

需要 Go 1.25 或以上版本。下面的命令在项目根目录执行。

```sh
# 初始化 config.yaml（不会覆盖已有配置，不生成证书）
go run ./cmd/ws-downloader init

# 添加文件或目录。服务未启动时仍会保存配置并返回资源 ID。
go run ./cmd/cli add /path/to/photos
go run ./cmd/cli add /path/to/archive.zip

# 启动 HTTP / WS 后端服务
go run ./cmd/ws-downloader serve
```

Windows 路径示例：`go run ./cmd/cli add "D:\Photos"`。

本地开发打开 **http://127.0.0.1:8080**，填入 CLI 返回的资源 ID，点击“获取文件列表”，选择保存文件夹并开始下载。也可以用 `http://127.0.0.1:8080/?id=资源ID` 预填资源。本机回环地址可使用浏览器目录读写 API；远程访问通过下面的 HTTPS 代理入口进行。

## Caddy 反向代理

程序只监听 HTTP/WS，不生成、读取或管理证书。证书申请、加载和续期由 Caddy 等代理负责。示例见 `Caddyfile.example`，将域名替换为实际域名：

```caddyfile
downloads.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

浏览器访问 `https://downloads.example.com`，页面自动使用 `wss://downloads.example.com/ws`。Caddy 的 `reverse_proxy` 支持 WebSocket，并默认保留 HTTP 上游请求的原始 Host，无需手写 Upgrade 转发规则。[Caddy 官方说明](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy)

默认 CLI 直接通过 `http://127.0.0.1:8080` 调用本机管理接口，也可以将 `server_url` 或 `--server` 设置为代理的 HTTPS 地址；此时使用系统默认的证书信任和校验。

## 构建可执行文件

```sh
go build -o dist/ ./cmd/ws-downloader ./cmd/cli
```

在 Windows 上得到 `dist/ws-downloader.exe`、`dist/cli.exe`，Linux/macOS 上得到无 `.exe` 后缀的两个文件。客户端 HTML 内嵌在服务端可执行文件中，不需要额外复制页面资源；`client/index.html` 也是完整的单文件页面。

```sh
./dist/ws-downloader init
./dist/cli add /path/to/photos
./dist/ws-downloader serve
```

## CLI 命令

所有命令支持 `--config 路径` 和 `--help`。管理命令可以通过 `--server http://127.0.0.1:8080` 或 `--server https://host` 覆盖配置中的管理地址；`add`、`remove` 支持把选项放在路径/ID 后面。

```sh
go run ./cmd/cli list
go run ./cmd/cli remove 资源ID
go run ./cmd/cli reload
go run ./cmd/cli add /path/to/file --config ./other/config.yaml
```

- ID 为规范化绝对路径字符串的 MD5；重复添加同一路径返回相同 ID。
- `add` / `remove` 先持久化配置，再通过 HTTP API 通知服务端重载。
- 无法连接服务时打印提示，配置保留，下次启动或手动 `reload` 后生效。
- 配置使用临时文件提交；并发 CLI 更新使用配置旁的空 `.lock` 目录互斥。
- 无效配置重载失败时，服务继续使用旧配置。已连接的下载保留原配置和目录树。
- 监听地址变化需要重启服务；其他配置支持重载。

## 配置

参见 `config.example.yaml`。默认监听 `127.0.0.1:8080`，配置中没有证书字段。

| 字段                 | 默认值                  | 含义                                                      |
| -------------------- | ----------------------- | --------------------------------------------------------- |
| `listen`             | `127.0.0.1:8080`        | HTTP/WS 后端监听地址                                      |
| `server_url`         | `http://127.0.0.1:8080` | CLI 默认管理地址，支持 HTTP/HTTPS                         |
| `chunk_bytes`        | `4194304`               | 下载块大小，默认 4 MiB，上限 64 MiB                       |
| `channel_bytes`      | `33554432`              | 每条连接的文件数据缓冲预算，默认 32 MiB，至少可容纳一块   |
| `client_concurrency` | `8`                     | 页面默认同时请求的分块数量                                |
| `workers`            | `8`                     | 每条连接的读取工作协程数                                  |
| `pending_requests`   | `64`                    | 每条连接未完成请求的数量上限                              |
| `allowed_origins`    | `[]`                    | 额外允许的页面 origin，默认允许同 Host 的 HTTP/HTTPS 页面 |
| `resources`          | `[]`                    | CLI 管理的资源 ID 与绝对路径                              |

代理与程序部署在同一台机器时，保持 `127.0.0.1:8080` 即可；分开部署时，将 `listen` 和代理上游地址调整为实际内网地址。代理应保留浏览器访问的 Host；如果代理改写 Host，或将单 HTML 页面部署在另一个站点，需在 `allowed_origins` 中加入页面的 origin，例如 `https://downloads.example.com`。

如果使用上一版生成的配置，删除 `tls_cert`、`tls_key`，将 `listen` 与 `server_url` 调整为上述配置。`init` 不覆盖已有文件。

目前按需求不提供 Token 认证。公网传输加密由代理的 TLS 提供，资源 ID 用于定位资源。

## 下载、续传与覆盖

- 保存资源原名：`/data/photos` 下载到所选目录下的 `photos/`；单个文件保留文件名。
- 包含空目录、空文件、隐藏文件；支持选择部分文件/目录，筛选不改变已有选择。
- 服务端解析软链接，下载其指向的真实文件或目录内容。允许目录外、绝对路径目标；无法解析的循环链接、目录递归环和失效链接会出现在页面记录中，其余条目继续处理。
- 客户端自主请求分块，默认 4 MiB/块、最多 8 个请求。服务端读取前申请容量，写入 WS 完成后释放；每条连接默认最多占用 32 MiB 文件数据缓冲，不含系统网络缓冲等开销。
- 每块携带 SHA-256；完整文件也进行流式 SHA-256 校验。无需将整个文件放入内存。
- 同名文件校验一致则跳过；不一致时默认报错，只有勾选“允许覆盖”才写入。
- 不监控源目录变化，也不提供版本快照。源文件在下载期间变化可能导致读失败或最终校验失败。
- 网络中断自动尝试重连、重新请求失败块，最多尝试 3 次；点击暂停或关闭页面后可重新选择原保存目录恢复。

临时文件与目标文件直接同级，不额外创建目录：

```text
photos/
  archive.zip.0._part
  archive.zip.1._part
  archive.zip._part.json
```

每块成功写入并关闭文件流后，才提交断点记录。恢复时先核验已有分块；丢失或损坏的块会重新请求。全部块齐全后流式合并写入 `archive.zip`，提交后复读校验，再删除分块和记录。合并中断时，已有分块仍可再次合并。合并阶段需要同时容纳分块和完整文件的磁盘空间。

为避免误覆盖，源资源名称与分块/记录名称冲突、记录属于其他任务或分块设置不一致时会明确报错。浏览器或目标操作系统不支持的文件名会作为对应条目的错误显示，不自动改名。

## 开发验证

Go 测试覆盖 HTTP/WS 分块下载、HTTPS/WSS 代理到明文后端、页面来源校验、乱序请求、路径范围限制、软链接与循环检测、配置重载和并发容量预算。前端集成测试使用项目真实 HTTP/WS 服务和磁盘文件适配器，覆盖暂停恢复、损坏分块、断线重连、覆盖与提交失败。

需要 Node.js 24 或以上版本运行前端集成测试。

```powershell
# Windows / PowerShell
./scripts/check.ps1
```

```sh
# Linux / macOS
sh scripts/check.sh
```

需求见 `plan.md`，消息格式见 `docs/protocol.md`。
