import Cocoa
import WebKit

private let defaultPort = 7788
private let dashboardURLString = "http://127.0.0.1:\(defaultPort)"
private let healthURLString = "\(dashboardURLString)/api/overview"
private let summaryURLString = "\(dashboardURLString)/api/summary"

private struct StatusSummary: Decodable, Sendable {
    let pending_user: Int
    let pending_ai: Int
    let total_runs: Int
    let has_scans: Bool
    let last_scan_passed: Bool
    let last_scan_project: String
    let last_scan_at: String
    let guarded_projects: Int
}

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate, NSWindowDelegate, WKNavigationDelegate {
    private var statusItem: NSStatusItem?
    private var contextMenu: NSMenu?
    private var pendingSummaryItem: NSMenuItem?
    private var lastScanSummaryItem: NSMenuItem?
    private var guardSummaryItem: NSMenuItem?
    private var window: NSWindow?
    private var webView: WKWebView?
    private var serverProcess: Process?
    private var summaryPollTask: Task<Void, Never>?
    private var isQuitting = false

    func applicationDidFinishLaunching(_ notification: Notification) {
        setupMainMenu()
        setupStatusItem()
        setupWindow()
        showWindow()
        ensureServerAndLoad()
        startSummaryPolling()
    }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        showWindow()
        return true
    }

    func applicationWillTerminate(_ notification: Notification) {
        isQuitting = true
        summaryPollTask?.cancel()
        summaryPollTask = nil
        stopServerProcessIfOwned()
    }

    // MARK: - Window & WebView

    private func setupWindow() {
        let contentRect = NSRect(x: 0, y: 0, width: 1120, height: 760)
        let styleMask: NSWindow.StyleMask = [.titled, .closable, .miniaturizable, .resizable, .fullSizeContentView]
        let win = NSWindow(
            contentRect: contentRect,
            styleMask: styleMask,
            backing: .buffered,
            defer: false
        )
        win.title = "SlopGuard"
        win.titleVisibility = .hidden
        win.titlebarAppearsTransparent = true
        win.isMovableByWindowBackground = true
        let toolbar = NSToolbar(identifier: "SlopGuardMainToolbar")
        toolbar.showsBaselineSeparator = false
        win.toolbar = toolbar
        win.toolbarStyle = .unifiedCompact
        win.minSize = NSSize(width: 860, height: 580)
        win.isReleasedWhenClosed = false
        win.center()
        win.delegate = self

        let config = WKWebViewConfiguration()
        config.applicationNameForUserAgent = "SlopGuardApp/1.0"
        config.preferences.setValue(true, forKey: "developerExtrasEnabled")

        let wv = WKWebView(frame: contentRect, configuration: config)
        wv.autoresizingMask = [.width, .height]
        wv.navigationDelegate = self
        win.contentView = wv

        self.window = win
        self.webView = wv
    }

    func windowShouldClose(_ sender: NSWindow) -> Bool {
        // Hide to menu bar instead of terminating (Magpie-style).
        sender.orderOut(nil)
        return false
    }

    @objc private func showWindow() {
        guard let win = window else { return }
        NSApp.activate(ignoringOtherApps: true)
        win.makeKeyAndOrderFront(nil)
    }

    @objc private func toggleWindow() {
        guard let win = window else { return }
        if win.isVisible && win.isKeyWindow {
            win.orderOut(nil)
        } else {
            showWindow()
        }
    }

    @objc private func reloadPage() {
        if let currentURL = webView?.url, currentURL.host == "127.0.0.1" {
            webView?.reload()
        } else {
            ensureServerAndLoad()
        }
        Task { @MainActor [weak self] in
            await self?.refreshStatusSummary()
        }
    }

    @objc private func openMistakesTab() {
        showWindow()
        if let currentURL = webView?.url, currentURL.host == "127.0.0.1" {
            webView?.evaluateJavaScript("window.slopguardSwitchTab && window.slopguardSwitchTab('mistakes')")
        } else if let url = URL(string: "\(dashboardURLString)/#tab=mistakes") {
            webView?.load(URLRequest(url: url))
        }
    }

    @objc private func closeActiveWindow() {
        window?.orderOut(nil)
    }

    @objc private func quitApp() {
        isQuitting = true
        NSApp.terminate(nil)
    }

    // MARK: - Menu Bar Icon & Menus

    private func setupStatusItem() {
        let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        if let button = item.button {
            if let img = NSImage(systemSymbolName: "shield.lefthalf.filled", accessibilityDescription: "SlopGuard") {
                img.isTemplate = true
                button.image = img
            } else {
                button.title = "SG"
            }
            button.toolTip = "SlopGuard"
            button.target = self
            button.action = #selector(statusItemClicked(_:))
            button.sendAction(on: [.leftMouseUp, .rightMouseUp])
        }

        let menu = NSMenu()

        let pendingItem = NSMenuItem(title: "待我校验：连接服务中…", action: #selector(openMistakesTab), keyEquivalent: "")
        pendingItem.target = self
        menu.addItem(pendingItem)
        self.pendingSummaryItem = pendingItem

        let scanItem = NSMenuItem(title: "最近体检：暂无记录", action: nil, keyEquivalent: "")
        scanItem.isEnabled = false
        menu.addItem(scanItem)
        self.lastScanSummaryItem = scanItem

        let guardItem = NSMenuItem(title: "后台守护：未开启", action: nil, keyEquivalent: "")
        guardItem.isEnabled = false
        menu.addItem(guardItem)
        self.guardSummaryItem = guardItem

        menu.addItem(NSMenuItem.separator())

        let openItem = NSMenuItem(title: "打开面板 (Open)", action: #selector(showWindow), keyEquivalent: "")
        openItem.target = self
        menu.addItem(openItem)

        let reloadItem = NSMenuItem(title: "刷新 (Reload)", action: #selector(reloadPage), keyEquivalent: "")
        reloadItem.target = self
        menu.addItem(reloadItem)

        menu.addItem(NSMenuItem.separator())

        let quitItem = NSMenuItem(title: "退出 (Quit)", action: #selector(quitApp), keyEquivalent: "q")
        quitItem.target = self
        menu.addItem(quitItem)

        self.statusItem = item
        self.contextMenu = menu
    }

    @objc private func statusItemClicked(_ sender: NSStatusBarButton) {
        let event = NSApp.currentEvent
        let isRightClick = event?.type == .rightMouseUp || (event?.modifierFlags.contains(.control) == true)
        if isRightClick, let item = statusItem, let menu = contextMenu {
            Task { @MainActor [weak self] in
                await self?.refreshStatusSummary()
            }
            item.menu = menu
            sender.performClick(nil)
            DispatchQueue.main.async {
                item.menu = nil
            }
        } else {
            toggleWindow()
        }
    }

    private func setupMainMenu() {
        let mainMenu = NSMenu()

        // App menu (Cmd+Q)
        let appMenuItem = NSMenuItem()
        let appMenu = NSMenu()
        let quitMenuItem = NSMenuItem(title: "退出 SlopGuard", action: #selector(quitApp), keyEquivalent: "q")
        quitMenuItem.target = self
        appMenu.addItem(quitMenuItem)
        appMenuItem.submenu = appMenu
        mainMenu.addItem(appMenuItem)

        // Window / View menu (Cmd+R, Cmd+W)
        let viewMenuItem = NSMenuItem()
        let viewMenu = NSMenu(title: "窗口")
        let reloadItem = NSMenuItem(title: "刷新", action: #selector(reloadPage), keyEquivalent: "r")
        reloadItem.target = self
        viewMenu.addItem(reloadItem)

        let closeItem = NSMenuItem(title: "关闭窗口", action: #selector(closeActiveWindow), keyEquivalent: "w")
        closeItem.target = self
        viewMenu.addItem(closeItem)
        viewMenuItem.submenu = viewMenu
        mainMenu.addItem(viewMenuItem)

        // Edit menu (Cmd+C/V/X/A/Z for WKWebView inputs)
        let editMenuItem = NSMenuItem()
        let editMenu = NSMenu(title: "编辑")
        editMenu.addItem(withTitle: "撤销", action: Selector(("undo:")), keyEquivalent: "z")
        editMenu.addItem(withTitle: "剪切", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        editMenu.addItem(withTitle: "拷贝", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        editMenu.addItem(withTitle: "粘贴", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        editMenu.addItem(withTitle: "全选", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        editMenuItem.submenu = editMenu
        mainMenu.addItem(editMenuItem)

        NSApp.mainMenu = mainMenu
    }

    // MARK: - Server Lifecycle

    private func ensureServerAndLoad() {
        Task { @MainActor [weak self] in
            guard let self else { return }
            if await Self.isServerReachable() {
                self.loadDashboard()
                return
            }

            if !self.startServerProcessIfNeeded() {
                self.loadErrorHTML(
                    title: "未找到 slopguard 可执行文件",
                    detail: "请先编译或安装 slopguard（例如 make build 或放入 /usr/local/bin/slopguard），然后按 Cmd+R 重试。"
                )
                return
            }

            // Wait up to ~6 seconds (40 * 150ms) for port 7788 to come up.
            for _ in 0..<40 {
                if self.isQuitting { return }
                if await Self.isServerReachable() {
                    self.loadDashboard()
                    return
                }
                try? await Task.sleep(nanoseconds: 150_000_000)
            }

            self.loadErrorHTML(
                title: "无法连接到 SlopGuard 服务",
                detail: "已尝试启动 slopguard ui --no-open --port \(defaultPort)，但端口 \(defaultPort) 未在预期时间内响应。请按 Cmd+R 重试。"
            )
        }
    }

    private func loadDashboard() {
        guard let url = URL(string: dashboardURLString) else { return }
        webView?.load(URLRequest(url: url))
        Task { @MainActor [weak self] in
            await self?.refreshStatusSummary()
        }
    }

    private func startSummaryPolling() {
        summaryPollTask?.cancel()
        summaryPollTask = Task { @MainActor [weak self] in
            while let self, !self.isQuitting, !Task.isCancelled {
                await self.refreshStatusSummary()
                try? await Task.sleep(nanoseconds: 30_000_000_000)
            }
        }
    }

    private func refreshStatusSummary() async {
        guard let summary = await Self.fetchStatusSummary() else {
            return
        }
        applyStatusSummary(summary)
    }

    private func applyStatusSummary(_ summary: StatusSummary) {
        pendingSummaryItem?.title = "待我校验：\(summary.pending_user) 条 · 待分析：\(summary.pending_ai) 条"

        if summary.has_scans {
            let statusWord = summary.last_scan_passed ? "通过" : "未通过"
            let proj = summary.last_scan_project.isEmpty ? "默认项目" : summary.last_scan_project
            let rel = Self.formatRelativeTime(summary.last_scan_at)
            let suffix = rel.isEmpty ? "" : " (\(rel))"
            lastScanSummaryItem?.title = "最近体检：\(statusWord) · \(proj)\(suffix)"
        } else {
            lastScanSummaryItem?.title = "最近体检：暂无记录"
        }

        if summary.guarded_projects > 0 {
            guardSummaryItem?.title = "后台守护：已开启 \(summary.guarded_projects) 个项目"
        } else {
            guardSummaryItem?.title = "后台守护：未开启"
        }

        if let button = statusItem?.button {
            if summary.pending_user > 0 {
                button.title = " \(summary.pending_user)"
                button.imagePosition = .imageLeading
            } else {
                button.title = ""
                button.imagePosition = .imageOnly
            }
            let scanTip = summary.has_scans ? (summary.last_scan_passed ? "最近体检通过" : "最近体检未通过") : "暂无体检"
            button.toolTip = "SlopGuard · 待校验 \(summary.pending_user) · \(scanTip)"
        }
    }

    nonisolated private static func formatRelativeTime(_ iso: String) -> String {
        let trimmed = iso.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.isEmpty { return "" }
        let fmtFrac = ISO8601DateFormatter()
        fmtFrac.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        let fmtPlain = ISO8601DateFormatter()
        fmtPlain.formatOptions = [.withInternetDateTime]
        guard let date = fmtFrac.date(from: trimmed) ?? fmtPlain.date(from: trimmed) else {
            return ""
        }
        let delta = max(0, Int(Date().timeIntervalSince(date)))
        if delta < 60 { return "刚刚" }
        if delta < 3600 { return "\(delta / 60) 分钟前" }
        if delta < 86400 { return "\(delta / 3600) 小时前" }
        return "\(delta / 86400) 天前"
    }

    nonisolated private static func fetchStatusSummary() async -> StatusSummary? {
        guard let url = URL(string: summaryURLString) else { return nil }
        var request = URLRequest(url: url)
        request.timeoutInterval = 1.5
        do {
            let (data, response) = try await URLSession.shared.data(for: request)
            guard let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) else {
                return nil
            }
            return try JSONDecoder().decode(StatusSummary.self, from: data)
        } catch {
            return nil
        }
    }

    private func loadErrorHTML(title: String, detail: String) {
        let html = """
        <!doctype html>
        <html lang="zh-CN">
        <head>
        <meta charset="utf-8">
        <style>
          body {
            font-family: -apple-system, BlinkMacSystemFont, "SF Pro Text", sans-serif;
            background: #f5f5f7;
            color: #1d1d1f;
            display: flex;
            align-items: center;
            justify-content: center;
            height: 100vh;
            margin: 0;
          }
          .card {
            background: #fff;
            border: 1px solid #e5e5ea;
            border-radius: 12px;
            padding: 28px 32px;
            max-width: 460px;
            box-shadow: 0 4px 20px rgba(0, 0, 0, 0.06);
          }
          h2 { margin: 0 0 10px; font-size: 18px; }
          p { margin: 0; color: #515154; font-size: 14px; line-height: 1.5; }
        </style>
        </head>
        <body>
          <div class="card">
            <h2>\(title)</h2>
            <p>\(detail)</p>
          </div>
        </body>
        </html>
        """
        webView?.loadHTMLString(html, baseURL: nil)
    }

    nonisolated private static func isServerReachable() async -> Bool {
        guard let url = URL(string: healthURLString) else { return false }
        var request = URLRequest(url: url)
        request.timeoutInterval = 1.0
        do {
            let (_, response) = try await URLSession.shared.data(for: request)
            if let http = response as? HTTPURLResponse {
                return (200..<300).contains(http.statusCode)
            }
            return false
        } catch {
            return false
        }
    }

    private func startServerProcessIfNeeded() -> Bool {
        if let proc = serverProcess, proc.isRunning {
            return true
        }
        guard let binaryPath = resolveSlopGuardBinary() else {
            return false
        }

        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: binaryPath)
        proc.arguments = ["ui", "--no-open", "--port", "\(defaultPort)"]
        proc.standardOutput = FileHandle.nullDevice
        proc.standardError = FileHandle.nullDevice
        proc.terminationHandler = { [weak self] _ in
            Task { @MainActor [weak self] in
                self?.serverProcess = nil
            }
        }

        do {
            try proc.run()
            self.serverProcess = proc
            return true
        } catch {
            return false
        }
    }

    private func stopServerProcessIfOwned() {
        guard let proc = serverProcess, proc.isRunning else { return }
        proc.terminate()
        proc.waitUntilExit()
        serverProcess = nil
    }

    private func resolveSlopGuardBinary() -> String? {
        let fm = FileManager.default
        var candidates: [String] = []

        // 1. Bundled inside SlopGuard.app (Contents/Resources/slopguard to avoid case-insensitive APFS collision with Contents/MacOS/SlopGuard)
        let bundleURL = Bundle.main.bundleURL
        candidates.append(bundleURL.appendingPathComponent("Contents/Resources/slopguard").path)

        // 2. Repo-relative when running from build/SlopGuard.app
        let parentDir = bundleURL.deletingLastPathComponent()
        candidates.append(parentDir.appendingPathComponent("slopguard").path)
        candidates.append(parentDir.deletingLastPathComponent().appendingPathComponent("bin/slopguard").path)

        // 3. Current working directory
        let cwd = fm.currentDirectoryPath
        candidates.append((cwd as NSString).appendingPathComponent("bin/slopguard"))

        // 4. Common install locations
        let home = fm.homeDirectoryForCurrentUser.path
        candidates.append((home as NSString).appendingPathComponent("bin/slopguard"))
        candidates.append((home as NSString).appendingPathComponent(".local/bin/slopguard"))
        candidates.append((home as NSString).appendingPathComponent("go/bin/slopguard"))
        candidates.append("/opt/homebrew/bin/slopguard")
        candidates.append("/usr/local/bin/slopguard")

        // 5. PATH lookup
        if let pathEnv = ProcessInfo.processInfo.environment["PATH"] {
            for dir in pathEnv.split(separator: ":") {
                candidates.append((String(dir) as NSString).appendingPathComponent("slopguard"))
            }
        }

        for path in candidates where fm.isExecutableFile(atPath: path) {
            return path
        }
        return nil
    }

    // MARK: - WKNavigationDelegate

    func webView(
        _ webView: WKWebView,
        decidePolicyFor navigationAction: WKNavigationAction,
        decisionHandler: @escaping @MainActor (WKNavigationActionPolicy) -> Void
    ) {
        guard let url = navigationAction.request.url else {
            decisionHandler(.allow)
            return
        }
        let scheme = url.scheme?.lowercased() ?? ""
        if scheme == "about" || scheme == "data" {
            decisionHandler(.allow)
            return
        }
        if let host = url.host?.lowercased(), host == "127.0.0.1" || host == "localhost" {
            decisionHandler(.allow)
            return
        }
        // Open external links in the default browser.
        NSWorkspace.shared.open(url)
        decisionHandler(.cancel)
    }
}

MainActor.assumeIsolated {
    let app = NSApplication.shared
    let delegate = AppDelegate()
    app.delegate = delegate
    app.run()
}
