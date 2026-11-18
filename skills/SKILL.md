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
| `sg_dry_run_rule` | `{ engine, rule_id?, rule_payload, sample_bad?, files?, threshold? }` | 在临时配置中试跑候选规则，绝不修改项目配置 |
| `sg_apply_rule` | `{ engine, rule_id?, rule_payload, scope: "project"\|"global", rationale?, confirm: true }` | 用户确认后写入项目配置或全局模板，并记录规则台账 |

## 标准工作流

1. **改完代码先验证**：调用 `sg_verify`。默认只检查 Git 变更文件；需要全量时传 `all: true`，需要定点检查时传 `paths`，需要 UI 烟雾验证时传 `ui: true`（未配置或未装 Playwright 会跳过，不影响 passed）。
2. **失败先修复**：读取 `findings`，修复后再次 `sg_verify`，直到 `passed: true`。
3. **记录重复错误**：若同一类 AI 错误再次出现，调用 `sg_record_mistake`，提供 `category`、`summary`，尽量附上 `bad_snippet` 与 `good_snippet`。相同指纹会累计 `count`。
4. **召回候选**：当同类错误累计达到 2 次，调用 `sg_list_candidates` 获取待提炼错题簇。
5. **草拟规则**：宿主 Agent 根据错题选择合适的载体（ESLint、Ruff 或 Semgrep），生成 `rule_payload`。
6. **必须先沙箱试跑**：调用 `sg_dry_run_rule`，检查 `hits`、`sample_files`、`sample_matched`、`warning` 与 `duration_ms`。命中 0 处、`sample_matched=false` 或命中数超过阈值（默认 20）时，必须回到规则设计，不得直接落盘。
7. **用户确认后才写入**：把规则内容、试跑报告和将修改的配置 Diff 展示给用户；只有用户明确确认后，才调用 `sg_apply_rule` 且必须传 `confirm: true`。`scope: "project"` 写项目配置，`scope: "global"` 写 `~/.slopguard/presets/` 模板。
8. **写入后复验**：再次调用 `sg_verify`，确认规则生效且没有误杀。

## CLI 等价入口

- `slopguard record --summary "..." [--category ...] [--bad-snippet ...] [--good-snippet ...]`：人工或脚本录入错题。
- `slopguard verify [--all] [--json] [--ui]`：本地门禁；`--ui` 追加 Playwright UI 烟雾验证。
- `slopguard radar [--top 3] [--json]`：坏样本雷达。

## 约束

- 不要直接修改项目正式配置来“试跑”规则；必须使用 `sg_dry_run_rule`。
- `sg_apply_rule` 的 `confirm: true` 只能代表用户已经明确确认；Agent 不得自行推断确认。
- SlopGuard 是确定性工具，不负责生成规则语义；规则内容由宿主 Agent 负责。
