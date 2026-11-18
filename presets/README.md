# presets

- 全局规则模板由 adapter 规则写入能力管理，位置为
  `~/.slopguard/presets/<engine>/rules.json`（engine 支持 eslint / ruff / semgrep）。
- `ui/` 提供 Playwright UI 烟雾模板与 `.slopguard/ui.yaml` 示例，配置字段和安装说明见
  [presets/ui/README.md](ui/README.md)。
