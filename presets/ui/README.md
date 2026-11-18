# Playwright UI 烟雾模板

`presets/ui/smoke.spec.ts` 是 SlopGuard 内置的通用 Playwright 烟雾测试模板，
由 `internal/adapter/ui_runner.go` 嵌入并在临时目录执行，检查每个关键路由：

- 浏览器 `console.error`
- 未捕获的 `pageerror`
- 任意 5xx 响应
- 每个路由输出全页截图

## 项目配置

将 `ui.yaml` 复制到项目 `.slopguard/ui.yaml` 并按需修改：

```yaml
base_url: http://127.0.0.1:3000
routes:
  - /
  - /health
screenshot_dir: .slopguard/ui-screenshots
timeout_sec: 60
```

运行时通过 `slopguard verify --ui` 触发（默认关闭以保持日常门禁速度）。
项目没有 `.slopguard/ui.yaml` 或没有安装 Playwright 时自动跳过，不影响常规 verify。

## 安装 Playwright

```bash
npm install -D @playwright/test
npx playwright install chromium
```

`ui_runner` 会优先使用项目 `node_modules/.bin/playwright`，其次使用 PATH 中的
`playwright`。截图路径会写入 `contract.UISmokeResult.Screenshots`，失败时返回结构化
`console.error` / `pageerror` / 5xx findings。
