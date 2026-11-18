# presets

Phase 1/2 仅保留目录占位，不包含预设规则或模板。

Phase 3 的全局规则模板由 adapter 规则写入能力管理，位置为
`~/.slopguard/presets/<engine>/rules.json`（engine 支持 eslint / ruff / semgrep）；
仓库内该目录继续作为后续内置模板的占位。
