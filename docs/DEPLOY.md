# 部署与接入

## 生产建议
- 为网关单独创建 MySQL 用户（不要使用 root），仅授予所需库的读写权限。
- `ADMIN_TOKEN` 与 `CLIENT_TOKEN` 只通过环境变量注入，不要写进代码或提交。
- `KEY_ENC_MASTER_B64` 是加密主密钥（base64 的 32 字节），生成后长期保存且不要泄露。

## 环境变量
- `HTTP_ADDR`：例如 `:8080`
- `MYSQL_DSN`：例如 `gw:pass@tcp(127.0.0.1:3306)/claude_gateway?parseTime=true&charset=utf8mb4&collation=utf8mb4_unicode_ci`
- `KEY_ENC_MASTER_B64`：32 字节主密钥 base64
- `ADMIN_TOKEN`：访问 `/admin` 的管理 token
- `CLIENT_TOKEN`：可选。设置后，所有 `/v1/*` 请求必须携带 token（`Authorization: Bearer ...` 或 `x-api-key`）

生成 `KEY_ENC_MASTER_B64`（示例）：

```bash
python3 - <<'PY'
import os,base64
print(base64.b64encode(os.urandom(32)).decode())
PY
```

## 启动

### 方式 A：Docker（推荐）

```bash
docker compose up -d --build
```

### 方式 B：直接运行

```bash
go run ./cmd/gateway
```

## 通过面板配置

打开：`http(s)://<host>:<port>/admin/`

配置顺序建议：
1. Providers：创建 `anthropic/openai/gemini` 三类 provider，设置 base_url
2. Credentials：为每个 provider 添加一个或多个 key（会加密落库）
3. Channels：为 credential 建 channel，可选设置 model_map_json / extra_headers_json
4. Pools：把 channel 组为池（建议先建一个名为 `default` 的 pool）
5. Rules：按 priority 写匹配规则，把模型/门面请求分流到不同 pool

## Claude Code 接入

Claude Code 通过 `ANTHROPIC_BASE_URL` 指向网关域名即可（客户端会请求 `/v1/messages`）。

```bash
export ANTHROPIC_BASE_URL="https://your-gateway.example.com"
export ANTHROPIC_MODEL="claude-sonnet-4-5"
export ANTHROPIC_AUTH_TOKEN="$CLIENT_TOKEN"  # 如果你启用了 CLIENT_TOKEN
```

## OpenAI 客户端接入

把客户端 base URL 指向网关根地址，调用 `/v1/chat/completions`。

