## 现状结论（基于代码与表结构）
- 本项目的数据模型其实已经是“供应商(Provider) → 多个凭据(Credential/API Key) → 渠道(Channel) → 渠道池(Pool) → 路由规则(Rule)”的层级；问题主要在管理台把它拆成多个 Tab，导致大量 Key 必须逐条管理，且模型映射放在 Channel 上不利于“按供应商统一配置”。
- 目前 Provider 已支持配置端点（base_url）与默认请求头（default_headers_json）；模型映射目前只在 Channel（model_map_json）上。

## 目标改造（按你的描述对齐）
- “APICenter 多渠道 + 每渠道大量 key”：在本网关里把每个“渠道/上游实例”落为一个 Provider；其下挂多条 Credential（大量 key 一排一排管理）。
- “凭据合并到供应商后面”：管理台以 Provider 为主视图，展开/进入详情即可管理该 Provider 下所有 key。
- “配置供应商模型端点与映射规则”：Provider 级支持配置 base_url（已有）+ provider 级模型映射规则（新增），并保留/支持 key 级（channel）覆盖（可选）。
- “从 APICenter 迁移已有 key 和端点”：提供数据库探测 + 一键迁移脚本/命令，把旧表内容导入到 providers/credentials（必要时自动创建最小可用 pools/rules）。

## 数据库与路由层调整
- 新增 Provider 级配置字段（通过新增 migration SQL）：
  - `providers.model_map_json JSON NULL`（供应商默认模型映射）
  - （可选）`providers.extra_headers_json JSON NULL`（供应商额外请求头；与 default_headers 的定位区分为“固定头 vs 可变头”，也可以只复用 default_headers）
- 路由逻辑调整：
  - 组装 headers 时：Provider 默认头 +（可选 Provider extra）+ Channel extra（保持现有覆盖行为）。
  - 计算上游 model 时：先应用 Provider model_map，再应用 Channel model_map（Channel 可覆盖 Provider）。

## Admin API 调整（兼容旧接口）
- 扩展 `GET/POST/PUT /admin/api/providers`：支持读写 provider 级 `model_map_json`（以及可选 `extra_headers_json`）。
- 为大批量 Key 提供更高效接口（二选一，倾向于更简单的 query 方式）：
  - 方案 A：`GET /admin/api/credentials?provider_id=...` + `POST /admin/api/credentials/bulk`（一次提交多条 key）。
  - 方案 B：`GET/POST /admin/api/providers/{id}/credentials`（更语义化）。
- 仍保留现有 `/admin/api/credentials` 的单条 CRUD，避免破坏兼容。

## 管理台 UI（index.html）重构
- Tab 简化与重排：
  - 把“凭据管理”并入“供应商”Tab（保留其它 tabs，但降低其日常配置成本）。
- Provider 主列表升级：
  - 列表行支持“展开详情/进入详情”，详情区包含：
    - Provider 基础：type、base_url、默认头、启用状态
    - Provider 级模型映射 JSON 编辑器（textarea + JSON 校验）
    - “Key 列表”表格（Credential）：显示 name、last4、weight、concurrency、enabled，支持批量启用/禁用、批量删除（可选）
    - “批量导入 Key”：支持粘贴多行（每行一个 key；可选 name,key,weight,conc），一键导入
- 让配置更“项目化/快捷”：
  - 在 Provider 详情里加“快速初始化路由”按钮：没有配置时自动创建 default pool + 一条兜底 rule（match={})，把该 provider 下的 key（或默认 channel）全部纳入。

## 从 APICenter 迁移（数据库探测 + 导入）
- 先做“探测脚本”：新增 `scripts/inspect_db.py`（或 Go cmd），读取项目/环境中的 `MYSQL_DSN`，列出：
  - databases、tables、关键字段（information_schema）
  - 自动识别可能的 APICenter 相关表（按表名/字段名特征：channel/provider/key/base_url 等）
- 再做“迁移命令”：新增 `cmd/migrate_apicenter`（或 `scripts/migrate_apicenter.py`）：
  - 从旧表提取：渠道名、endpoint/base_url、供应商类型、keys、权重/限流（如果有）、模型映射/规则（如果有）
  - 写入新表：providers/credentials
  - 写入方式优先走 Admin API 或复用同一套 AESGCM 加密逻辑（确保历史 key 在库里都是加密 blob）
  - 迁移完成后自动补齐最小可运行配置（default pool + 默认 rule），避免“导完但不可用”。

## 验证与回归
- 本地启动后验证：
  - 管理台能按 Provider 展示/批量导入 key
  - 路由能正确应用 provider 级 model_map，并且 channel 级可覆盖
  - 迁移命令在目标库上可重复执行（幂等：按 type+base_url、provider_id+name 或 key_last4+name 做去重策略）

如果你确认这个方向，我会按以上步骤开始改：先落 DB migration + 路由支持 provider 级映射，再改 Admin API，最后重构管理台并补上 DB 探测/迁移脚本。