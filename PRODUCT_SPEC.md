# SlopGuard v2 产品与工程规格书（唯一真相源）

> **最高约束**：任何 AI Agent 严禁擅自修改本文档、严禁自行降级需求、严禁增加本文档未列出的功能。

---

## 1. 产品定位与体验标杆

* **定位**：跨语言、跨 Agent、跨平台（macOS / Windows 原生）的轻量级代码质量护栏与 AI 自动规则演进工具。
* **体验标杆**：`yetone/magpie` —— 底层处理复杂工程细节，表层仅提供一个常驻菜单栏/系统托盘的极简桌面弹窗；**无网页版、无本地 HTTP 监听端口、无多标签后台**。
* **核心理念**：
  1. **确定性优先**：所有检查和拦截必须靠真实工具（原生 Linter + Semgrep）执行，禁止“假绿灯”和“假规则生效”。
  2. **AI 自主评估，人类极简拍板**：后台通过 OpenAI 兼容接口（默认 DeepSeek）静默分析高频错题，经本地沙箱验证通过后，才向用户推送“采纳 / 忽略”卡片。
  3. **零侵入与 100% 可恢复**：绝不改写目标项目原有的 Linter 配置文件；修改宿主 Agent 配置时必须微创编辑并留存快照，关闭开关即无损还原。

---

## 2. 严格禁止事项（Non-Goals / Hard Bans）

以下设计已被明确否决，严禁在任何代码或配置中出现：

1. **禁止启动任何本地 HTTP Server**：桌面端必须通过 Wails v3 进程内绑定直接调用 Go 方法，禁止监听本地端口或编写本地网页鉴权。
2. **禁止编写平台专属原生外壳或系统守护进程**：禁止 `macos/*.swift`、`launchd`、Windows Service；桌面应用打开常驻托盘即运行。
3. **禁止依赖 WSL2、`/bin/sh -c` 或 `bash -c`**：所有外部命令调用必须在 macOS 与 Windows 原生环境下直接运行。
4. **禁止无头调用宿主 Agent CLI 或制作后台工单 UI**：禁止后台 spawn `codex`/`claude` 进程，禁止在 UI 中做“复制提示词弹窗”、“错题收件箱大表格”、“多标签管理后台”。
5. **禁止修改目标项目原生配置文件**：严禁触碰目标项目的 `.eslintrc*`、`pyproject.toml`、`go.mod`、`Cargo.toml` 等文件；所有由 SlopGuard 采纳的新规则必须独立存放于 `<project>/.slopguard/rules/`。
6. **禁止静默覆盖已有配置**：添加项目或初始化时，若 `<project>/.slopguard/config.yaml` 已存在，严禁覆写。
7. **禁止“假规则生效”**：严禁只写数据库台账为 `active/approved` 而不真实落盘规则文件。

---

## 3. 严格目录架构（4 模块完全隔离）

```text
slopguard/
├── PRODUCT_SPEC.md              # 产品与架构唯一真相源
├── AGENTS.md                    # 全模型防跑偏执行铁律
├── go.mod
├── cmd/
│   └── slopguard/
│       └── main.go              # CLI 与 MCP 统一入口
├── internal/
│   ├── contract/                # 跨层不可变数据结构、枚举与错误定义（零外部业务依赖）
│   ├── core/                    # 统一检查引擎：7语言包、Semgrep、沙箱、Git范围与Hook
│   ├── reviewer/                # SQLite 指纹库、触发队列、DeepSeek(OpenAI兼容)评审器、沙箱自修
│   └── bridge/                  # stdio MCP 服务、嵌入 SKILL.md、Magpie 风格无损 Agent 开关
├── desktop/                     # （批次二）Wails v3 双端托盘极简桌面端（薄外壳，严禁写业务逻辑）
├── skills/
│   └── SKILL.md                 # 统一宿主 Agent 行为契约
├── integration/                 # 端到端黑盒集成测试
└── .github/
    └── workflows/
        └── ci.yml               # macOS + Windows 双端自动编译与测试
```

**依赖铁律**：`desktop/` 与 `cmd/` 只能调用 `internal/core`、`internal/reviewer`、`internal/bridge` 暴露的服务方法，严禁重复编排检查流程。

---

## 4. 核心引擎规范（`internal/core`）

### 4.1 诚实状态枚举（彻底消除假绿灯）

`VerifyResult.Status` 必须且只能是以下 5 个值之一：

| 状态枚举 | CLI 退出码 | 严格触发条件 |
|---|---|---|
| `PASS` | `0` | **至少 1 个真实检查器实际执行并扫描了文件**，且错误数为 0。 |
| `FAIL` | `1` | 至少发现 1 个错误或规则违规。 |
| `MISSING_TOOL` | `2` | 识别到了项目语言或存在自定义 Semgrep 规则，但本机缺少对应检查器二进制，必须返回明确的安装指引；**绝不可返回 PASS**。 |
| `UNSUPPORTED` | `2` | 未识别到任何内置语言且 `.slopguard/rules/` 下无有效规则；**绝不可返回 PASS**。 |
| `NO_CHANGES` | `0` | 增量检查（`--scope changed`）模式下 Git 工作区干净无变更文件，明确声明未执行扫描；**绝不可伪装成全量 PASS**。 |

