# SlopGuard 宿主 Agent 使用指南

SlopGuard 是确定性的本地代码质量护栏。二进制本身**不调用任何 LLM**；语义提炼、规则草拟和用户沟通由宿主 Agent 完成，SlopGuard 只负责检测、沙箱验证、持久化和规则落盘。

## 启动 MCP Server

在目标项目根目录运行：

```bash
slopguard mcp
```

MCP 使用 stdio，传输为**换行分隔的 JSON-RPC 2.0**：每行一个 JSON 对象。`stdout` 只输出协议帧，日志和警告全部走 `stderr`；不要从 `stdout` 读取普通文本日志。

## MCP 工具

| 工具 | 参数 | 用途 |
| --- | --- | --- |
| `sg_verify` | `{ all?: bool, paths?: string[], ui?: bool }` | 执行确定性门禁，返回 `contract.VerifyResult`；`ui: true` 追加 UI 烟雾验证 |
| `sg_radar` | `{ top?: number }` | 返回 Top N 坏样本热点与降级明细 |
| `sg_record_mistake` | `{ category, summary, bad_snippet?, good_snippet? }` | 记录一次 AI 错题；相同指纹累计计数 |
| `sg_list_candidates` | `{ min_count?: number }` | 列出累计次数达到阈值的待提炼错题 |
| `sg_list_inbox` | `{ project?: string, review_status?: string }` | 查看待分析或待人工校验的错题收件箱（`pending_ai` / `pending_user`） |
| `sg_submit_analysis` | `{ fingerprint, decision: "recommend"\|"skip", reason, rule_draft? }` | 提交通俗中文分析理由与入库建议，供用户在看板「错题栏」校验确认 |
| `sg_dry_run_rule` | `{ engine, rule_id?, rule_payload, sample_bad?, files?, threshold? }` | 在临时配置中试跑候选规则，绝不修改项目配置 |
| `sg_apply_rule` | `{ engine, rule_id?, rule_payload, scope: "project"\|"global", rationale?, confirm: true }` | 用户确认后写入项目配置或全局模板，并记录规则台账 |

## 标准工作流

1. **改完代码先验证**：调用 `sg_verify`。默认只检查 Git 变更文件；需要全量时传 `all: true`，需要定点检查时传 `paths`，需要 UI 烟雾验证时传 `ui: true`（未配置或未装 Playwright 会跳过，不影响 passed）。
2. **顺手分析返回的新错题**：如果 `sg_verify` 返回的 `pending_inbox` 非空，请顺手结合上下文对这些错题调用 `sg_submit_analysis`（给出通俗中文原因、`recommend` 或 `skip` 建议及可选 `rule_draft`），让用户在看板直接看懂并一键确认。
3. **失败先修复**：读取 `findings`，修复后再次 `sg_verify`，直到 `passed: true`。
3. **记录重复错误**：若同一类 AI 错误再次出现，调用 `sg_record_mistake`，提供 `category`、`summary`，尽量附上 `bad_snippet` 与 `good_snippet`。相同指纹会累计 `count`。
4. **主动巡检与分析错题收件箱**：定期或当用户要求分析错题时，调用 `sg_list_inbox`（如 `review_status: "pending_ai"`）获取自动捕获或待分析的错误模式；结合上下文判断是真实坏习惯（`recommend`）还是偶发误报（`skip`），用**通俗易懂的中文**说明“为什么建议入库 / 为什么建议忽略”（必要时附带 `rule_draft` 草案），调用 `sg_submit_analysis` 推送到看板「错题栏」等待用户一键确认。
5. **召回候选**：当同类错误累计达到 2 次，也可调用 `sg_list_candidates` 获取待提炼错题簇。
6. **草拟规则**：宿主 Agent 根据错题选择合适的载体（ESLint、Ruff 或 Semgrep），生成 `rule_payload`。
7. **必须先沙箱试跑**：调用 `sg_dry_run_rule`，检查 `hits`、`sample_files`、`sample_matched`、`warning` 与 `duration_ms`。命中 0 处、`sample_matched=false` 或命中数超过阈值（默认 20）时，必须回到规则设计，不得直接落盘。
8. **用户确认后才写入**：把规则内容、试跑报告和将修改的配置 Diff 展示给用户；只有用户明确确认后，才调用 `sg_apply_rule` 且必须传 `confirm: true`。`scope: "project"` 写项目配置，`scope: "global"` 写 `~/.slopguard/presets/` 模板。
9. **写入后复验**：再次调用 `sg_verify`，确认规则生效且没有误杀。

## CLI 等价入口

- `slopguard record --summary "..." [--category ...] [--bad-snippet ...] [--good-snippet ...]`：人工或脚本录入错题。
- `slopguard verify [--all] [--json] [--ui]`：本地门禁；`--ui` 追加 Playwright UI 烟雾验证。
- `slopguard radar [--top 3] [--json]`：坏样本雷达。
- `slopguard ui [--port 7788] [--no-open]`：启动或唤起本地可视化看板（仅 127.0.0.1；若后台服务已运行则直接打开独立窗口）。
- `slopguard service install|uninstall|status [--port 7788]`：管理 macOS LaunchAgent 后台常驻看板服务。

## 约束

- 不要直接修改项目正式配置来“试跑”规则；必须使用 `sg_dry_run_rule`。
- `sg_apply_rule` 的 `confirm: true` 只能代表用户已经明确确认；Agent 不得自行推断确认。
- SlopGuard 是确定性工具，不负责生成规则语义；规则内容由宿主 Agent 负责。
