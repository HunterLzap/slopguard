# SlopGuard 宿主 Agent 操作契约

SlopGuard 是本地确定性代码门禁。二进制本身**不调用 LLM**，由宿主 Agent 负责编码修复与规则提炼。

## 硬性门禁契约（必须遵守）

1. **改动收尾必跑验证**：完成代码改动前必须执行 `slopguard verify`（或 MCP `sg_verify`）直到退出码为 `0`（`passed: true`）。
2. **修复 Bug 必跑消融**：修复 Bug 时必须执行 `slopguard verify --ablation` 确认测试经历 `RED -> GREEN` 消融通过。
3. **禁止绕过或敷衍门禁**：
   - 严禁使用 `git commit --no-verify` 跳过 `pre-commit` 卡口。
   - 严禁通过新增抑制注释（`@ts-ignore`、`eslint-disable`、`# type: ignore`、`# noqa` 等）绕过棘轮检查。
   - 严禁在 diff 中添加临时兜底或不可执行约束注释（如 `workaround`、`temp hack`、`do not remove`）；若命中 `probe:*` 的 `action_hint`，必须按提示做最小根因修复或沉淀为可执行类型/断言/测试/Lint 规则后删掉注释。

## 常用 CLI 命令

- `slopguard verify [--staged] [--ablation] [--all] [--json]`：默认增量检查变更并运行 diff 棘轮与注释探针；`--staged` 仅查暂存区；`--ablation` 执行隔离 worktree 消融验证。
- `slopguard hook install [--pre-commit]` / `uninstall`：安装/卸载阻塞式 `pre-commit` 提交卡口。
- `slopguard baseline --tighten --yes`：逃生舱净减少后收紧基线。
- `slopguard mcp`：以 stdio JSON-RPC 2.0 启动 MCP 服务（`stdout` 仅输出协议帧）。

## MCP 工具与错题沉淀闭环

| 工具 | 核心参数 | 用途 |
| --- | --- | --- |
| `sg_verify` | `{ all?, paths?, ui? }` | 执行确定性门禁，并返回 `pending_inbox` 待分析错题 |
| `sg_radar` | `{ top? }` | 扫描坏样本热点文件 |
| `sg_record_mistake` | `{ category, summary, bad_snippet?, good_snippet? }` | 记录错题，同构指纹自动聚簇计数 |
| `sg_list_inbox` | `{ review_status? }` | 获取 `pending_ai` / `pending_user` 收件箱 |
| `sg_submit_analysis` | `{ fingerprint, decision, reason, rule_draft? }` | 提交中文分析建议供用户在看板一键确认 |
| `sg_dry_run_rule` | `{ engine, rule_id?, rule_payload, sample_bad?, files? }` | 沙箱试跑规则草案，绝不改正式配置 |
| `sg_apply_rule` | `{ engine, rule_id?, rule_payload, scope, confirm: true }` | 经用户明确确认后落盘规则 |

规则提炼闭环：`sg_verify` / `sg_list_inbox` 发现高频错题 -> `sg_dry_run_rule` 沙箱试跑（命中 >0 且未超阈值）-> 向用户展示 diff 并获明确同意后 `sg_apply_rule(confirm: true)` -> 复跑 `slopguard verify` 确认通过。
