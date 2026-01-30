# 个人多厂商模型网关（Claude Code 兼容）

## 目标与边界
- 目标：做一个“统一入口”的 LLM Gateway，前端面板可配置上游厂商（Anthropic/OpenAI/Gemini）、API Key、模型映射、负载策略与分流规则；客户端（重点 Claude Code）只需把 Base URL 指向该网关，不再频繁改本地配置。
- 边界：不做付费/计费/多用户体系；只做单人可用的“配置 + 路由 + 观测 + 稳定性”。

## Claude Code 兼容方式（关键约束）
- 兼容点：Claude Code 支持通过 `ANTHROPIC_BASE_URL` 改指向自建服务，并通过 `ANTHROPIC_AUTH_TOKEN/ANTHROPIC_API_KEY` 与 `ANTHROPIC_MODEL` 指定模型（社区/云厂商文档有明确示例）。
  - 参考：302.ai 帮助中心与阿里云百炼的 Claude Code 接入说明（均以 Anthropic 兼容 API + Base URL 覆盖为核心）。
- 协议要求：对 `/v1/messages` 的 SSE 流式事件格式要与 Anthropic Messages Streaming 兼容（`message_start/content_block_* / message_delta / message_stop/ping` 等事件）。
  - 参考：Anthropic 官方“Streaming Messages”文档。

## 总体架构
- **gateway-api**：HTTP 服务，提供三套“门面 API”（facade），但内部统一为一个“规范化请求/响应（Canonical）”进行路由与转换。
  - Anthropic 兼容：`POST /v1/messages`（含 stream），`GET /v1/models`（最少实现）
  - OpenAI 兼容：`POST /v1/chat/completions`（含 stream），可选 `POST /v1/responses`
  - 管理接口：`/admin/*`（面板与管理 API，不对外公开）
- **router/policy-engine**：根据“路由规则 + 池策略 + 健康状态”选出目标 channel。
- **provider-connectors**：Anthropic/OpenAI/Gemini 三个上游连接器，负责：签名/headers、请求格式、流式解析、错误映射。
- **mysql**：存储配置（厂商/凭据/channel/pool/规则）与日志（请求、错误、健康、审计）。
- **panel（轻量）**：单人面板，用于 CRUD 配置、查看实时日志与健康。

## 统一规范（Canonical）设计
- Canonical 请求：
  - messages（role/content），system，tools/tool_choice，temperature/top_p，max_tokens，stream
  - attachments（图片/文件的抽象占位；先对 Claude Code 必需项做“可透传/可忽略”策略）
  - routing-hints（从 header/metadata 提取的标签、优先级）
- Canonical 响应：
  - 非流：统一为“最终文本/多段 content blocks + usage + stop_reason + tool_calls”
  - 流式：内部统一为“增量 token/text delta + tool delta + usage delta”，再转换为 Anthropic SSE 或 OpenAI SSE。

## 关键功能设计（按你强调的“简明但强”）

### 1) 多配置注册与模型映射
- Provider（厂商实例）：type=anthropic/openai/gemini，base_url（可自建/中转），默认 headers（如 anthropic-version）。
- Credential（凭据）：绑定 provider；加密存储 api_key；可设置权重、启用/禁用、并发上限、RPM/TPM 上限。
- Channel（可路由单元）：credential + “上游模型名映射 + 额外 headers + tags”。
- Model Alias：把 Claude Code 的 `ANTHROPIC_MODEL`（含自定义字符串）映射到内部模型（例如 `sonnet` → 某个 pool）。

### 2) 渠道队列与负载策略
- Pool（渠道池）：一组 channel + 选择策略：
  - round_robin / weighted_rr / least_inflight / latency_ewma / priority_failover
- 失败处理：
  - 连接/超时/5xx 自动重试（幂等条件下），并在 pool 内“切换 channel”。
  - Circuit Breaker：连续失败 N 次 → 冷却 T 秒；冷却后探活恢复。
- 队列：
  - 全局并发上限 + per-pool/per-channel 并发上限。
  - 超限返回 429（并映射为对应门面格式）。