### 4.2 双层检查架构（支持混合语言 Monorepo）

一个项目可同时命中多种语言，按变更或目标文件后缀并行路由检查：

1. **通用规则层（必选能力）**：调用本机 `semgrep`，**仅加载** `<project>/.slopguard/rules/*.yaml` 中的自定义规则。若目录中存在规则文件但本机未安装 `semgrep`，必须返回 `MISSING_TOOL`。
2. **7 种内置主流语言预设包（声明式注册，严禁硬编码耦合）**：

| 语言 ID | 识别特征文件 / 后缀 | 原生检查器（按优先级探测） |
|---|---|---|
| `typescript` | `package.json`, `tsconfig.json`, `.ts`, `.tsx`, `.js`, `.jsx` | `tsc --noEmit` + (`eslint` 或 `biome`) |
| `python` | `pyproject.toml`, `requirements.txt`, `.py` | `ruff check` + `pyright` |
| `go` | `go.mod`, `.go` | `go vet ./...` + (`golangci-lint run`，缺失则仅跑 `go vet` 并在警告中诚实标明) |
| `rust` | `Cargo.toml`, `.rs` | `cargo clippy --message-format=json` |
| `jvm` | `pom.xml`, `build.gradle`, `build.gradle.kts`, `.java`, `.kt` | `ktlint` 或项目内 `gradlew` / `mvn`（未安装则标明 `MISSING_TOOL`） |
| `dart` | `pubspec.yaml`, `.dart` | `dart analyze` 或 `flutter analyze` |
| `cpp` | `CMakeLists.txt`, `.c`, `.cpp`, `.h`, `.hpp` | `cppcheck` 或 `clang-tidy` |

3. **跨平台进程执行规范**：
   * 统一封装 `ProcessRunner`，使用 `exec.CommandContext` 直接传参数数组，严禁 `sh -c`。
   * 自动处理 Windows `.exe` / `.cmd` / `.bat` 查找与 `\r\n` 换行符标准化。
   * 统一使用 `filepath.Join` 和 `os.UserConfigDir()` 存放全局配置与 SQLite 数据库（禁止写死 `~/.slopguard`）。

### 4.3 项目注册与配置保护

* `RegisterProject(path)` 仅登记项目并探测技术栈；若 `<project>/.slopguard/config.yaml` 已存在，**严禁覆盖**，只能只读加载并返回保留提示。
* 仅在 `.slopguard/config.yaml` 不存在时生成默认配置。

---

## 5. AI 自动评审与沙箱闭环规范（`internal/reviewer`）

### 5.1 错题指纹与存储（纯 Go SQLite）

* 推荐使用免 CGO 的 `modernc.org/sqlite`，保证 macOS 与 Windows 无痛交叉编译。
* 每次 `verify` 产生错误时，按以下公式计算标准化指纹：
  `fingerprint = sha256(language + "|" + checker + "|" + rule_id_or_normalized_message)`
* 同一项目 + 指纹合并存储，更新 `occurrence_count`、`consecutive_failures`、`last_seen_at`，并仅保留最新 **最多 3 段**上下文代码片段（每段为报错行前后各 10 行，单段上限 25 行）。

### 5.2 自动触发与冷却状态机（不设每日硬上限，靠机制防刷屏）

* **自动流转**：`COLLECTING` -> `QUEUED` -> `REVIEWING` -> (`PROPOSED` | `IGNORED_BY_AI` | `FAILED_SANDBOX`)
* **用户拍板**：`PROPOSED` -> (`APPROVED` | `DISMISSED` | `SNOOZED`)
* **自动入队条件**（满足任一即可，且当前处于 `COLLECTING`）：
  1. `occurrence_count >= 3`；
  2. 同一会话内 `consecutive_failures >= 2`。
* **冷却去重规则**：
  * 状态为 `IGNORED_BY_AI`、`DISMISSED`、`APPROVED`、`FAILED_SANDBOX` 时，不再自动重复触发评审。
  * 状态为 `SNOOZED` 时，仅当 `occurrence_count` 较暂缓时再增加 **5 次及以上** 才重新进入 `QUEUED`。

### 5.3 DeepSeek（OpenAI 兼容）评审协议

* 配置项：`api_base_url`（默认 `https://api.deepseek.com/v1`）、`api_key`、`model`（默认 `deepseek-chat`）。
* 后台单协程顺序消费队列，强制要求模型输出严格 JSON（解析失败则记录错误，绝不崩溃）：

```json
{
  "decision": "IGNORE | HARD_RULE | GUARD_HINT",
  "summary_zh": "不超过40字的大白话解释：为什么值得拦截或为何忽略",
  "rule_id": "kebab-case-rule-id",
  "semgrep_yaml": "当 decision == HARD_RULE 时必填，完整合法的 Semgrep 规则 YAML",
  "hint_line": "当 decision == GUARD_HINT 时必填，单行精简约束文本"
}
```

