## 现状理解（基于当前代码）
- Admin UI 目前只有 Pools/Providers/Logs 三个页签，核心是 CRUD + JSON 文本编辑（default_headers_json / model_map_json）。
- 网关路由的模型映射生效顺序是：Provider 映射先应用，Pool 映射后应用（可覆盖）。
- Admin 后端缺少“测试 Key/渠道”“按供应商拉取模型列表”等端点；Logs 的 SSE 与历史日志已具备。

## 目标（你提出的“不达标点”对齐）
- 面板内可对“渠道池 client_key / 凭据 key”进行一键测试与批量自动测试，并记录结果。
- 供应商支持“命名 + 分组/分类”，列表可按组折叠/筛选，信息更直观。
- 每个供应商的模型列表：支持手动快速配置 + 通过 API 拉取刷新 + 在映射编辑器里可搜索选择。
- 模型映射改为结构化编辑器（表格/行编辑），并能清晰展示“最终生效映射”（Provider 默认 vs Pool 覆盖）。

## 后端与数据层改造
### 1）DB：补齐供应商分组/命名、测试结果、模型缓存
- providers 表新增：display_name（显示名）、group_name（分组/分类）、notes（可选说明）、models_json（缓存模型列表，可选）、models_refreshed_at。
- credentials 表新增：last_test_at、last_test_ok、last_test_status、last_test_latency_ms、last_test_error（不包含敏感信息）、last_test_model（测试用模型）。
- 可选新增 provider_models 表（如果 models_json 不够用）：provider_id + model_id + enabled + tags + updated_at，用于 UI 勾选与快速配置。
- 校验并修正 migrations 与“真实表结构”可能不一致的问题：先读取当前数据库 schema，再生成兼容迁移（避免重复列/失败）。

### 2）Admin API：增加“测试/模型拉取/映射工具化”端点
- 渠道池测试：POST /admin/api/pools/{id}/test
  - 入参：facade(openai/anthropic)、model、stream(false)、timeout、mode(health/models/chat)
  - 行为：使用该 pool 的 client_key 做一次端到端调用（优先本机回环 HTTP 调用网关自身，确保鉴权/路由/转换全链路生效）。
- 凭据测试：POST /admin/api/credentials/{id}/test
  - 行为：直接对该 credential 对应上游发起最小探测：
    - openai 类型：优先 GET /v1/models；失败则 POST /v1/chat/completions（max_tokens=1）
    - anthropic 类型：POST /v1/messages（max_tokens=1）
    - gemini 类型：执行最小 generateContent（如已支持）
  - 结果：返回 status/latency/error（不含 key），并写入 credentials 的 last_test_* 字段。
- 批量自动测试：POST /admin/api/providers/{id}/credentials/test（或 /admin/api/credentials/test?provider_id=）
  - 支持并发上限与节流，返回汇总统计。
- 供应商模型拉取：POST /admin/api/providers/{id}/models/refresh
  - 入参：credential_id（用于鉴权访问上游 models）
  - 行为：按 provider type 调用上游 models endpoint（openai: /v1/models；anthropic: /v1/models 若可用，否则返回“仅支持手动配置”），写入 models_json/provider_models。
- 获取模型列表：GET /admin/api/providers/{id}/models
  - 用于 UI 下拉选择与搜索。

### 3）实现细节（严谨性与安全）
- 不记录/回传任何明文 key；日志与测试结果只含 last4 或 credential_id。
- 测试请求 body 固定最小化，避免消耗；支持超时。
- 对外部 URL（图片、多模态）保持 https/data-url 安全约束；测试时不做未知协议请求。

## 前端（Admin UI）改造
### 1）Providers 列表：分组/命名 + 快捷动作
- Providers 列表改为“按 group_name 分组折叠”，每行展示：display_name（主标题）+ type + base_url（次要）。
- 增加快捷按钮：
  - “拉取模型”
  - “一键测试全部 Key”
  - “查看/编辑模型映射”

### 2）Credentials 管理：测试面板 + 自动测试
- 在供应商的“凭据弹窗/面板”中：
  - 每条 credential 增加“测试”按钮与状态徽章（最近一次测试：成功/失败、耗时、时间）。
  - 增加“批量测试”“失败重测”。
  - 新增/批量导入后可选“自动测试”（开关）。

### 3）模型列表与模型映射：结构化编辑器
- 现有 JSON 文本框改为双视图：
  - 结构化表格视图（默认）：每行【请求模型】→【上游模型】；支持新增/删除/复制、排序、搜索、冲突提示。
  - 原始 JSON 视图（高级）：可手动编辑，保存前校验。
- 映射编辑支持“从模型列表选择”：
  - 上游模型下拉来自 provider models（手动/拉取）。
  - 请求模型可提供常用模板与快速添加。
- “最终生效映射预览”：展示 provider 默认 + pool 覆盖后的结果，并高亮覆盖项。

### 4）渠道池（Pools）：渠道测试
- Pools 卡片增加“测试渠道”按钮：选择 facade/model/模式后一键测试，并显示结果（成功/失败、选中的 credential/provider、上游模型）。

## 测试与验收
- 单元测试：覆盖新增的 convert/校验函数与 models 解析。
- 集成测试：对新增 admin endpoints 的 handler 做 HTTP 测试（含鉴权、错误分支、并发节流）。
- 手工验收路径：
  - 新建供应商→导入 key→自动测试→拉取模型→编辑映射→渠道池测试→通过 Logs 验证 req_model→up_model。

## 交付物（最终你会得到）
- Admin 面板新增：渠道测试、Key 测试/批测、供应商分组命名、模型拉取与选择、清晰的映射编辑器与生效预览。
- 数据库与 API 完整支持上述能力，并保持向后兼容（原有 model_map_json 仍可工作）。

如果确认该方案，我将按上述顺序开始实现（先 DB+API，再 UI，再测试与验收）。