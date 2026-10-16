# WS下载器协议 v1

下载端口通过 HTTP 提供页面 `/`、健康检查 `/healthz` 和 WS。WS 默认使用 `/ws`，可通过 `ws_path` 配置并重载，新路径不影响已有连接。前端未指定 `client_ws_url` 时，自动使用页面同源的 `ws_path`。下载默认监听 `127.0.0.1:8080`。Caddy 等反向代理在入口终止 TLS，对外提供 HTTPS/WSS，再转发到下载端口的 HTTP/WS。程序不生成、读取或管理证书。文件数据只走 WebSocket。

管理接口 `POST /api/reload` 仅由独立管理端口提供，默认监听 `127.0.0.1:8081`，通过 `admin_listen` 配置且仅允许回环地址。下载端口的 `/api`、`/api/*` 始终返回 404；管理端口不提供页面或 WS，也不接受非回环客户端或带 `Origin` 的请求。CLI 根据 `admin_listen` 自动生成 HTTP 地址连接管理端口，无需配置 `server_url`。更改任一监听地址需重启服务。

`cli add` 在重载请求中附带 `{"prepare_resource_id":"资源ID"}`，服务端重载配置后预先计算该资源的 SHA-256，缓存完成后才响应。响应增加 `prepared: {"files": 文件数, "warnings": 无法处理的条目数}`；仅允许准备配置中存在的资源。普通重载可保持请求体为空，不触发预计算。

代理应保留原始 `Host`（Caddy 对 HTTP 上游默认如此），使浏览器的 HTTPS Origin 与代理转发的 Host 对应。程序不依赖后端连接是否为 TLS 来判断页面 Origin。若代理改写 Host，需在 `allowed_origins` 显式填写页面 origin。

## 控制请求

客户端发送 JSON 文本消息。`request_id` 为不超过 64 字节的非空字符串，在当前连接的未完成请求中必须唯一。

每条连接先请求一次目录树：

```json
{ "type": "manifest", "request_id": "1", "resource_id": "md5-of-absolute-path" }
```

响应包含 `type: "manifest"`、对应的 `request_id` 和 `manifest`：

```json
{
  "resource_id": "md5-of-absolute-path",
  "name": "photos",
  "chunk_bytes": 4194304,
  "client_concurrency": 8,
  "max_pending_requests": 64,
  "entries": [
    { "id": "photos", "path": "photos", "kind": "directory", "size": 0 },
    {
      "id": "photos/a.zip",
      "path": "photos/a.zip",
      "kind": "file",
      "size": 8000000,
      "sha256": "64-hex-digits"
    }
  ],
  "warnings": []
}
```

`path` 是包含资源顶层名称的 `/` 分隔相对路径，`id` 只在当前目录树中定位文件；不会向客户端暴露服务端绝对路径。软链接已解析为普通文件/目录条目。完整文件 SHA-256 由服务端本地 KV 缓存提供：首次访问或大小、修改时间变化时流式计算并保存，其他请求复用缓存。内部缓存数据库不出现在目录树中。`warnings` 中每项包含 `path` 和 `message`。

客户端下载选中的文件块，索引从 0 开始：

```json
{ "type": "block", "request_id": "2", "file_id": "photos/a.zip", "index": 0 }
```

只能请求当前目录树中的普通文件，最后一块允许不足 `chunk_bytes`。空文件不请求数据块。服务端可能按不同于请求的顺序返回结果，客户端使用 `request_id` 和块定位信息匹配响应。

## 二进制块响应

每个块对应一条 WS 二进制消息，可在底层由多个 WS 帧组成：

```text
4 字节：JSON 消息头长度，uint32，大端
N 字节：UTF-8 JSON 消息头
剩余字节：原始文件数据，不进行 Base64 编码
```

消息头：

```json
{
  "type": "block",
  "request_id": "2",
  "file_id": "photos/a.zip",
  "index": 0,
  "offset": 0,
  "length": 4194304,
  "crc32c": "8-hex-digits"
}
```

`crc32c` 为原始分块数据的 CRC32C（Castagnoli），以固定 8 位小写十六进制字符串表示，不带 `0x`。例如 ASCII `123456789` 的校验值为 `e3069283`。此字段替代原分块消息头中的 `sha256`，服务端与页面需同步更新；目录树中的整文件 `sha256` 保持不变。

客户端核验块定位、长度与 CRC32C，写入独立的同级分块文件并成功关闭可写流后，才提交含 `crc32c`、`length` 的断点并补充下一个请求。续传与合并时重新计算分块 CRC32C，整文件仍使用 SHA-256 校验。旧版断点中的分块 SHA-256 经核验后转换为 CRC32C。不使用额外的逐块业务 ACK。

## 容量与错误

- 每条连接使用独立的文件数据预算，覆盖准备、排队和正在写入的数据；先申请容量再读取，WS 消息写入完成后释放。
- JSON 消息头和控制消息不计入文件数据预算；请求数量另有上限。
- 超过未完成请求上限或复用仍在处理的请求 ID 时，连接以 1013 关闭。非法控制消息以 1008 关闭。
- JSON 控制请求最大 64 KiB。服务端禁用 WS 消息压缩。
- 服务端定期发送 WS Ping，浏览器由协议栈处理 Pong。

常规请求错误返回文本 JSON，不关闭其他正常请求：

```json
{
  "type": "error",
  "request_id": "2",
  "code": "read_failed",
  "message": "错误原因"
}
```

错误包括 `not_found`、`invalid_block`、`read_failed`、`manifest_failed`、`not_ready`、`already_listed`、`unknown_type`。断线后重新连接并获取一次目录树，再请求本地尚缺失的块。协议不提供源文件版本和一致性快照。
