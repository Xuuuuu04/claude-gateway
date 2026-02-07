# Claude Gateway（多厂商模型网关）- 项目体检

最后复核：2026-02-05

## 状态
- 状态标签：active
- 定位：把 Claude Code / OpenAI 客户端请求统一接入一个网关，完成多厂商转换、路由、负载与最小化运维面板。

## 架构速览
- 入口：`cmd/gateway/main.go`
- HTTP 框架：`chi` + `cors`
- 对外接口：
  - `GET /healthz`：健康检查
  - `GET /metrics`：Prometheus 指标
  - `POST /v1/messages`：Anthropic Messages facade
  - `POST /v1/chat/completions` 等：OpenAI facade
  - `/admin/*`：配置与观测面板 + 测试 API
- 核心模块：
  - `internal/router/`：路由与选路（pool/tier/provider/credential），含路由缓存与失败剔除策略
  - `internal/providers/`：上游 provider 代理（anthropic/openai/gemini）
  - `internal/convert/` + `internal/streamconv/`：协议与流式（SSE）兼容转换
  - `internal/db/`：MySQL 打开与迁移
  - `internal/crypto/`：上游 Key 加密（AES-GCM，主密钥来自 `KEY_ENC_MASTER_B64`）
  - `internal/logbus/`：请求事件落库（请求/响应字节、tokens、TTFT、TPS、错误等）
  - `internal/metrics/`：延迟/状态码等指标

## 运行与部署
- 本地运行：`go run ./cmd/gateway`
- Docker：`docker compose up -d --build`
- 关键环境变量：见 `docs/DEPLOY.md`

## 当前实现亮点
- 同时提供 Anthropic 与 OpenAI facade，支持多 provider 统一管理。
- 路由层具备缓存与剔除：失败、401/403、429、5xx 会触发路由缓存失效/换路由尝试。
- 事件总线（logbus）与 metrics 具备“平台化”的可观测基础。
- 对上游 Key 做加密落库，避免明文存储。

## 风险与建议（优先级）
- 建议将仓库中的二进制文件 `gateway` 移出版本控制（或加入 `.gitignore`），避免提交大体积构建产物。
- `/.local/` 存在本地 MariaDB 数据目录，建议加入 `.gitignore` 并避免提交（仓库体积与隐私风险）。
- 文档与配置建议补齐：
  - “默认路由规则/模型映射”的最小例子（从 0 配到可用的 5 步脚本化流程）
  - 常见错误码与排障（上游 401/429、SSE 中断、MySQL 迁移失败）

## 下一步可做（按你的目标方向）
- 平台产品化：补一个“租户/配额/预算”最小实现草案（哪怕先做字段 + UI + 只读统计）。
- 质量与回归：为 `convert/streamconv` 增加 golden tests（输入→输出快照），保证兼容层不回归。

