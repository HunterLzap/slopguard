# SlopGuard 整体项目规划与架构设计

> 定位：面向个人开发者的轻量级 **AI 代码质量护栏与工程记忆系统**。  
> 核心理念：**确定性检测在本地极速完成，语义提炼完全交由宿主 AI Agent，规则写入与重构由人最终审批。**

---

## 1. 已确定的核心架构决策

| 维度 | 决策 | 设计意图 |
| :--- | :--- | :--- |
| **实现语言** | **Go**（纯 Go，零 CGO） | 单二进制零依赖分发、常驻/峰值内存低（~10–30MB）、启动毫秒级、AI 编写 Go 代码稳定性高 |
| **Adapter 扩展** | **Go 接口 + 声明式 YAML** | 内置 `TS/JS` 与 `Python` 一等公民 Adapter；其他技术栈通过用户/项目 YAML 声明扩展，无需重编译 |
| **LLM 解耦** | **零内置 LLM 调用** | 二进制本身 100% 确定性、无需配置 API Key；错题聚类与规则起草通过 MCP/Skill 交由宿主 Agent 完成 |
| **交互与存储** | **CLI + stdio MCP + 内嵌 SQLite** | CLI 供人/Hook 使用；MCP 供 Agent 原生调用；`modernc.org/sqlite` 存跨项目错题与规则历史，项目内保留可读的 `baseline.json` |
| **资源与验证** | **有界并发 + 增量优先 + 统一 UI 接口** | 限制并发子进程数、流式截断输出、默认只扫 Git 变更文件；UI 验证通过插件接口按需触发 |

---

## 2. 系统分层架构

```text
+-------------------------------------------------------------------+
|                         接入层 (Interfaces)                        |
|   CLI (cobra/kong)      stdio MCP Server       Git Hooks / SKILL   |
+-------------------------------------------------------------------+
                                  │
                                  ▼
+-------------------------------------------------------------------+
|                       编排与调度层 (Core Engine)                    |
|  - Task Scheduler: 有界 Worker 池、超时控制、信号优雅退出 (SIGINT)   |
|  - Diff Resolver : Git 变更集提取、增量文件 hash 缓存                |
|  - Ratchet Engine: 基线比对（存量放行，增量拦截）                     |
|  - Dry-Run Sandbox: 候选规则试跑、误杀率统计                         |
+-------------------------------------------------------------------+
          │                       │                       │
          ▼                       ▼                       ▼
+------------------+   +--------------------+   +-------------------+
|  Adapter 层      |   |  规则与验证载体    |   |  持久化层 (Store)  |
|  - TS/JS (内置)  |   |  - ESLint / tsc    |   |  - ~/.slopguard/  |
|  - Python (内置) |   |  - Ruff / Pyright  |   |    slopguard.db   |
|  - YAML 自定义   |   |  - Semgrep (兜底)  |   |    presets/       |
|  - UI Runner     |   |  - knip / jscpd    |   |  - <repo>/.sg/    |
+------------------+   +--------------------+   +-------------------+
```

---

## 3. 核心工作流设计

### 3.1 `init / doctor`：底座体检与零破坏初始化
1. 扫描目标仓库特征文件（`package.json`、`tsconfig.json`、`pyproject.toml`、`go.mod` 等）及自定义 YAML Adapter。
2. 检查缺失项：严格类型配置、Linter、死代码工具、统一 `verify` 入口、UI 烟雾脚本。
3. **非破坏性生成**：仅生成缺失的 `.slopguard/config.yaml` 与初始 `.slopguard/baseline.json`；若修改已有配置（如 `package.json` 添加 `verify` 脚本），打印 Diff 由用户确认或 `--yes` 应用。

### 3.2 `verify`：极速确定性门禁（日常最高频）
1. **范围解析**：默认调用 `git status --porcelain` + `git diff` 提取变更文件列表；传 `--all` 时全量扫描。
2. **缓存命中**：若文件 `mtime + size`（或 SHA256）未变且配置未变，直接复用上次通过结果。
3. **有界并发执行**：Worker 池同时最多运行 `min(CPU/2, 2)` 个外部子进程（防止 `tsc` + `eslint` 同时拉起导致内存飙升）。
4. **棘轮判定（Ratchet）**：
   - 新增语法/类型/Lint 错误 → **直接失败（Exit 1）**。
   - 逃生舱指标（`any` / `@ts-ignore` / `eslint-disable` / `# type: ignore`）：总数 `<= baseline` 放行；`> baseline` 拦截。
   - 若总数 `< baseline`，提示可运行 `slopguard baseline --tighten` 自动收紧基线（只减不增）。