### 5.4 强制沙箱验真与 1 次自修闭环

* 收到 `HARD_RULE` 后，**严禁直接变为 `PROPOSED`**，必须依次通过以下校验：
  1. **语法校验**：在临时目录写入规则文件并执行 `semgrep --validate`。
  2. **正样本召回校验**：用该规则扫描该指纹记录的报错代码片段，**必须命中至少 1 处**。
  3. **项目误报校验**：用该规则扫描目标项目代码，统计命中文件数（若扫描异常，或命中超过项目 30% 以上文件且超过 20 处，判定为过宽噪音规则）。
  4. **最多 1 次自修**：上述任一步失败时，将具体报错或过宽原因回传给模型要求修正 **1 次** 并重跑沙箱；仍失败则置为 `FAILED_SANDBOX` 静默丢弃。
  5. **生成沙箱凭证**：全部通过后计算 `sandbox_proof_hash = sha256(semgrep_yaml + positive_hits + project_hits)`，连同实测指标写入 SQLite，状态置为 `PROPOSED`。
* 收到 `GUARD_HINT` 时，检查 `<project>/.slopguard/hints.md` 行数未超过上限 **15 条**，直接置为 `PROPOSED` 等待审批。

### 5.5 真实落盘原子性（彻底杜绝假生效）

用户点击“采纳写入”或 MCP 调用 `sg_apply_proposal` 时，必须严格按以下顺序执行：

1. 校验 `proposal.Status == PROPOSED`；若为 `HARD_RULE`，必须校验 `sandbox_proof_hash` 非空且与当前 YAML 哈希一致。
2. **先真实写盘**：
   * `HARD_RULE`：原子写入 `<project>/.slopguard/rules/<rule_id>.yaml`，并立刻用 `semgrep --validate` 复验落盘文件。
   * `GUARD_HINT`：追加 1 行到 `<project>/.slopguard/hints.md`（若已满 15 条则拒绝并返回明确提示）。
3. **写盘校验成功后再更新库**：将 SQLite 中该提案状态更新为 `APPROVED`。写盘失败必须回滚并返回真实错误。

---

## 6. MCP、Skill 与无损 Agent 开关规范（`internal/bridge`）

### 6.1 精简 stdio MCP 工具（仅保留 5 个）

1. `sg_verify(project_path, scope="changed"|"all")`：执行统一检查并记录错题，返回诚实 `VerifyResult`。
2. `sg_status(project_path)`：返回项目技术栈、真实生效规则数、待审批提案数、上次真实扫描状态。
3. `sg_list_proposals(project_path)`：仅返回已通过沙箱验真的 `PROPOSED` 提案列表。
4. `sg_apply_proposal(project_path, proposal_id, confirm=true)`：执行第 5.5 节真实落盘流程。
5. `sg_dismiss_proposal(project_path, proposal_id, mode="dismiss"|"snooze")`：标记忽略或暂缓。

### 6.2 Magpie 风格无损 Agent 接入开关与兜底

* **本机探测**：启动时探测本机实际存在的 Agent 配置（覆盖 Codex、Claude Code、Cursor、Antigravity / Gemini CLI、DSH / OpenClaw）。
* **微创修改 + 快照还原**：打开开关前将原配置备份至 `<UserConfigDir>/slopguard/backups/<agent>/`；仅精确插入 `slopguard` MCP 节点（保持 JSON/TOML/YAML 其余键与注释无损）并同步安装 `SKILL.md`。关闭开关时精准摘除 `slopguard` 节点并验证配置语法完好。
* **通用兜底**：提供导出标准 MCP JSON/TOML 片段的函数，以及为目标项目一键安装/卸载轻量级 **Git `pre-commit` 钩子**（采用标记块追加与移除，绝不覆盖用户原有 hook）。

---

## 7. Wails v3 极简桌面端规范（`desktop/`，批次二实施）

* **形态**：macOS 顶部菜单栏图标 / Windows 右下角系统托盘图标；左键弹出无边框紧凑面板（宽 `420px`，高自适应 `520~640px`），失去焦点自动收起；右键仅保留“打开面板 / 退出”。
* **视觉与交互**：干净浅色/深色极简风（自动跟随系统，参考 Magpie），**单屏无标签页**，自上而下仅 3 个区块 + 顶部设置抽屉：
  1. **AI 提案区（条件显示）**：仅当存在 `PROPOSED` 提案时在最顶部显示卡片（大白话原因 + 沙箱实测数字 + `[采纳写入]` `[忽略]`）。
  2. **本机 Agent 开关区**：仅显示探测到的本机 Agent + 开关切换 + `[复制 MCP 配置]` 兜底按钮。
  3. **受保护项目区**：`[+ 添加项目]` 原生目录选择按钮 + 项目卡片列表（语言标签、诚实状态灯、真实生效规则数、Git Hook 开关、`[立即检查]`；点击红灯展开精简报错摘要及已生效规则独立开关/删除）。
  4. **设置抽屉（齿轮图标）**：API Base URL、API Key（密码框）、Model 名、`[测试连通性]` 按钮、开机自启开关。