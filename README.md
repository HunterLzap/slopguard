# SlopGuard

SlopGuard 是一个纯 Go 的 AI 代码质量护栏：一条 `verify` 命令完成类型检查、Lint、逃生舱棘轮比对，并提供 MCP 工具让宿主 Agent 把反复出现的 AI 错误沉淀成规则。

- **零 CGO**：`modernc.org/sqlite` 纯 Go 实现，单二进制可交叉编译到 Windows / Linux / macOS。
- **零内置 LLM**：二进制完全确定性执行，不调用任何模型；语义提炼交给宿主 Agent。
- **低侵入**：不修改宿主项目的 `package.json`，规则只写入项目已有的 ESLint / Ruff / Semgrep 配置。

## 安装

需要 Go 1.23+：

```bash
go install github.com/HunterLzap/slopguard/cmd/slopguard@latest
```

或从源码构建：

```bash
git clone https://github.com/HunterLzap/slopguard.git
cd slopguard
go build -o slopguard ./cmd/slopguard
```

也可以直接下载各平台预编译二进制，无需安装 Go 运行时。

## 快速上手

```bash
cd your-project
slopguard init          # 生成 .slopguard/config.yaml 与 baseline.json
slopguard verify        # 日常门禁：只检查 Git 变更文件
slopguard verify --all  # 首次全量体检
```

`init` 会识别 TypeScript / Python 栈并探测可用适配器；已有配置不会被覆盖，重跑会先打印 Diff，只有 `--yes` 或交互确认才会应用。

想让新项目继承全局沉淀：

```bash
slopguard init --presets   # 合并 ~/.slopguard/presets/ 下的通用规则模板
slopguard init --ui        # 生成 .slopguard/ui.yaml（Playwright 烟雾验证配置）
```

## CLI 命令

| 命令 | 说明 |
| --- | --- |
| `slopguard init [--yes] [--presets] [--ui] [--root DIR]` | 初始化项目配置与基线；`--presets` 非破坏合并全局规则模板，`--ui` 生成 UI 烟雾配置 |
| `slopguard verify [--all] [--json] [--jobs N] [--record] [--ui] [--root DIR]` | 运行门禁；默认只查 Git 变更文件，`--all` 全量，`--json` 输出 `contract.VerifyResult`，`--record` 记录扫描历史，`--ui` 追加 UI 烟雾验证（默认关闭） |
| `slopguard baseline [--tighten \| --reset] [--yes] [--root DIR]` | 查看 / 收紧 / 重置逃生舱基线；操作前打印 Diff 并确认 |
| `slopguard radar [--top N] [--json] [--root DIR]` | 输出坏样本热点与传染风险分 TOP N |
| `slopguard record --summary TEXT [--category C] [--bad-snippet S] [--good-snippet S] [--project DIR]` | 录入一次 AI 错题，重复指纹累计次数 |
| `slopguard mcp [--root DIR]` | 以 stdio 模式启动 MCP Server |

退出码：`0` 通过，`1` 检查失败，`2` 工具链缺失或系统错误。`--json` 的输出结构与 `contract.VerifyResult` 对齐。

## MCP 接入

`slopguard mcp` 使用换行分隔的 JSON-RPC 2.0，`stdout` 只输出协议帧，日志全部走 `stderr`。

Claude Code（`.mcp.json` 或用户级配置）：

```json
{
  "mcpServers": {
    "slopguard": {
      "command": "slopguard",
      "args": ["mcp", "--root", "/absolute/path/to/project"]
    }
  }
}
```

Cursor（`.cursor/mcp.json`）配置格式相同。Windows 下 `command` 写成 `slopguard.exe` 或绝对路径。

暴露的 6 个工具：

| 工具 | 用途 |
| --- | --- |
| `sg_verify` | 运行确定性门禁，返回 findings 与基线对比；可选 `ui: true` 追加 UI 烟雾验证 |
| `sg_radar` | 返回坏样本热点 Top N |
| `sg_record_mistake` | 录入 AI 错题，按指纹累计 |
| `sg_list_candidates` | 列出累计次数达到阈值的待提炼错题 |
| `sg_dry_run_rule` | 在隔离临时配置中试跑候选规则 |
| `sg_apply_rule` | 用户确认后写入项目配置或全局模板并记录台账 |

配套宿主 Agent 工作流见 [skills/SKILL.md](skills/SKILL.md)：`sg_verify → sg_record_mistake → sg_list_candidates → sg_dry_run_rule → 用户确认 → sg_apply_rule`。

## 全局规则与适配器

- `~/.slopguard/presets/<engine>/rules.json`：全局规则模板（engine 为 `eslint` / `ruff` / `semgrep`），`slopguard init --presets` 非破坏性合并进项目。
- `~/.slopguard/adapters/*.yaml`：全局声明式适配器；项目 `.slopguard/adapters/` 中的同名适配器优先覆盖全局。
- `~/.slopguard/slopguard.db`：SQLite 错题、规则台账与扫描历史。

自定义 YAML 适配器示例：

```yaml
name: go-standard
detect:
  files_any: ["go.mod"]
extensions: [".go"]
checks:
  - id: vet
    cmd: ["go", "vet", "./..."]
    timeout_sec: 30
  - id: lint
    cmd: ["golangci-lint", "run", "--new-from-rev=HEAD~1"]
    optional: true
    timeout_sec: 60
escape_patterns:
  - name: nolint
    regex: "//nolint"
```

## UI 烟雾验证

`slopguard init --ui` 会在 `.slopguard/ui.yaml` 生成最小配置（`base_url`、`routes`、`screenshot_dir`、`timeout_sec`）；Playwright 烟雾模板内置在二进制中，仓库内也提供了 `presets/ui/` 模板说明。安装 Playwright 后：

```bash
slopguard verify --ui
```

该开关默认关闭以保持秒级门禁；项目未配置 UI 或未安装 Playwright 时会优雅跳过，不影响常规 `verify` 退出码。