### 3.3 `radar`：零 LLM 成本坏样本哨兵
1. 扫描项目静态健康指标：逃生舱分布、超长文件/高圈复杂度函数、死代码（`knip` / `vulture`）、重复代码块（`jscpd`）、Git 近 30 天高频 `fix:` 返工文件。
2. 按 **`传染风险分 = 被引用次数 × (复杂度 + 逃生舱数 + 返工次数)`** 排序。
3. 仅输出 **Top 3 最值得清理的目标文件** 及具体原因，避免信息过载。

### 3.4 `record` & `learn`：错题沉淀与规则闭环（与宿主 Agent 协作）
1. **收集阶段**：
   - 用户或宿主 Agent 调用 `record_mistake`（命令行或 MCP），记录：`项目路径`、`错误模式描述`、`错误代码片段`、`正确替代写法`、`来源会话/Commit`。
   - SQLite 对相似错误进行聚合计数。
2. **提炼阶段（由宿主 Agent 执行）**：
   - 当某类错题累计 **≥ 2 次**，宿主 Agent 通过 MCP `list_pending_mistakes` 拉取结构化上下文包，决定采用哪种规则载体（ESLint `no-restricted-syntax` / Ruff `TID251` / Semgrep YAML），并起草规则文本及清晰的 `message`。
3. **沙箱试跑阶段（核心防误杀）**：
   - 宿主 Agent 调用 MCP `dry_run_rule(rule_payload)`。
   - Go 引擎在**内存/临时配置文件**中执行检查（绝不直接改项目正式配置），返回：`新增命中数`、`命中文件样例`、`是否命中了已知错误样本`、`执行耗时`。
   - **拦截异常规则**：若命中数 `== 0`（无效规则）或命中数超过阈值（如 `> 20` 处），标记警告。
4. **审批写入**：
   - 宿主 Agent 向用户展示规则内容 + 试跑报告；用户确认后，调用 `apply_rule` 写入项目配置（并可选 `--global` 同步至 `~/.slopguard/presets/`）。

---

## 4. 存储与扩展 Schema 设计

### 4.1 全局 SQLite (`~/.slopguard/slopguard.db`)
使用纯 Go 驱动 `modernc.org/sqlite`，开启 WAL 模式以支持多终端并发安全读取：