### 3) “请求梯度 / 分流规则”
- RoutingRule：按优先级匹配，条件包含：
  - facade（anthropic/openai）、模型名/正则、是否 stream、是否包含 tools、max_tokens/输入长度阈值、header 标签（如 `x-route: cheap`）。
- 动作：选定 pool + fallback pool 链（梯度）：
  - 例：小请求→cheap pool；大上下文/工具调用→quality pool；失败→fallback pool。

### 4) 日志实时监控与可观测性
- 请求日志（可选采样）：记录 request_id、客户端门面、模型、选中 channel、耗时、输入/输出 tokens（若可得）、错误类型。
- 实时查看：面板提供 SSE/WebSocket 订阅“最新日志事件”（来自内存 ring buffer + 落库异步）。
- 指标：
  - Prometheus：QPS、latency、错误率、每 channel inflight、熔断状态、429 计数。

### 5) 错误管理与格式互转
- 内部统一错误分类：auth / rate_limit / invalid_request / context_limit / upstream_timeout / upstream_5xx / unknown。
- 对外渲染：
  - Anthropic 门面返回 Anthropic error schema；流式中也要正确终止 SSE。
  - OpenAI 门面返回 OpenAI error schema。

## MySQL 表结构（最小可用，后续可扩展）
- providers(id, type, base_url, default_headers_json, enabled)
- credentials(id, provider_id, name, api_key_ciphertext, key_last4, weight, rpm_limit, tpm_limit, concurrency_limit, enabled)
- channels(id, credential_id, name, tags_json, model_map_json, extra_headers_json, enabled)
- pools(id, name, strategy, channel_ids_json, enabled)
- routing_rules(id, priority, match_json, pool_id, fallback_pool_id, enabled)
- request_logs(id, ts, request_id, facade, req_model, pool_id, channel_id, status, latency_ms, input_tokens, output_tokens, error_type, error_message_hash)
- channel_health(id, ts, channel_id, ok, latency_ms, last_error_type)

## 安全设计（单人但必须稳）
- 不把任何 key/数据库密码写进代码或提交；统一从环境变量或运行时写入。
- api_key 加密落库（AES-GCM），主密钥放环境变量；面板只显示 last4。
- 管理端 `/admin` 使用单一 admin token（Header 或 Basic Auth）+ 可选 IP allowlist。
- 日志默认不存 prompt 全文；只存统计与必要字段（可选“调试开关”）。

## 技术选型（建议）
- 后端：Go（net/http + SSE 转发稳定，资源占用低）。
- 前端：Vite + React/Vue 任一，最终编译为静态文件由后端托管。
- 依赖：MySQL 驱动、迁移工具、Prometheus client。

## 验证方式（交付必须可用）
- 使用 curl 跑通：
  - Anthropic 非流/流式 `/v1/messages`
  - OpenAI 非流/流式 `/v1/chat/completions`
- 用 Claude Code 实测：配置 `ANTHROPIC_BASE_URL` 指向网关，`ANTHROPIC_MODEL` 选择你在面板配置的 alias/pool。

## 实施步骤（确认后开始写代码）
1. 初始化项目骨架（gateway-api + admin-api + 面板静态托管），打通 MySQL 迁移。
2. 实现 Canonical schema + Anthropic 门面（含 SSE）+ Anthropic 上游连接器。
3. 实现 OpenAI 门面与连接器，并完成 Canonical ↔ OpenAI 的互转（含工具调用/流式）。
4. 实现 Gemini 连接器（generateContent/streamGenerateContent）并接入 Canonical。
5. 实现路由规则、pool 策略、健康检查、熔断与重试。
6. 实现面板：Provider/Credential/Channel/Pool/Rule 管理 + 实时日志视图。
7. 补齐错误映射与指标，完成端到端测试与部署文档（Docker Compose）。

---

确认后我会开始落地代码与数据库迁移。

附：Claude Code Base URL 覆盖与 Anthropic SSE 事件格式的依据来源：
- Anthropic Streaming Messages 文档：https://docs.anthropic.com/en/api/messages-streaming
- Claude Code 通过 ANTHROPIC_BASE_URL 接入的示例（第三方/云厂商文档）：
  - https://help.aliyun.com/zh/model-studio/claude-code
  - https://help.302.ai/docs/jie-ru-dao-Claude-Code
