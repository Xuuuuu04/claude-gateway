# Claude Gateway（个人多厂商模型网关）

目标：把 Claude Code / OpenAI 客户端的请求统一打到一个入口，由网关完成多厂商转换、路由与负载策略，并提供一个最小面板管理配置与查看日志。

## 快速启动（开发）

需要环境变量：
- `MYSQL_DSN`：例如 `user:pass@tcp(127.0.0.1:3306)/claude_gateway?parseTime=true&charset=utf8mb4&collation=utf8mb4_unicode_ci`
- `ADMIN_TOKEN`：访问 `/admin` 与 `/admin/api/*` 的管理 token（请求头 `Authorization: Bearer <token>`）
- `KEY_ENC_MASTER_B64`：32 字节主密钥的 base64（用于加密落库的上游 API Key）
- `CLIENT_TOKEN`：可选。设置后，所有 `/v1/*` 请求需携带 token（`Authorization: Bearer ...` 或 `x-api-key`）
- `HTTP_ADDR`：默认 `:8080`

启动：

```bash
go run ./cmd/gateway
```

## Claude Code 接入（思路）

Claude Code 支持通过 `ANTHROPIC_BASE_URL` 指向兼容 Anthropic Messages API 的自建服务，并用 `ANTHROPIC_AUTH_TOKEN/ANTHROPIC_API_KEY` 与 `ANTHROPIC_MODEL` 选择模型。网关会实现 `/v1/messages` 兼容与 SSE 流式事件兼容。

## 安全提醒

- 不要把任何 API Key 或数据库密码写进代码或提交到仓库。
- 生产环境建议为网关单独创建 MySQL 账号并最小授权（不要使用 root）。

## 部署

见 `docs/DEPLOY.md`。


## 开发进度（截至 2026-02-07）
- 已完成可公开仓库基线整理：补齐许可证、清理敏感与内部说明文件。
- 当前版本可构建/可运行，后续迭代以 issue 与提交记录持续公开追踪。