```sql
-- 错题记录表
CREATE TABLE mistakes (
    id           TEXT PRIMARY KEY,
    project_path TEXT NOT NULL,
    fingerprint  TEXT NOT NULL,          -- 归一化特征指纹，用于去重/计数
    category     TEXT NOT NULL,          -- import | syntax | type | architecture | ui
    summary      TEXT NOT NULL,
    bad_snippet  TEXT,
    good_snippet TEXT,
    source       TEXT NOT NULL,          -- manual | agent | git_fix
    status       TEXT DEFAULT 'pending', -- pending | promoted | ignored
    created_at   DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- 已沉淀规则台账（用于跨项目迁移与命中追踪）
CREATE TABLE rules (
    id           TEXT PRIMARY KEY,
    scope        TEXT NOT NULL,          -- global | project
    project_path TEXT,
    engine       TEXT NOT NULL,          -- eslint | ruff | semgrep
    rule_key     TEXT NOT NULL,
    payload      TEXT NOT NULL,          -- JSON 或 YAML 规则体
    rationale    TEXT NOT NULL,
    hit_count    INTEGER DEFAULT 0,
    created_at   DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- 扫描历史（用于趋势对比）
CREATE TABLE scan_runs (
    id           TEXT PRIMARY KEY,
    project_path TEXT NOT NULL,
    duration_ms  INTEGER NOT NULL,
    escapes_cnt  INTEGER NOT NULL,
    passed       INTEGER NOT NULL,
    created_at   DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

### 4.2 项目级基线 (`.slopguard/baseline.json`)
保持极简与 Git 友好（按 key 排序输出，避免无意义 diff）：

```json
{
  "version": 1,
  "updated_at": "2026-11-17T21:50:00Z",
  "metrics": {
    "ts_ignore": 3,
    "eslint_disable": 5,
    "explicit_any": 12,
    "type_ignore_py": 0
  },
  "suppressed_files": []
}
```

### 4.3 声明式扩展 Adapter (`~/.slopguard/adapters/go.yaml` 示例)
无需改动 Go 源码即可扩展任意语言或框架：

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

---

## 5. CLI 与 MCP 接口契约

### 5.1 CLI 命令面
* `slopguard init [--yes]`：检测技术栈并补齐配置与基线。
* `slopguard verify [--all] [--json] [--jobs N] [--record]`：运行门禁检查（Exit `0` 通过，`1` 检查失败，`2` 工具链缺失或系统错误）；通过后加 `--record` 记录一条 `scan_runs`，本地数据库不可用时仅警告、不影响退出码。
* `slopguard radar [--top 3] [--json]`：输出坏样本雷达与热点文件。
* `slopguard record --summary "..." [--bad "..."] [--good "..."]`：录入一次 AI 错题。
* `slopguard baseline [--tighten | --reset] [--yes]`：更新棘轮基线，操作前打印 Diff 并确认。
* `slopguard mcp`：以 `stdio` 模式启动 MCP Server。

`verify --json` 输出 `contract.VerifyResult`：`duration_ms` 为 int64 毫秒（`DurationMs`）；`skipped` 保留为被跳过检查的 ID 数组（兼容既有消费者），`skipped_details` 新增 `{id, tool, reason}` 跳过明细；`suppressed` 为被 `.slopguard/baseline.json` 的 `suppressed_files` 过滤掉的文件数。

### 5.2 MCP Server 暴露工具（供宿主 Agent 调用）
`slopguard mcp` 使用换行分隔的 JSON-RPC 2.0 帧，`stdout` 只输出协议帧，所有日志/警告走 `stderr`。

1. `sg_verify`：参数 `{ all?: bool, paths?: string[] }` → 返回结构化错误列表（文件、行号、规则、修复提示）及基线对比。
2. `sg_radar`：参数 `{ top?: number }` → 返回 Top-N 坏样本文件与指标明细。
3. `sg_record_mistake`：参数 `{ category, summary, bad_snippet?, good_snippet? }` → 写入 SQLite 并返回当前同类累计次数。
4. `sg_list_candidates`：返回累计 `>= 2` 次的待提炼错题簇。
5. `sg_dry_run_rule`：参数 `{ engine, rule_payload, sample_bad? }` → 临时沙箱试跑并返回命中统计与是否通过样本验证。
6. `sg_apply_rule`：参数 `{ engine, rule_payload, scope: "project" | "global" }` → 写入对应配置并更新台账（需宿主 Agent 先向用户确认）。

---

## 6. 底座工程规范（内存、兼容性与稳定性）

1. **严格内存控制**：
   - 子进程输出绝不 `io.ReadAll` 全量读入内存；统一使用 `bufio.Scanner` 流式解析，单进程输出超过 `2MB` 自动截断并保留首尾关键摘要。
   - 外部工具并发受 `semaphore` 严格限制（默认 `2`），防止并发 `tsc` / `webpack` / `ruff` 引发 OOM。
   - 进程组清理：为每个子进程设置独立进程组（Unix `Setpgid` / Windows Job Object），超时或收到 `SIGINT` 时连带子孙进程一并终止，杜绝僵尸进程。
2. **跨平台兼容性**：
   - 全程使用 `filepath.Join`、`os.UserHomeDir()`，不硬编码 `/` 或 `~/.config`。
   - `modernc.org/sqlite` 纯 Go 实现，支持 `GOOS=darwin/linux/windows` 一键交叉编译。
   - 外部工具缺失时优雅降级（给出明确安装提示或跳过 `optional: true` 检查，不崩溃）。
3. **低耦合测试**：
   - 外部命令调用全部抽象在 `Runner` 接口后，单元测试使用 Mock Runner；同时提供集成测试夹具（fixture repos）验证真实 ESLint/Ruff 行为。

---

## 7. 推荐项目目录结构

```text
slopguard/
├── cmd/slopguard/main.go          # 入口，CLI 路由与信号处理
├── internal/
│   ├── cli/                       # CLI 子命令定义与 JSON/TTY 格式化
│   ├── mcp/                       # stdio MCP Server 与 Tool 注册
│   ├── engine/
│   │   ├── scheduler.go           # 有界并发调度器 + 超时/进程组清理
│   │   ├── git.go                 # Git 变更文件解析与高频 fix 统计
│   │   ├── ratchet.go             # Baseline 棘轮比对
│   │   └── sandbox.go             # 规则 Dry-run 沙箱
│   ├── adapter/
│   │   ├── registry.go            # Adapter 接口与发现机制
│   │   ├── typescript.go          # TS/JS 一等公民适配器
│   │   ├── python.go              # Python 一等公民适配器
│   │   ├── yaml_loader.go         # 声明式 YAML 适配器加载器
│   │   └── ui_runner.go           # UI 烟雾验证插件接口
│   ├── radar/                     # 坏样本扫描与传染分排序
│   └── store/                     # SQLite (modernc) 迁移与 DAO
├── presets/                       # 内置默认通用规则与 Playwright 模板
├── skills/SKILL.md                # 配套宿主 Agent 的标准 Skill 文件
├── go.mod
└── Makefile                       # 交叉编译、test、lint
```

---

## 8. 分阶段实施路线图与验收标准

| 阶段 | 核心交付 | 验收标准 (DoD) |
| :--- | :--- | :--- |
| **Phase 1：底座与 `verify`** | Go 骨架、有界调度器、Git 增量解析、TS/Python 内置 Adapter、YAML 扩展加载、`baseline.json` 棘轮 | 1. 跨平台编译通过且单二进制无 CGO 依赖<br>2. 在真实 TS/Python 仓库跑通 `init` 和 `verify`，增量模式 `< 3s`、内存峰值 `< 50MB`<br>3. 调度器超时杀进程与 Ratchet 单测全绿 |
| **Phase 2：`radar` 与 SQLite** | `modernc.org/sqlite` 存储层、逃生舱/复杂度/死代码/重复代码扫描、传染分排序 | 1. `radar --top 3 --json` 准确输出热点坏样本<br>2. 无新增长逃生舱时 `verify` 放行，新增 1 个 `@ts-ignore` 时 `verify` 拦截 |
| **Phase 3：MCP 与 `learn` 闭环** | stdio MCP Server、`record_mistake`、候选规则 `dry_run` 沙箱、`apply_rule`、配套 `SKILL.md` | 1. 任意支持 MCP 的 Agent 可连接 `slopguard mcp`<br>2. 模拟录入 2 次同类错误 → Agent 起草 ESLint/Ruff 规则 → 沙箱试跑验证命中 → 写入生效 |
| **Phase 4：UI 烟雾与全局同步** | Playwright 通用烟雾模板集成、`presets` 全局规则同步到新项目 | 新建测试项目运行 `slopguard init` 可一键继承全局通用规则与 UI 检查入口 |

---

## 9. 交给下一位开发 Agent 的启动指令（可直接复制）

> 我要从零开发一个名为 **SlopGuard** 的 Go CLI + stdio MCP 工具。请先完整阅读并严格遵循架构设计文档，按 **Phase 1 → Phase 4** 分阶段实现。  
> 约束要求：  
> 1. 使用 Go（推荐 Go 1.23+）与纯 Go SQLite (`modernc.org/sqlite`)，严禁引入 CGO 依赖。  
> 2. 每完成一个 Phase 必须编写并运行对应单元测试与真实夹具仓库集成测试，确认全部通过并报告内存/耗时表现后，再进入下一 Phase。  
> 3. 现在先初始化项目目录结构与 Phase 1 核心代码（`scheduler`、`git`、`ratchet`、`typescript/python adapter`、`yaml_loader`、`init/verify` CLI），完成后向我汇报。
