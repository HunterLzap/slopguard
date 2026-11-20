(() => {
  "use strict";

  const state = {
    projects: [],
    project: localStorage.getItem("slopguard.project") || "",
    tab: localStorage.getItem("slopguard.tab") || "overview",
    theme: localStorage.getItem("slopguard.theme") || "system",
    granularity: "day",
    radarTop: "10",
    reviewStatus: "pending",
    overview: null,
    mistakes: [],
    events: [],
    rules: [],
    radar: null,
    meta: null,
    verifyResult: null,
    verifying: false,
    guardStatus: null,
    agentsData: null,
    agentReports: {},
    agentPromptMode: "review",
    diffPreview: null,
    userPickedReviewTab: false,
    recBannerDismissed: false,
    onboardingDismissed: localStorage.getItem("slopguard.onboardingDismissed") === "1",
  };

  const $ = (id) => document.getElementById(id);

  async function api(path, options) {
    const opts = { ...(options || {}) };
    const headers = new Headers(opts.headers || {});
    headers.set("X-Requested-With", "SlopGuard");
    const method = (opts.method || "GET").toUpperCase();
    if (method !== "GET" && method !== "HEAD" && opts.body !== undefined && !headers.has("Content-Type")) {
      headers.set("Content-Type", "application/json");
    }
    opts.headers = headers;
    const response = await fetch(path, opts);
    const data = await response.json().catch(() => ({}));
    if (!response.ok) {
      throw new Error(data.error || response.statusText || "请求失败");
    }
    return data;
  }

  function escapeHTML(value) {
    return String(value ?? "")
      .replaceAll("&", "&amp;")
      .replaceAll("<", "&lt;")
      .replaceAll(">", "&gt;")
      .replaceAll('"', "&quot;")
      .replaceAll("'", "&#39;");
  }

  function formatDate(value) {
    if (!value) return "—";
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return String(value);
    return date.toLocaleString("zh-CN");
  }

  function formatShortTime(value) {
    if (!value) return "—";
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return String(value);
    return (
      date.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit" }) +
      " " +
      date.toLocaleDateString("zh-CN", { month: "numeric", day: "numeric" })
    );
  }

  function shortProject(path) {
    if (!path) return "全部项目";
    const parts = String(path).split(/[\\/]/).filter(Boolean);
    return parts.length ? parts[parts.length - 1] : path;
  }

  function shortPathDisplay(path) {
    if (!path) return "";
    return String(path).replace(/^\/(?:Users|home)\/[^/]+/, "~");
  }

  // 全局悬浮提示（规避卡片 overflow 裁切）
  function initFloatingTooltips() {
    const tipEl = $("global-tooltip");
    if (!tipEl) return;
    let activeTrigger = null;

    function hideTip() {
      activeTrigger = null;
      tipEl.hidden = true;
      tipEl.textContent = "";
    }

    function showTip(trigger) {
      const text = trigger?.dataset?.tip;
      if (!text) {
        hideTip();
        return;
      }
      activeTrigger = trigger;
      tipEl.textContent = text;
      tipEl.hidden = false;

      const rect = trigger.getBoundingClientRect();
      const tipRect = tipEl.getBoundingClientRect();
      const pad = 8;
      let left = rect.left + rect.width / 2 - tipRect.width / 2;
      left = Math.max(pad, Math.min(left, window.innerWidth - tipRect.width - pad));

      let top = rect.top - tipRect.height - 6;
      if (top < pad) {
        top = rect.bottom + 6;
      }
      if (top + tipRect.height > window.innerHeight - pad) {
        top = Math.max(pad, window.innerHeight - tipRect.height - pad);
      }
      tipEl.style.left = `${Math.round(left)}px`;
      tipEl.style.top = `${Math.round(top)}px`;
    }

    document.addEventListener("mouseover", (e) => {
      const trigger = e.target.closest?.("[data-tip]");
      if (trigger) {
        showTip(trigger);
      }
    });
    document.addEventListener("mouseout", (e) => {
      if (!activeTrigger) return;
      if (!e.relatedTarget || !activeTrigger.contains(e.relatedTarget)) {
        hideTip();
      }
    });
    document.addEventListener("focusin", (e) => {
      const trigger = e.target.closest?.("[data-tip]");
      if (trigger) showTip(trigger);
    });
    document.addEventListener("focusout", () => hideTip());
    window.addEventListener("scroll", hideTip, true);
    document.addEventListener("keydown", (e) => {
      if (e.key === "Escape") hideTip();
    });
  }

  async function selectProject(projectPath) {
    state.project = projectPath || "";
    if (state.project) {
      localStorage.setItem("slopguard.project", state.project);
    } else {
      localStorage.removeItem("slopguard.project");
    }
    const sel = $("project-select");
    if (sel) sel.value = state.project;
    updateTopbarProject();
    await refresh();
  }

  function renderQuickProjectAction(container, emptyLabel) {
    if (!container) return;
    container.innerHTML = "";
    if (!state.projects.length) {
      const label = document.createElement("span");
      label.className = "muted";
      label.textContent = emptyLabel || "尚未添加项目";
      const btn = document.createElement("button");
      btn.type = "button";
      btn.className = "btn btn-sm btn-primary";
      btn.textContent = "+ 添加项目";
      btn.addEventListener("click", openAddProjectModal);
      container.append(label, btn);
      return;
    }

    const chips = document.createElement("div");
    chips.className = "quick-project-chips";
    const label = document.createElement("span");
    label.className = "muted";
    label.textContent = "选择项目：";
    chips.appendChild(label);

    for (const p of state.projects.slice(0, 5)) {
      const chip = document.createElement("button");
      chip.type = "button";
      chip.className = "btn btn-sm btn-secondary";
      chip.textContent = shortProject(p.project_path);
      chip.title = p.project_path;
      chip.addEventListener("click", () => selectProject(p.project_path));
      chips.appendChild(chip);
    }

    const addBtn = document.createElement("button");
    addBtn.type = "button";
    addBtn.className = "btn btn-sm btn-ghost";
    addBtn.textContent = "+ 添加项目";
    addBtn.addEventListener("click", openAddProjectModal);
    container.append(chips, addBtn);
  }

  function projectQuery(extra = {}) {
    const params = new URLSearchParams();
    for (const [k, v] of Object.entries(extra)) {
      if (v !== undefined && v !== null && v !== "") {
        params.set(k, String(v));
      }
    }
    if (state.project) params.set("project", state.project);
    return params.toString();
  }

  // 主题切换
  function applyTheme(theme) {
    state.theme = theme;
    localStorage.setItem("slopguard.theme", theme);
    if (theme === "light" || theme === "dark") {
      document.documentElement.setAttribute("data-theme", theme);
    } else {
      document.documentElement.removeAttribute("data-theme");
    }
    document.querySelectorAll(".theme-btn").forEach((btn) => {
      btn.classList.toggle("active", btn.dataset.themeChoice === theme);
    });
  }

  // 导航与侧边栏（极简 3 页：总览 / 错题与规则 / 接入与守护）
  function switchTab(tab, opts = {}) {
    let target = tab;
    if (target === "events") {
      target = "verify";
    } else if (target === "rules") {
      target = "mistakes";
      opts.rulesView = true;
    } else if (target === "radar") {
      target = "overview";
      opts.openRadar = true;
    }
    const validTabs = ["overview", "mistakes", "verify"];
    target = validTabs.includes(target) ? target : "overview";
    state.tab = target;
    localStorage.setItem("slopguard.tab", target);
    document.querySelectorAll(".nav-item").forEach((btn) => {
      btn.classList.toggle("active", btn.dataset.tab === target);
    });
    document.querySelectorAll(".tab-panel").forEach((panel) => {
      panel.hidden = panel.id !== `tab-${target}`;
    });
    if (opts.rulesView) {
      selectReviewTab("rules_view", true);
    }
    if (opts.openRadar) {
      const details = $("overview-radar-details");
      if (details) details.open = true;
    }
    closeSidebar();
  }

  function openSidebar() {
    $("sidebar").classList.add("open");
    $("sidebar-backdrop").hidden = false;
  }

  function closeSidebar() {
    $("sidebar").classList.remove("open");
    $("sidebar-backdrop").hidden = true;
  }

  // 服务运行状态与切换
  let serviceInstalled = false;

  async function loadServiceStatus() {
    const dot = $("service-status-dot");
    const text = $("service-status-text");
    const sw = $("service-switch");
    try {
      const data = await api("/api/service-status");
      serviceInstalled = Boolean(data && data.installed);
      if (sw) {
        sw.disabled = false;
        sw.checked = serviceInstalled;
      }
      if (serviceInstalled) {
        dot.className = "health-dot pass";
        text.textContent = "后台常驻";
        text.title = `已安装开机后台常驻 (${data.platform || "local"}${data.port ? ` · :${data.port}` : ""})`;
      } else {
        dot.className = "health-dot neutral";
        text.textContent = "按需运行";
        text.title = "未安装后台常驻服务，当前仅在打开应用时运行";
      }
    } catch {
      dot.className = "health-dot neutral";
      text.textContent = "按需运行";
      if (sw) {
        sw.disabled = false;
        sw.checked = false;
      }
    }
  }

  async function toggleService(enableOpt) {
    const sw = $("service-switch");
    const nextEnable = typeof enableOpt === "boolean" ? enableOpt : !serviceInstalled;
    if (sw) sw.disabled = true;
    try {
      await api("/api/service/toggle", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ enable: nextEnable }),
      });
      await loadServiceStatus();
    } catch (error) {
      alert(`操作失败：${error.message}`);
      await loadServiceStatus();
    } finally {
      if (sw) sw.disabled = false;
    }
  }

  async function loadProjects() {
    try {
      const data = await api("/api/projects");
      state.projects = Array.isArray(data.projects) ? data.projects : [];
      const select = $("project-select");
      if (select) {
        select.innerHTML = "";
        const all = document.createElement("option");
        all.value = "";
        all.textContent = "全部项目";
        select.appendChild(all);

        for (const project of state.projects) {
          const option = document.createElement("option");
          option.value = project.project_path;
          const count = project.scan_count ?? project.total_runs ?? 0;
          option.textContent = `${shortProject(project.project_path)} (${count})`;
          option.title = project.project_path;
          select.appendChild(option);
        }

        const addOpt = document.createElement("option");
        addOpt.value = "__add__";
        addOpt.textContent = "+ 添加项目…";
        select.appendChild(addOpt);

        if (state.project && !state.projects.some((p) => p.project_path === state.project)) {
          state.project = "";
        }
        select.value = state.project;
      }
      updateTopbarProject();
      renderProjectsTable(state.projects);
    } catch (error) {
      console.error(error);
    }
  }

  function updateTopbarProject() {
    const select = $("project-select");
    const dot = $("project-health-dot");
    const verifyBtn = $("btn-run-verify");

    if (select && select.value !== state.project) {
      select.value = state.project || "";
    }

    if (!state.project) {
      if (dot) dot.className = "health-dot neutral";
      if (verifyBtn) {
        verifyBtn.disabled = true;
        verifyBtn.dataset.tip = "请先在顶部下拉框选择或添加项目";
        verifyBtn.title = "请先在顶部下拉框选择或添加项目";
      }
      return;
    }

    if (verifyBtn) {
      verifyBtn.disabled = state.verifying;
      delete verifyBtn.dataset.tip;
      verifyBtn.title = "";
    }

    const proj = state.projects.find((p) => p.project_path === state.project);
    if (!proj) {
      dot.className = "health-dot neutral";
    } else if (proj.pass_rate !== undefined && Number(proj.pass_rate) < 50) {
      dot.className = "health-dot fail";
    } else if (Number(proj.latest_escapes || 0) > 0) {
      dot.className = "health-dot warn";
    } else {
      dot.className = "health-dot pass";
    }
  }

  function renderOnboardingCard() {
    const card = $("onboarding-card");
    if (!card) return;
    const hasProjects = state.projects.length > 0;
    const hasGuard = Number(state.guardStatus?.guarded_projects || 0) > 0 ||
      Boolean(state.guardStatus?.status?.enabled || state.guardStatus?.status?.hook_installed);
    const hasAgent = Boolean(
      (state.agentsData?.agents || []).some((a) => a.enabled || a.partial)
    );

    const stepProj = $("onboard-step-project");
    const stepGuard = $("onboard-step-guard");
    const stepAgent = $("onboard-step-agent");
    if (stepProj) {
      stepProj.textContent = hasProjects ? "已完成" : "待完成";
      stepProj.className = `pill ${hasProjects ? "pass" : "warn"}`;
    }
    if (stepGuard) {
      stepGuard.textContent = hasGuard ? "已开启" : "未开启";
      stepGuard.className = `pill ${hasGuard ? "pass" : ""}`;
    }
    if (stepAgent) {
      stepAgent.textContent = hasAgent ? "已接入" : "未接入";
      stepAgent.className = `pill ${hasAgent ? "pass" : ""}`;
    }

    const allDone = hasProjects && hasGuard && hasAgent;
    card.hidden = state.onboardingDismissed || allDone;
  }

  // 总览
  function renderOverview(overview) {
    state.overview = overview || {};
    const t = overview.totals || overview;
    const totalRuns = Number(t.total_runs || 0);
    const failedRuns = Number(t.failed_runs || 0);
    const passedRuns = t.passed_runs !== undefined
      ? Number(t.passed_runs)
      : Math.max(0, totalRuns - failedRuns);
    const passRate = t.pass_rate !== undefined
      ? Number(t.pass_rate)
      : (totalRuns > 0 ? (passedRuns / totalRuns) * 100 : 0);

    $("stat-runs").textContent = totalRuns.toLocaleString();
    $("stat-runs-sub").textContent = `通过 ${passedRuns} · 未通过 ${failedRuns}`;

    const passEl = $("stat-pass-rate");
    passEl.textContent = totalRuns > 0 ? `${passRate.toFixed(1)}%` : "—";
    passEl.className = "stat-value " + (totalRuns === 0 ? "" : passRate >= 80 ? "pass" : passRate >= 50 ? "warn" : "fail");
    $("stat-pass-sub").textContent = totalRuns > 0 ? `${passedRuns}/${totalRuns} 次` : "暂无扫描";

    $("stat-duration").textContent = Number(t.avg_duration_ms || 0).toFixed(1);

    const recentRuns = Array.isArray(overview.recent_runs) ? overview.recent_runs : [];
    const currentEscapes = t.latest_escapes !== undefined
      ? Number(t.latest_escapes)
      : (recentRuns.length ? Number(recentRuns[0].escapes_cnt || 0) : 0);
    const escEl = $("stat-escapes");
    escEl.textContent = (recentRuns.length || t.latest_escapes !== undefined) ? currentEscapes.toLocaleString() : "—";
    escEl.className = "stat-value " + (currentEscapes > 0 ? "warn" : "pass");

    if (t.candidate_mistakes !== undefined || t.pending_mistakes !== undefined) {
      const pendCount = Number(t.pending_mistakes || 0);
      $("stat-candidates").textContent = pendCount.toLocaleString();
      $("stat-candidates").className = "stat-value " + (pendCount > 0 ? "warn" : "");
    }

    $("stat-rules").textContent = Number(t.rule_count || 0).toLocaleString();

    renderTrend(Array.isArray(overview.trend) ? overview.trend : []);
    renderRecentRuns(recentRuns);
  }

  function renderTrend(points) {
    const svg = $("trend-chart");
    const empty = $("trend-empty");
    svg.innerHTML = "";
    if (!points.length) {
      empty.hidden = false;
      svg.style.display = "none";
      return;
    }
    empty.hidden = true;
    svg.style.display = "block";

    const width = 1000;
    const height = 190;
    const left = 36;
    const right = 24;
    const top = 14;
    const bottom = 26;
    const plotW = width - left - right;
    const plotH = height - top - bottom;

    const maxRuns = Math.max(1, ...points.map((p) => {
      const passed = Number(p.passed || 0);
      const failed = Number(p.failed || 0);
      return Math.max(Number(p.runs || 0), passed + failed);
    }));
    const maxEscapes = Math.max(1, ...points.map((p) => Number(p.escapes || 0)));
    const maxDur = Math.max(1, ...points.map((p) => Number(p.avg_duration_ms || 0)));

    const n = points.length;
    const barW = Math.min(28, Math.max(6, (plotW / n) * 0.48));

    const grid = [];
    for (let i = 0; i <= 3; i++) {
      const y = top + (plotH * i) / 3;
      grid.push(`<line x1="${left}" y1="${y.toFixed(1)}" x2="${width - right}" y2="${y.toFixed(1)}" class="chart-grid" />`);
    }

    const bars = points.map((p, i) => {
      const cx = left + (plotW / n) * (i + 0.5);
      const x = cx - barW / 2;
      const passed = Number(p.passed || 0);
      const failed = Number(p.failed || 0);
      const total = passed + failed || Number(p.runs || 0);
      const totalH = (total / maxRuns) * plotH;
      const passH = total > 0 ? (passed / total) * totalH : 0;
      const failH = totalH - passH;
      const yPass = top + plotH - passH;
      const yFail = yPass - failH;
      const title = `${escapeHTML(p.bucket)} · 通过: ${passed}, 未通过: ${failed}, 跳过注释: ${p.escapes || 0}, 平均耗时: ${Number(p.avg_duration_ms || 0).toFixed(1)}ms`;
      let rects = "";
      if (passH > 0) {
        rects += `<rect x="${x.toFixed(1)}" y="${yPass.toFixed(1)}" width="${barW.toFixed(1)}" height="${Math.max(1, passH).toFixed(1)}" rx="2" fill="var(--success)" opacity="0.78"><title>${title}</title></rect>`;
      }
      if (failH > 0) {
        rects += `<rect x="${x.toFixed(1)}" y="${yFail.toFixed(1)}" width="${barW.toFixed(1)}" height="${Math.max(1, failH).toFixed(1)}" rx="2" fill="var(--error)" opacity="0.85"><title>${title}</title></rect>`;
      }
      return rects;
    }).join("");

    const escCoords = points.map((p, i) => {
      const cx = left + (plotW / n) * (i + 0.5);
      const y = top + plotH - (Number(p.escapes || 0) / maxEscapes) * plotH * 0.85;
      return `${cx.toFixed(1)},${y.toFixed(1)}`;
    }).join(" ");
    const escLine = `<polyline points="${escCoords}" fill="none" stroke="var(--warning)" stroke-width="2" stroke-linejoin="round" stroke-linecap="round" />`;

    const durCoords = points.map((p, i) => {
      const cx = left + (plotW / n) * (i + 0.5);
      const y = top + plotH - (Number(p.avg_duration_ms || 0) / maxDur) * plotH * 0.75;
      return `${cx.toFixed(1)},${y.toFixed(1)}`;
    }).join(" ");
    const durLine = `<polyline points="${durCoords}" fill="none" stroke="var(--accent)" stroke-width="1.75" stroke-dasharray="4 3" stroke-linejoin="round" />`;

    const labels = points.map((p, i) => {
      if (n > 10 && i % Math.ceil(n / 8) !== 0 && i !== n - 1) return "";
      const cx = left + (plotW / n) * (i + 0.5);
      return `<text x="${cx.toFixed(1)}" y="${height - 6}" text-anchor="middle" class="chart-label">${escapeHTML(p.bucket)}</text>`;
    }).join("");

    svg.setAttribute("viewBox", `0 0 ${width} ${height}`);
    svg.innerHTML = `${grid.join("")}${bars}${durLine}${escLine}${labels}`;
  }

  function renderRecentRuns(runs) {
    const body = $("recent-runs-body");
    const empty = $("recent-runs-empty");
    $("recent-runs-count").textContent = `最近 ${runs.length} 次`;
    body.innerHTML = "";
    if (!runs.length) {
      empty.hidden = false;
      return;
    }
    empty.hidden = true;
    for (const run of runs) {
      const tr = document.createElement("tr");
      const passed = Boolean(run.passed);
      tr.innerHTML = `
        <td class="mono muted">${escapeHTML(formatShortTime(run.created_at))}</td>
        <td class="mono" title="${escapeHTML(run.project_path)}">${escapeHTML(shortProject(run.project_path))}</td>
        <td><span class="pill ${passed ? "pass" : "fail"}">${passed ? "通过" : "未通过"}</span></td>
        <td class="num">${Number(run.escapes_cnt || 0)}</td>
        <td class="num">${Number(run.duration_ms || 0)}ms</td>`;
      body.appendChild(tr);
    }
  }

  function renderProjectsTable(projects) {
    const body = $("projects-body");
    const empty = $("projects-empty");
    $("projects-count").textContent = `${projects.length} 个项目`;
    body.innerHTML = "";
    if (!projects.length) {
      empty.hidden = false;
      return;
    }
    empty.hidden = true;
    for (const p of projects) {
      const tr = document.createElement("tr");
      tr.className = "clickable";
      const runs = Number(p.scan_count ?? p.total_runs ?? 0);
      const passRate = p.pass_rate !== undefined ? `${Number(p.pass_rate).toFixed(0)}%` : "—";
      const escapes = p.latest_escapes !== undefined ? Number(p.latest_escapes) : "—";
      const pending = p.pending_mistakes !== undefined ? Number(p.pending_mistakes) : "—";
      const guardCell = p.guard_enabled
        ? '<span class="pill pass">守护中</span>'
        : '<span class="muted">—</span>';
      tr.innerHTML = `
        <td>
          <div style="font-weight:600">${escapeHTML(shortProject(p.project_path))}</div>
          <div class="mono muted" title="${escapeHTML(p.project_path)}">${escapeHTML(shortPathDisplay(p.project_path))}</div>
        </td>
        <td class="mono muted">${escapeHTML(formatShortTime(p.last_scan_at))}</td>
        <td class="num">${runs}</td>
        <td class="num">${passRate}</td>
        <td class="num">${escapes}</td>
        <td class="num">${pending}</td>
        <td>${guardCell}</td>`;
      tr.addEventListener("click", () => selectProject(p.project_path));
      body.appendChild(tr);
    }
  }

  // 错题栏（含确定性一键分析 + Agent 建议 + 转为规则）
  function populateSelectOptions(selectEl, values, currentVal, labelFn, allLabel = "全部") {
    const existing = Array.from(new Set(values.filter(Boolean))).sort();
    selectEl.innerHTML = `<option value="">${escapeHTML(allLabel)}</option>`;
    for (const val of existing) {
      const opt = document.createElement("option");
      opt.value = val;
      opt.textContent = labelFn ? labelFn(val) : val;
      selectEl.appendChild(opt);
    }
    selectEl.value = existing.includes(currentVal) ? currentVal : "";
  }

  function formatCategoryLabel(cat) {
    const map = {
      lint: "代码规范",
      type: "类型隐患",
      escape: "跳过注释",
      ast: "结构问题",
      session: "会话返工",
      runtime: "运行时隐患",
      general: "常规",
    };
    return map[cat] || cat || "常规";
  }

  function formatToolLabel(tool) {
    const map = {
      sg_verify: "代码体检",
      sg_radar: "坏样本雷达",
      sg_list_inbox: "拉取待分析错题",
      sg_submit_analysis: "提交分析建议",
      sg_promote_rule: "规则入库",
    };
    return map[tool] ? `${map[tool]} (${tool})` : (tool || "—");
  }

  function formatRuleStatusLabel(status) {
    const map = {
      active: "已启用",
      enabled: "已启用",
      disabled: "已停用",
      draft: "草稿",
    };
    return map[String(status || "").toLowerCase()] || status || "—";
  }

  function formatBaselineMetricLabel(name) {
    const map = {
      type_ignore_py: "Python 忽略类型 (# type: ignore)",
      noqa: "Python 跳过规范 (# noqa)",
      pyright_ignore: "Pyright 忽略 (# pyright: ignore)",
      ts_ignore: "TS 单行忽略 (@ts-ignore)",
      ts_expect_error: "TS 预期报错 (@ts-expect-error)",
      ts_nocheck: "TS 整文件跳过 (@ts-nocheck)",
      as_any: "任意类型强转 (as any)",
      eslint_disable: "ESLint 跳过注释",
    };
    return map[name] || name;
  }

  function normalizeReviewStatus(m) {
    if (m.review_status) return m.review_status;
    if (m.status === "ignored") return "ignored";
    if (m.status === "applied" || m.status === "promoted") return "approved";
    return m.ai_decision ? "pending_user" : "pending_ai";
  }

  function formatSourceLabel(src) {
    const map = {
      auto: "自动捕获",
      verify: "自动捕获",
      auto_verify: "自动体检",
      auto_watch: "文件守护",
      git_hook: "Git 钩子",
      git_fix: "Git 返工",
      session_claude: "Claude 会话",
      session_codex: "Codex 会话",
      session_antigravity: "Antigravity 会话",
      manual: "手动记录",
      cli: "CLI 记录",
      agent: "Agent 记录",
      mcp: "Agent 记录",
    };
    return map[src] || src || "未知来源";
  }

  function renderMistakes(mistakes) {
    state.mistakes = mistakes;

    const counts = {
      pending_user: 0,
      pending_ai: 0,
      approved: 0,
      ignored: 0,
    };
    let recApprove = 0;
    let recSkip = 0;
    for (const m of mistakes) {
      const rs = normalizeReviewStatus(m);
      if (counts[rs] !== undefined) counts[rs]++;
      if (rs === "pending_user") {
        const dec = String(m.ai_decision || "").toLowerCase();
        if (dec === "recommend" || dec === "approve" || dec === "promote" || dec === "recommend_approve") {
          recApprove++;
        } else if (dec === "skip" || dec === "ignore") {
          recSkip++;
        }
      }
    }

    const pendingTotal = counts.pending_user + counts.pending_ai;
    const pendEl = $("count-pending");
    if (pendEl) pendEl.textContent = String(pendingTotal);
    const allEl = $("count-all-mistakes");
    if (allEl) allEl.textContent = String(mistakes.length);
    const ignEl = $("count-ignored");
    if (ignEl) ignEl.textContent = String(counts.ignored);

    const promptBtn = $("btn-agent-prompt");
    if (promptBtn) {
      promptBtn.textContent = counts.pending_ai > 0
        ? `复制分析提示词 (${counts.pending_ai})`
        : "Agent 提示词";
    }

    const banner = $("session-scan-banner");
    if (banner && !state.recBannerDismissed && (recApprove > 0 || recSkip > 0)) {
      const parts = [];
      if (recApprove > 0) parts.push(`建议入库 <strong>${recApprove}</strong> 条`);
      if (recSkip > 0) parts.push(`建议忽略 <strong>${recSkip}</strong> 条`);
      const actionParts = [];
      if (recApprove > 0) actionParts.push(`${recApprove} 转规则`);
      if (recSkip > 0) actionParts.push(`${recSkip} 忽略`);
      showInboxBanner(
        `<strong>Agent 审查建议已回传：</strong>共 ${recApprove + recSkip} 条待处理（${parts.join(" · ")}）。`,
        `<button class="btn btn-primary btn-sm js-apply-ai-rec" type="button">一键采纳建议（${actionParts.join(" · ")}）</button>`,
        true
      );
      const applyBtn = banner.querySelector(".js-apply-ai-rec");
      if (applyBtn) {
        applyBtn.addEventListener("click", () => applyAIRecommendations(applyBtn));
      }
    }

    $("stat-candidates").textContent = pendingTotal.toLocaleString();
    $("stat-candidates").className = "stat-value " + (pendingTotal > 0 ? "warn" : "");
    $("stat-candidates-sub").textContent = `已分析 ${counts.pending_user} · 待分析 ${counts.pending_ai}`;

    const badge = $("badge-mistakes");
    badge.textContent = String(pendingTotal);
    badge.hidden = pendingTotal === 0;

    populateSelectOptions($("mistakes-filter-category"), mistakes.map((m) => m.category), $("mistakes-filter-category").value, formatCategoryLabel, "全部分类");
    populateSelectOptions($("mistakes-filter-source"), mistakes.map((m) => m.source), $("mistakes-filter-source").value, formatSourceLabel, "全部来源");

    applyMistakeFilters();
  }

  function showInboxBanner(textHTML, actionHTML = "", isRecPrompt = false) {
    const banner = $("session-scan-banner");
    if (!banner) return;
    banner.hidden = false;
    banner.innerHTML = `<span class="banner-text">${textHTML}</span>` +
      `<div class="card-header-actions">${actionHTML}<button class="icon-btn js-close-banner" type="button" aria-label="关闭提示">✕</button></div>`;
    const closeBtn = banner.querySelector(".js-close-banner");
    if (closeBtn) {
      closeBtn.addEventListener("click", () => {
        banner.hidden = true;
        if (isRecPrompt) state.recBannerDismissed = true;
      });
    }
  }

  function applyMistakeFilters() {
    const reviewF = state.reviewStatus;
    const catF = $("mistakes-filter-category").value;
    const srcF = $("mistakes-filter-source").value;
    const q = $("mistakes-filter-q").value.trim().toLowerCase();

    const filtered = state.mistakes.filter((m) => {
      const rs = normalizeReviewStatus(m);
      if (reviewF === "pending") {
        if (rs !== "pending_ai" && rs !== "pending_user") return false;
      } else if (reviewF && reviewF !== "rules_view" && rs !== reviewF) {
        return false;
      }
      if (catF && m.category !== catF) return false;
      if (srcF && m.source !== srcF) return false;
      if (q) {
        const hay = `${m.summary || ""} ${m.fingerprint || ""} ${m.bad_snippet || ""} ${m.good_snippet || ""} ${m.ai_reason || ""} ${m.category || ""}`.toLowerCase();
        if (!hay.includes(q)) return false;
      }
      return true;
    });

    if (reviewF === "") {
      const rank = { pending_user: 0, pending_ai: 1, approved: 2, ignored: 3 };
      filtered.sort((a, b) => (rank[normalizeReviewStatus(a)] ?? 9) - (rank[normalizeReviewStatus(b)] ?? 9));
    }

    $("mistakes-summary").textContent = `共 ${filtered.length} 条`;
    const list = $("mistakes-list");
    const empty = $("mistakes-empty");
    list.innerHTML = "";
    if (!filtered.length) {
      empty.textContent = reviewF === "pending"
        ? "待处理已清零 · 所有捕获问题均已处理，可在「已入库规则」或「全部」中查看"
        : "当前分类暂无错题 · 点击右上「立即体检」或「嗅探会话」可自动捕获";
      empty.hidden = false;
      return;
    }
    empty.hidden = true;

    for (const m of filtered) {
      const count = Number(m.count || 0);
      const isCandidate = Boolean(m.is_candidate) || count >= 2;
      const rs = normalizeReviewStatus(m);
      const decision = String(m.ai_decision || "").toLowerCase();
      const isApproveRec = decision === "recommend" || decision === "approve" || decision === "promote" || decision === "recommend_approve";

      let statusPillHTML = "";
      if (rs === "approved") {
        statusPillHTML = '<span class="pill pass">已入库规则</span>';
      } else if (rs === "ignored") {
        statusPillHTML = '<span class="pill ignored">已忽略</span>';
      } else if (rs === "pending_user" && decision) {
        statusPillHTML = `<span class="pill ${isApproveRec ? "warn" : "ignored"}">${isApproveRec ? "建议入库" : "建议忽略"}</span>`;
      }

      const card = document.createElement("article");
      card.className = "mistake-card" + (rs === "pending_user" || (isCandidate && rs === "pending_ai") ? " candidate" : "");

      const snippets = [];
      if (m.bad_snippet) {
        snippets.push(`<div class="snippet-box"><span class="snippet-label">错误示例</span><pre class="snippet bad">${escapeHTML(m.bad_snippet)}</pre></div>`);
      }
      if (m.good_snippet) {
        snippets.push(`<div class="snippet-box"><span class="snippet-label">修复参考</span><pre class="snippet good">${escapeHTML(m.good_snippet)}</pre></div>`);
      }

      // 分析建议或初步解读（不重复堆叠标题与徽标）
      let aiAdviceHTML = "";
      if (m.ai_reason) {
        const draftHTML = m.ai_rule_draft
          ? `<details class="inline-details" style="margin-top:6px;"><summary>查看生成规则草稿</summary><pre class="code-block" style="margin-top:6px;">${escapeHTML(m.ai_rule_draft)}</pre></details>`
          : "";
        aiAdviceHTML = `
          <div class="ai-advice">
            <div class="ai-advice-head">
              <span class="ai-advice-title">分析建议</span>
              ${m.ai_analyzed_at ? `<span class="muted mono">${escapeHTML(formatShortTime(m.ai_analyzed_at))}</span>` : ""}
            </div>
            <p class="ai-advice-reason">${escapeHTML(m.ai_reason)}</p>
            ${draftHTML}
          </div>`;
      } else if (rs === "pending_ai" && m.local_hint) {
        aiAdviceHTML = `<p class="local-hint-line"><strong>初步解读：</strong>${escapeHTML(m.local_hint)}</p>`;
      }

      const actionBtns = [];
      if (rs !== "approved") {
        const approveCls = rs === "pending_user" && !isApproveRec ? "btn-secondary" : "btn-primary";
        actionBtns.push(`<button class="btn ${approveCls} btn-sm" data-action="approve" data-fp="${escapeHTML(m.fingerprint)}" type="button">转为规则</button>`);
      }
      if (rs !== "ignored") {
        actionBtns.push(`<button class="btn btn-ghost btn-sm" data-action="ignore" data-fp="${escapeHTML(m.fingerprint)}" type="button">忽略</button>`);
      } else {
        actionBtns.push(`<button class="btn btn-secondary btn-sm" data-action="restore" data-fp="${escapeHTML(m.fingerprint)}" type="button">恢复</button>`);
      }

      card.innerHTML = `
        <div class="mistake-count" data-tip="累计捕获 ${count} 次">${count}</div>
        <div class="mistake-main">
          <div class="mistake-title-row">
            <h3 class="mistake-title">${escapeHTML(m.summary || "未命名错题")}</h3>
            ${statusPillHTML}
            ${isCandidate ? `<span class="pill warn" data-tip="该错题已重复出现至少 2 次，建议「转为规则」长期拦截">高频 &ge;2</span>` : ""}
          </div>
          <div class="mistake-meta">
            <span class="pill">${escapeHTML(formatCategoryLabel(m.category))}</span>
            <span class="pill">${escapeHTML(formatSourceLabel(m.source))}</span>
            <span>${escapeHTML(shortProject(m.project_path))}</span>
            <span>${escapeHTML(formatDate(m.updated_at || m.created_at))}</span>
          </div>
          ${snippets.length ? `<div class="snippet-row">${snippets.join("")}</div>` : ""}
          ${aiAdviceHTML}
        </div>
        <div class="mistake-actions">${actionBtns.join("")}</div>`;

      card.querySelectorAll("button[data-action]").forEach((btn) => {
        btn.addEventListener("click", () => handleMistakeAction(btn.dataset.fp, btn.dataset.action, btn));
      });

      list.appendChild(card);
    }
  }

  async function handleMistakeAction(fingerprint, action, button) {
    if (!fingerprint) return;
    button.disabled = true;
    try {
      await api(`/api/mistakes/${encodeURIComponent(fingerprint)}/${action}`, { method: "POST" });
      await refresh();
    } catch (error) {
      button.disabled = false;
      button.textContent = "操作失败";
      button.title = error.message;
    }
  }

  // Agent 活动流
  function renderEvents(events) {
    state.events = events;
    const badge = $("badge-events");
    badge.textContent = String(events.length);
    badge.hidden = events.length === 0;

    populateSelectOptions($("events-filter-tool"), events.map((e) => e.tool || e.tool_name), $("events-filter-tool").value, formatToolLabel);
    applyEventFilters();
  }

  function applyEventFilters() {
    const toolF = $("events-filter-tool").value;
    const q = $("events-filter-q").value.trim().toLowerCase();

    const filtered = state.events.filter((e) => {
      const tool = e.tool || e.tool_name || "";
      if (toolF && tool !== toolF) return false;
      if (q) {
        const hay = `${tool} ${e.project_path || ""} ${e.args_summary || ""} ${e.summary || ""}`.toLowerCase();
        if (!hay.includes(q)) return false;
      }
      return true;
    });

    $("events-summary").textContent = `显示 ${filtered.length} / 共 ${state.events.length} 条活动`;
    const body = $("events-body");
    const empty = $("events-empty");
    body.innerHTML = "";
    if (!filtered.length) {
      empty.hidden = false;
      return;
    }
    empty.hidden = true;

    for (const ev of filtered) {
      const tr = document.createElement("tr");
      const tool = ev.tool || ev.tool_name || "—";
      const passed = ev.passed !== false && ev.status !== "intercepted" && ev.status !== "failed";
      const findings = Number(ev.findings_cnt ?? ev.findings_count ?? 0);
      const duration = Number(ev.duration_ms || 0);
      const argsSummary = ev.args_summary || ev.summary || "—";
      const detailText = ev.error_text ? `${argsSummary} · 错误: ${ev.error_text}` : argsSummary;

      tr.innerHTML = `
        <td class="mono muted">${escapeHTML(formatShortTime(ev.created_at))}</td>
        <td class="mono" title="${escapeHTML(ev.project_path || "")}">${escapeHTML(shortProject(ev.project_path))}</td>
        <td><code class="pill mono" title="${escapeHTML(tool)}">${escapeHTML(formatToolLabel(tool))}</code></td>
        <td class="args-summary" title="${escapeHTML(detailText)}">${escapeHTML(detailText)}</td>
        <td><span class="pill ${passed ? "pass" : "fail"}">${passed ? "通过" : "已拦截"}</span></td>
        <td class="num">${findings}</td>
        <td class="num">${duration}ms</td>`;
      body.appendChild(tr);
    }
  }

  // 规则台账（集成于「错题与规则」的「已生效规则」视图）
  function renderRules(rules) {
    state.rules = rules;
    const countEl = $("count-rules");
    if (countEl) countEl.textContent = String(rules.length);

    populateSelectOptions($("rules-filter-engine"), rules.map((r) => r.engine), $("rules-filter-engine").value, null, "全部工具");
    populateSelectOptions($("rules-filter-status"), rules.map((r) => r.status), $("rules-filter-status").value, formatRuleStatusLabel, "全部状态");

    applyRuleFilters();
  }

  function applyRuleFilters() {
    const engineF = $("rules-filter-engine").value;
    const scopeF = $("rules-filter-scope").value;
    const statusF = $("rules-filter-status").value;

    const filtered = state.rules.filter((r) => {
      if (engineF && r.engine !== engineF) return false;
      if (scopeF && r.scope !== scopeF) return false;
      if (statusF && r.status !== statusF) return false;
      return true;
    });

    $("rules-summary").textContent = `显示 ${filtered.length} / 共 ${state.rules.length} 条规则`;
    const body = $("rules-body");
    const empty = $("rules-empty");
    body.innerHTML = "";
    if (!filtered.length) {
      empty.hidden = false;
      return;
    }
    empty.hidden = true;

    for (const rule of filtered) {
      const tr = document.createElement("tr");
      const scope = rule.scope || "project";
      const scopeLabel = scope === "global" ? "全局" : "项目级";
      const ruleKey = rule.rule_key || rule.rule_id || "—";
      const hasPayload = Boolean(rule.payload);
      tr.innerHTML = `
        <td><span class="pill ${scope === "global" ? "accent" : ""}">${escapeHTML(scopeLabel)}</span></td>
        <td class="mono">${escapeHTML(rule.engine || "—")}</td>
        <td><code>${escapeHTML(ruleKey)}</code></td>
        <td>${escapeHTML(rule.rationale || "—")}</td>
        <td>${rule.source ? `<span class="pill">${escapeHTML(formatSourceLabel(rule.source))}</span>` : "—"}</td>
        <td class="mono muted">${escapeHTML(formatShortTime(rule.applied_at || rule.created_at))}</td>
        <td class="num">${Number(rule.hit_count || 0).toLocaleString()}</td>
        <td class="num">${hasPayload ? `<button class="btn btn-ghost btn-sm" type="button">查看</button>` : "—"}</td>`;
      body.appendChild(tr);

      if (hasPayload) {
        const payloadTr = document.createElement("tr");
        payloadTr.className = "payload-row";
        payloadTr.hidden = true;
        payloadTr.innerHTML = `<td colspan="8"><pre class="code-block">${escapeHTML(rule.payload)}</pre></td>`;
        body.appendChild(payloadTr);

        const toggleBtn = tr.querySelector("button");
        toggleBtn.addEventListener("click", () => {
          payloadTr.hidden = !payloadTr.hidden;
          toggleBtn.textContent = payloadTr.hidden ? "查看" : "收起";
        });
      }
    }
  }

  // 坏样本雷达与基线棘轮
  function renderRadar(data) {
    state.radar = data || {};
    $("radar-project-badge").textContent = state.project ? shortProject(state.project) : "未选项目";

    const skipped = Array.isArray(data.skipped) ? data.skipped : [];
    const skipBanner = $("radar-skipped-banner");
    if (skipped.length) {
      skipBanner.hidden = false;
      skipBanner.innerHTML = `<strong>可选扫描工具未安装（不影响基础守护）：</strong> ` +
        skipped.map((s) => `<span class="mono">${escapeHTML(s.tool || s.dimension || "?")}</span>`).join(" · ");
    } else {
      skipBanner.hidden = true;
    }

    const baseline = data.baseline || {};
    const statePill = $("baseline-state");
    $("baseline-updated").textContent = baseline.updated_at ? formatShortTime(baseline.updated_at) : "";
    if (!baseline.available) {
      statePill.textContent = "未配置";
      statePill.className = "pill";
    } else {
      statePill.textContent = baseline.passed ? "通过" : "未通过";
      statePill.className = `pill ${baseline.passed ? "pass" : "fail"}`;
    }

    const metricList = $("baseline-list");
    metricList.innerHTML = "";
    const metrics = Array.isArray(baseline.metrics) ? baseline.metrics : [];
    if (!state.project) {
      const box = document.createElement("div");
      box.className = "quick-project-bar";
      renderQuickProjectAction(box, "暂无项目");
      metricList.appendChild(box);
    } else if (!metrics.length) {
      metricList.innerHTML = `<p class="empty-state">${baseline.available ? "暂无逃生舱指标" : "未找到 baseline.json"}</p>`;
    } else {
      const head = document.createElement("div");
      head.className = "metric-row metric-head";
      head.innerHTML = `<span>指标</span><span class="metric-num">基线配额</span><span class="metric-num">当前</span><span class="metric-num">变化</span><span></span>`;
      metricList.appendChild(head);

      const max = Math.max(1, ...metrics.map((m) => Math.max(Number(m.baseline || 0), Number(m.current || 0))));
      for (const m of metrics) {
        const curr = Number(m.current || 0);
        const base = Number(m.baseline || 0);
        const delta = m.delta !== undefined ? Number(m.delta) : (curr - base);
        const width = Math.min(100, (curr / max) * 100);
        const deltaStr = delta > 0 ? `+${delta}` : String(delta);
        const deltaCls = delta > 0 ? "delta-pos" : delta < 0 ? "delta-neg" : "delta-zero";
        const row = document.createElement("div");
        row.className = "metric-row";
        row.innerHTML = `
          <span title="${escapeHTML(m.name)}">${escapeHTML(formatBaselineMetricLabel(m.name))}</span>
          <span class="metric-num muted">${base}</span>
          <span class="metric-num">${curr}</span>
          <span class="metric-num ${deltaCls}">${deltaStr}</span>
          <span class="metric-bar"><span class="${delta > 0 ? "over" : ""}" style="width:${width.toFixed(1)}%"></span></span>`;
        metricList.appendChild(row);
      }
    }

    const suppressed = Array.isArray(baseline.suppressed_files) ? baseline.suppressed_files : [];
    const supWrap = $("suppressed-wrap");
    if (suppressed.length) {
      supWrap.hidden = false;
      $("suppressed-count").textContent = String(suppressed.length);
      $("suppressed-list").innerHTML = suppressed.map((f) => `<li>${escapeHTML(f)}</li>`).join("");
    } else {
      supWrap.hidden = true;
    }

    const hotspots = Array.isArray(data.hotspots) ? data.hotspots : [];
    $("hotspots-count").textContent = `${hotspots.length} 个文件`;
    const body = $("hotspot-body");
    const empty = $("hotspot-empty");
    body.innerHTML = "";
    if (!hotspots.length) {
      empty.hidden = false;
    } else {
      empty.hidden = true;
      for (const h of hotspots) {
        const risk = Number(h.risk_score || 0);
        const riskCls = risk >= 15 ? "high" : risk >= 7 ? "med" : "low";
        const tr = document.createElement("tr");
        tr.innerHTML = `
          <td class="hotspot-file" title="${escapeHTML(h.file)}">${escapeHTML(h.file)}</td>
          <td class="num"><span class="risk-badge ${riskCls}">${risk}</span></td>
          <td class="num">${Number(h.references || 0)}</td>
          <td class="num">${Number(h.complexity || 0)}</td>
          <td class="num">${Number(h.escapes || 0)}</td>
          <td class="num">${Number(h.rework || 0)}</td>
          <td class="num">${Number(h.dead_code || 0)}</td>
          <td class="num">${Number(h.duplicate || 0)}</td>`;
        body.appendChild(tr);
      }
    }
  }

  // 适配器与配置
  function renderKV(container, obj, emptyText) {
    container.innerHTML = "";
    if (!obj || typeof obj !== "object" || !Object.keys(obj).length) {
      container.innerHTML = `<span class="muted">${escapeHTML(emptyText)}</span>`;
      return;
    }
    const entries = flattenObject(obj);
    for (const [k, v] of entries) {
      const kEl = document.createElement("span");
      kEl.className = "kv-key";
      kEl.textContent = k;
      const vEl = document.createElement("span");
      vEl.className = "kv-val";
      const rawVal = Array.isArray(v) ? v.join(", ") : String(v);
      vEl.textContent = rawVal.replace(/\/(?:Users|home)\/[^/\s]+/g, "~");
      if (vEl.textContent !== rawVal) {
        vEl.title = rawVal;
      }
      const mEl = document.createElement("span");
      mEl.className = "kv-meta";
      container.append(kEl, vEl, mEl);
    }
  }

  function flattenObject(obj, prefix = "") {
    const out = [];
    for (const [k, v] of Object.entries(obj)) {
      const key = prefix ? `${prefix}.${k}` : k;
      if (v && typeof v === "object" && !Array.isArray(v)) {
        out.push(...flattenObject(v, key));
      } else {
        out.push([key, v ?? "—"]);
      }
    }
    return out;
  }

  function renderProjectMeta(meta) {
    state.meta = meta;
    const emptyProj = $("adapters-empty-project");
    const content = $("adapters-content");
    if (!state.project) {
      emptyProj.hidden = false;
      content.hidden = true;
      renderQuickProjectAction(emptyProj, "尚未添加项目");
      return;
    }
    emptyProj.hidden = true;
    content.hidden = false;

    const nameEl = $("project-meta-name");
    const pathEl = $("project-meta-path");
    if (nameEl) nameEl.textContent = shortProject(state.project);
    if (pathEl) {
      pathEl.textContent = shortPathDisplay(state.project);
      pathEl.title = state.project;
    }

    const adapters = Array.isArray(meta?.adapters) ? meta.adapters : [];
    $("adapters-count").textContent = `${adapters.length} 个适配器`;

    const list = $("adapters-list");
    list.innerHTML = "";
    if (!adapters.length) {
      list.innerHTML = `<p class="empty-state">当前项目未检测到语言适配器。</p>`;
    } else {
      for (const a of adapters) {
        const card = document.createElement("div");
        card.className = "adapter-card";
        const exts = Array.isArray(a.extensions) ? a.extensions : [];
        const checks = Array.isArray(a.checks) ? a.checks : [];
        const patterns = Array.isArray(a.escape_patterns) ? a.escape_patterns : [];

        const checksHTML = checks.length
          ? `<div class="sub-block-title">检查项 (${checks.length})</div>
             <div class="table-wrap">
               <table class="mini-table">
                 <thead><tr><th>ID</th><th>命令</th><th>可选</th><th>超时</th></tr></thead>
                 <tbody>${checks.map((c) => {
                   const cmdRaw = Array.isArray(c.cmd) ? c.cmd.join(" ") : (c.cmd || "");
                   const cmdDisplay = String(cmdRaw).replace(/\/(?:Users|home)\/[^/\s]+/g, "~");
                   return `
                   <tr>
                     <td class="mono">${escapeHTML(c.id)}</td>
                     <td class="mono" title="${escapeHTML(cmdRaw)}">${escapeHTML(cmdDisplay)}</td>
                     <td>${c.optional ? "是" : "否"}</td>
                     <td class="mono">${c.timeout_sec ? `${c.timeout_sec}s` : "—"}</td>
                   </tr>`;
                 }).join("")}
                 </tbody>
               </table>
             </div>`
          : "";

        const patternsHTML = patterns.length
          ? `<div class="sub-block-title">跳过注释正则 (${patterns.length})</div>
             <div class="table-wrap">
               <table class="mini-table">
                 <thead><tr><th>名称</th><th>正则</th></tr></thead>
                 <tbody>${patterns.map((p) => `
                   <tr>
                     <td>${escapeHTML(p.name)}</td>
                     <td class="mono">${escapeHTML(p.regex)}</td>
                   </tr>`).join("")}
                 </tbody>
               </table>
             </div>`
          : "";

        card.innerHTML = `
          <div class="adapter-head">
            <span class="adapter-name">${escapeHTML(a.name)}</span>
            <div class="pill-row">${exts.map((e) => `<span class="pill mono">${escapeHTML(e)}</span>`).join("")}</div>
          </div>
          ${checksHTML}
          ${patternsHTML}`;
        list.appendChild(card);
      }
    }

    const cfg = meta?.config || null;
    const cfgLoaded = Boolean(cfg && cfg.exists !== false);
    $("config-status").textContent = cfgLoaded ? "已加载" : "缺失";
    $("config-status").className = `pill ${cfgLoaded ? "pass" : ""}`;
    renderKV($("config-kv"), cfgLoaded ? cfg : null, "未找到 .slopguard/config.yaml。");

    const uiCfg = meta?.ui_config || meta?.ui || null;
    const uiLoaded = Boolean(uiCfg && uiCfg.exists !== false);
    $("ui-config-status").textContent = uiLoaded ? "已加载" : "未配置";
    $("ui-config-status").className = `pill ${uiLoaded ? "pass" : ""}`;
    renderKV($("ui-config-kv"), uiLoaded ? uiCfg : null, "未配置 .slopguard/ui.yaml。");
  }

  // 添加项目弹窗与原生文件夹选择
  function openAddProjectModal() {
    $("add-project-modal").hidden = false;
    $("add-project-feedback").hidden = true;
    $("add-project-feedback").textContent = "";
    $("add-project-path").focus();
  }

  function closeAddProjectModal() {
    $("add-project-modal").hidden = true;
  }

  async function pickFolder() {
    const btn = $("btn-pick-folder");
    const feedback = $("add-project-feedback");
    btn.disabled = true;
    try {
      const res = await api("/api/pick-folder");
      if (res.ok && res.path) {
        $("add-project-path").value = res.path;
        feedback.hidden = true;
      } else if (!res.cancelled && res.error) {
        feedback.hidden = false;
        feedback.textContent = res.error;
      }
    } catch (error) {
      feedback.hidden = false;
      feedback.textContent = `打开文件夹选择器失败：${error.message}`;
    } finally {
      btn.disabled = false;
    }
  }

  async function submitAddProject() {
    const path = $("add-project-path").value.trim();
    const feedback = $("add-project-feedback");
    const btn = $("btn-submit-add-project");
    if (!path) {
      feedback.hidden = false;
      feedback.textContent = "请先选择项目目录";
      return;
    }
    btn.disabled = true;
    btn.textContent = "处理中…";
    feedback.hidden = false;
    feedback.textContent = "正在初始化并执行首次体检…";
    try {
      const res = await api("/api/projects/add", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ path }),
      });
      state.project = res.project || path;
      localStorage.setItem("slopguard.project", state.project);
      closeAddProjectModal();
      await loadProjects();
      await refresh();
    } catch (error) {
      feedback.hidden = false;
      feedback.textContent = error.message;
    } finally {
      btn.disabled = false;
      btn.textContent = "添加并体检";
    }
  }

  // 基线操作
  async function runBaselineAction(mode) {
    if (!state.project) {
      alert("请先在左侧工作区选择一个具体项目。");
      return;
    }
    if (mode === "reset" && !confirm("确认将基线重置为当前项目的逃生舱数量吗？")) {
      return;
    }
    const btn = $(mode === "tighten" ? "btn-baseline-tighten" : "btn-baseline-reset");
    if (btn) btn.disabled = true;
    try {
      await api(`/api/baseline/${mode}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ project: state.project }),
      });
      await refreshRadar();
    } catch (error) {
      alert(error.message);
    } finally {
      if (btn) btn.disabled = false;
    }
  }

  // Diff 预览弹窗 + AI Agent (MCP & Skill) 安全接入与一键还原
  function renderDiffHTML(diffText, action, hasChanges) {
    if (!diffText) {
      if (action === "restore") {
        return '<span class="diff-line head">当前文件与原始备份一致或未接入 SlopGuard，无需还原。</span>';
      }
      return hasChanges
        ? '<span class="diff-line head">（空变更）</span>'
        : '<span class="diff-line head">目标文件已包含最新 SlopGuard 配置，无需修改。</span>';
    }
    return diffText
      .split("\n")
      .map((line) => {
        let cls = "diff-line";
        if (line.startsWith("+++") || line.startsWith("---")) {
          cls += " head";
        } else if (line.startsWith("@@")) {
          cls += " hunk";
        } else if (line.startsWith("+")) {
          cls += " add";
        } else if (line.startsWith("-")) {
          cls += " del";
        }
        return `<span class="${cls}">${escapeHTML(line)}</span>`;
      })
      .join("");
  }

  function closeDiffModal() {
    $("diff-preview-modal").hidden = true;
    state.diffPreview = null;
  }

  function openDiffModal(preview) {
    state.diffPreview = preview;
    const isRestore = preview.action === "restore";
    const kindLabel = preview.kind === "agent"
      ? "Agent 接入（MCP + 护栏规则）"
      : preview.kind === "skill" ? "Skill / 规则"
        : preview.kind === "shared_skill" ? "共享护栏指令"
          : preview.kind === "guard_hook" ? "Git Hook" : "MCP 配置";
    $("diff-preview-title").textContent = isRestore
      ? `预览撤销 / 还原 ${kindLabel}：${preview.name || preview.agent_id || preview.target_id}`
      : `预览写入 ${kindLabel}：${preview.name || preview.agent_id || preview.target_id}`;
    $("diff-scope-pill").textContent = preview.scope === "global" ? "全局配置" : "项目级配置";
    $("diff-scope-pill").className = `pill ${preview.scope === "global" ? "warn" : "accent"}`;
    $("diff-preview-path").textContent = preview.config_path || "";
    $("diff-global-warning").hidden = preview.scope !== "global" || isRestore;
    $("diff-preview-content").innerHTML = renderDiffHTML(preview.diff, preview.action, preview.has_changes);

    const feedback = $("diff-preview-feedback");
    feedback.hidden = true;
    feedback.textContent = "";
    if (preview.has_backup && preview.backup_at) {
      feedback.hidden = false;
      feedback.textContent = `已存在原配置备份（${formatDate(preview.backup_at)}），支持随时还原。`;
    }

    const confirmBtn = $("btn-confirm-diff-apply");
    confirmBtn.disabled = false;
    confirmBtn.textContent = isRestore
      ? (preview.has_backup ? "确认从备份还原" : "确认撤销接入")
      : (preview.scope === "global" ? "我已确认 Diff，写入全局配置" : "确认写入");

    $("diff-preview-modal").hidden = false;
  }

  async function previewAgentChange(agent, action) {
    const reqProject = state.project || "";
    const preview = await api("/api/agents/preview", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        agent_id: agent.id,
        project: reqProject,
        action,
      }),
    });
    preview.project = reqProject;
    openDiffModal(preview);
  }

  async function applyAgentToggleDirect(agent, enable, checkbox) {
    // Kept as a thin compatibility wrapper: all agent mutations now go through
    // the same preview + preview_id confirmation path.
    if (checkbox) checkbox.disabled = true;
    try {
      await previewAgentChange(agent, enable ? "install" : "restore");
    } catch (error) {
      alert(`预览失败：${error.message}`);
      await loadAgents();
    } finally {
      if (checkbox) checkbox.disabled = false;
    }
  }

  async function confirmDiffApply() {
    const preview = state.diffPreview;
    if (!preview) return;
    const confirmBtn = $("btn-confirm-diff-apply");
    const feedback = $("diff-preview-feedback");
    confirmBtn.disabled = true;
    const isRestore = preview.action === "restore";
    confirmBtn.textContent = isRestore ? "正在还原…" : "正在备份并写入…";
    const boundProject = typeof preview.project === "string" ? preview.project : (state.project || "");

    try {
      if (preview.kind === "agent") {
        await api("/api/agents/toggle", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            agent_id: preview.agent_id,
            project: boundProject,
            enable: !isRestore,
            preview_id: preview.preview_id,
            confirmed: true,
            expected_hashes: preview.base_hashes || undefined,
          }),
        });
      } else if (preview.kind === "shared_skill") {
        await api("/api/shared-skill", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            content: preview.reset ? "" : (preview.content || ""),
            reset: Boolean(preview.reset),
            sync_enabled: preview.sync_enabled !== false,
            project: boundProject,
            preview_id: preview.preview_id,
            confirmed: true,
          }),
        });
      } else if (preview.kind === "guard_hook") {
        await api("/api/guard/toggle", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            project: boundProject,
            hook_enabled: !isRestore,
            preview_id: preview.preview_id,
            confirmed: true,
          }),
        });
      } else {
        const endpoint = preview.kind === "skill"
          ? (isRestore ? "/api/skills/restore" : "/api/skills/install")
          : (isRestore ? "/api/mcp-clients/restore" : "/api/mcp-clients/install");
        await api(endpoint, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            client_id: preview.client_id || preview.target_id,
            target_id: preview.target_id || preview.client_id,
            project: boundProject,
            preview_id: preview.preview_id,
            confirmed: true,
            expected_hashes: preview.base_hashes || undefined,
          }),
        });
      }
      closeDiffModal();
      await loadAgents();
      renderOnboardingCard();
    } catch (error) {
      feedback.hidden = false;
      feedback.textContent = `操作失败：${error.message}`;
      confirmBtn.disabled = false;
      confirmBtn.textContent = isRestore ? "确认还原" : "确认写入";
    }
  }

  function renderSharedSkillBox(shared) {
    const openBtn = $("btn-open-shared-skill");
    const badge = $("shared-skill-badge");
    const pathEl = $("shared-skill-path");
    const editor = $("shared-skill-editor");
    if (!shared) return;
    if (openBtn) {
      openBtn.textContent = shared.is_custom ? "共享护栏指令 · 已自定义" : "共享护栏指令";
    }
    if (badge) {
      badge.textContent = shared.is_custom ? "已自定义" : "默认";
      badge.className = `pill ${shared.is_custom ? "pass" : "accent"}`;
    }
    if (pathEl) {
      pathEl.textContent = shortPathDisplay(shared.path || "~/.slopguard/shared-skill.md");
      pathEl.title = shared.path || "";
    }
    if (editor && document.activeElement !== editor) {
      editor.value = shared.content || "";
    }
  }

  function openSharedSkillModal() {
    if (state.agentsData?.shared_skill) {
      renderSharedSkillBox(state.agentsData.shared_skill);
    }
    const feedback = $("shared-skill-feedback");
    if (feedback) feedback.textContent = "保存后将自动同步至已勾选的 Agent";
    $("shared-skill-modal").hidden = false;
  }

  function closeSharedSkillModal() {
    $("shared-skill-modal").hidden = true;
  }

  function pendingAIMistakesForPrompt() {
    return (Array.isArray(state.mistakes) ? state.mistakes : []).filter(
      (m) => normalizeReviewStatus(m) === "pending_ai"
    );
  }

  function buildAgentPromptText(mode) {
    const projectPath = state.project || "<请在 SlopGuard 顶部选择具体项目路径>";
    const pendingAI = pendingAIMistakesForPrompt();
    if (mode === "session") {
      return [
        `请使用当前项目已接入的 \`slopguard\` MCP 工具，直接结合我们**本次对话上下文**和近期代码改动做一次踩坑复盘与防线沉淀：`,
        `1. 目标项目：\`${projectPath}\``,
        `2. 回顾本次对话中反复修改、编译/Lint/类型报错、或曾经想绕过检查的问题点（如未判空、类型逃逸、缺失导入、重复踩坑逻辑）；`,
        `3. 筛选 1~5 条值得长期拦截的真实隐患，依次调用 \`sg_record_mistake\` 写入错题（写清 \`category\`、\`summary\`、\`bad_snippet\`、\`good_snippet\`）；`,
        `4. 对每条错题紧接着调用 \`sg_submit_analysis\`：\`decision\` 设为 \`"recommend"\` 或 \`"skip"\`，用简洁通俗的中文写明 \`reason\`（哪里有问题、会有什么后果、为何入库），并在 \`rule_draft\` 给出可直接落地的配置规则草案；`,
        `5. 提交完成后用中文简明列出已写入的条数和结论，提醒我回 SlopGuard 控制台一键采纳规则（暂不自行修改业务代码）。`,
      ].join("\n");
    }
    if (mode === "history") {
      return [
        `请使用 \`slopguard\` MCP 工具并结合当前仓库的真实 Git 历史，完成一次高精度返工与隐患嗅探：`,
        `1. 目标项目：\`${projectPath}\``,
        `2. 调用 \`sg_radar\`（如 \`{"top": 10}\`）查看返工热点与跳过注释分布，同时只读查看近 14~30 天 \`git log\` / \`git show\` 里的 \`fix\`、\`revert\` 或同一模块反复修改的提交 diff；`,
        `3. 对比修改前后 diff，识别 1~5 个最容易复发的问题根因（而非仅仅抄 commit 标题），依次调用 \`sg_record_mistake\` 记录（务必填好 \`category\`、\`summary\`、修前 \`bad_snippet\`、修后 \`good_snippet\`）；`,
        `4. 对每条错题紧接着调用 \`sg_submit_analysis\`，写入 \`decision\`（\`"recommend"\` / \`"skip"\`）、通俗中文 \`reason\` 及可直接落地的 \`rule_draft\`；`,
        `5. 提交后汇报命中提交与沉淀的规则建议，提醒我回 SlopGuard 控制台一键采纳。`,
      ].join("\n");
    }
    if (mode === "verify") {
      return [
        `请使用 \`slopguard\` MCP 对当前项目改动执行门禁检查与错题闭环：`,
        `1. 目标项目：\`${projectPath}\``,
        `2. 调用 \`sg_verify\` 检查当前改动与跳过注释基线；`,
        `3. 若返回中存在 \`pending_inbox\` 待分析错题或新报错，请结合当前仓库源码评估后，逐条调用 \`sg_submit_analysis\` 提交中文 \`reason\`、\`decision\`（\`"recommend"\` / \`"skip"\`）与 \`rule_draft\`；`,
        `4. 汇报本次检查结果，若有阻塞项再给出修复建议。`,
      ].join("\n");
    }

    const lines = [
      `请使用 \`slopguard\` MCP 对当前项目的待审查错题执行分析与回传：`,
      `1. 目标项目：\`${projectPath}\``,
      `2. 调用 \`sg_list_inbox\`（参数：\`{"review_status": "pending_ai"}\`）读取当前待分析错题，并结合仓库实际代码核对上下文；`,
      `3. 逐条评估判断：第三方缓存/构建产物误报（如 \`.cache/\`、\`.venv/\`、\`node_modules/\`）或纯偶然单次失误设 \`decision="skip"\`；高频返工或真实隐患设 \`decision="recommend"\`；`,
      `4. 逐条调用 \`sg_submit_analysis\` 提交 \`fingerprint\`、\`decision\`、通俗中文 \`reason\` 及简明 \`rule_draft\`（不要擅自静默修改业务文件）；`,
      `5. 全部提交后告知我回 SlopGuard 控制台点击「一键采纳建议」。`,
    ];
    if (pendingAI.length > 0) {
      lines.push("", `当前面板待分析清单（共 ${pendingAI.length} 条，以下列出前 ${Math.min(8, pendingAI.length)} 条参考）：`);
      for (const m of pendingAI.slice(0, 8)) {
        const cleanSummary = String(m.summary || "").replace(/\s+/g, " ").trim().slice(0, 120);
        lines.push(`- [${m.fingerprint}] (${m.category || "lint"} ×${m.count || 1}) ${cleanSummary}`);
      }
    }
    return lines.join("\n");
  }

  const PROMPT_MODE_GUIDES = {
    history: "从近 30 天返工记录中提取高频根因，冷启动生成拦截规则。",
    session: "趁上下文鲜活，把本轮踩坑沉淀为错题与规则草案。",
    review: "对照源码过滤缓存误报，补全待审错题的采纳建议。",
    verify: "核查当前改动与跳过注释基线，闭环遗漏阻塞项。",
  };

  let appToastTimer = null;
  function showToast(message) {
    const el = $("app-toast");
    if (!el) return;
    el.textContent = message;
    el.hidden = false;
    if (appToastTimer) clearTimeout(appToastTimer);
    appToastTimer = setTimeout(() => {
      el.hidden = true;
    }, 2600);
  }

  function renderAgentPromptModalBody() {
    const mode = PROMPT_MODE_GUIDES[state.agentPromptMode] ? state.agentPromptMode : "history";
    document.querySelectorAll("#agent-prompt-modes .seg-btn").forEach((b) => {
      b.classList.toggle("active", b.dataset.promptMode === mode);
    });
    const metaEl = $("agent-prompt-meta");
    if (metaEl) {
      const agents = Array.isArray(state.agentsData?.agents) ? state.agentsData.agents : [];
      const anyEnabled = agents.some((a) => a.enabled);
      const projLabel = state.project ? shortProject(state.project) : "未选定项目";
      const refreshIcon = `<svg class="pill-action-icon" width="11" height="11" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M13.5 8a5.5 5.5 0 1 1-1.35-3.6"/><path d="M13.5 2.5v2.7h-2.7"/></svg>`;
      const mcpBtn =
        `<button type="button" id="agent-prompt-go-mcp" class="pill pill-action ${anyEnabled ? "pass" : "warn"}" data-tip="点击检查 MCP 接入" aria-label="点击检查 MCP 接入">` +
        `${anyEnabled ? "MCP 已配置" : "MCP 未配置"}${refreshIcon}</button>`;
      metaEl.innerHTML = `<span class="pill accent">项目：${escapeHTML(projLabel)}</span>${mcpBtn}`;
    }
    const guideEl = $("agent-prompt-guide");
    if (guideEl) {
      guideEl.textContent = PROMPT_MODE_GUIDES[mode] || "";
    }
    const textEl = $("agent-prompt-text");
    if (textEl) {
      textEl.value = buildAgentPromptText(mode);
    }
    const copyBtn = $("btn-copy-agent-prompt");
    if (copyBtn) {
      copyBtn.textContent = "复制提示词";
    }
  }

  async function openAgentPromptModal(preferredMode) {
    if (!state.agentsData) {
      await loadAgents().catch(() => {});
    }
    const pendingCnt = pendingAIMistakesForPrompt().length;
    state.agentPromptMode = preferredMode || (pendingCnt > 0 ? "review" : "history");
    renderAgentPromptModalBody();
    const feedback = $("agent-prompt-feedback");
    if (feedback) {
      feedback.textContent = "粘贴到 Agent 发送，完成后刷新本页采纳";
    }
    $("agent-prompt-modal").hidden = false;
  }

  function closeAgentPromptModal() {
    $("agent-prompt-modal").hidden = true;
  }

  async function copyAgentPromptText() {
    const textEl = $("agent-prompt-text");
    const btn = $("btn-copy-agent-prompt");
    const feedback = $("agent-prompt-feedback");
    if (!textEl) return;
    const text = textEl.value || "";
    let copied = false;
    try {
      await navigator.clipboard.writeText(text);
      copied = true;
    } catch {
      textEl.focus();
      textEl.select();
      copied = document.execCommand("copy");
    }
    if (!copied) {
      if (feedback) feedback.textContent = "自动复制失败，请全选上方文本框手动 Ctrl/Cmd+C 复制。";
      return;
    }
    if (btn) btn.textContent = "已复制 ✓";
    closeAgentPromptModal();
    showToast("已复制，粘贴到 Agent 发送即可");
    showInboxBanner(
      `<strong>提示词已复制：</strong>请直接粘贴到「${escapeHTML(shortProject(state.project))}」对应的 Agent 对话框发送，待其通过 MCP 回传后刷新即可一键采纳。`,
      `<button class="btn btn-secondary btn-sm js-refresh-after-prompt" type="button">刷新错题状态</button>`
    );
    $("session-scan-banner")?.querySelector(".js-refresh-after-prompt")?.addEventListener("click", () => refresh());
  }

  function formatAgentHealthPill(item, report) {
    const hs = report?.health_status || item.health_status || (item.enabled ? "pending_activation" : "not_configured");
    const detail = report?.diagnosis || item.health_detail || "";
    if (hs === "active") {
      return `<span class="pill pass" data-tip="${escapeHTML(detail)}">已激活 · 可用</span>`;
    }
    if (hs === "broken") {
      return `<span class="pill fail" data-tip="${escapeHTML(detail)}">配置失效</span>`;
    }
    if (hs === "pending_activation") {
      return `<span class="pill warn" data-tip="${escapeHTML(detail)}">待激活</span>`;
    }
    if (item.detected) {
      return '<span class="pill" data-tip="本机已安装此客户端，打开右侧开关即可接入">已检测</span>';
    }
    return "";
  }

  function renderAgentList(container, agents) {
    if (!container) return;
    container.innerHTML = "";
    if (!agents.length) {
      container.innerHTML = `<p class="empty-state">未探测到支持的 AI Agent</p>`;
      return;
    }

    for (const item of agents) {
      const row = document.createElement("div");
      row.className = "agent-row";

      const report = state.agentReports[item.id] || null;
      const statusPill = formatAgentHealthPill(item, report);
      const recPill = item.recommended ? '<span class="pill accent">推荐</span>' : "";
      const backupPill = item.has_backup ? '<span class="pill">已备份</span>' : "";
      const pathText = shortPathDisplay(item.primary_path || "");
      let diagHTML = "";
      if (report?.diagnosis) {
        const dCls = report.health_status === "active"
          ? "ok"
          : (report.health_status === "broken" ? "fail" : "warn");
        diagHTML = `<div class="agent-diag ${dCls}">${escapeHTML(report.diagnosis)}</div>`;
      }

      row.innerHTML = `
        <div class="agent-row-main">
          <span class="agent-avatar">${escapeHTML(item.badge || "AG")}</span>
          <div class="agent-meta">
            <div class="agent-name-row">
              <span>${escapeHTML(item.name)}</span>
              ${recPill}
              ${statusPill}
              ${backupPill}
            </div>
            <div class="agent-path mono" title="${escapeHTML(item.primary_path || "")}">${escapeHTML(pathText)}</div>
            ${diagHTML}
          </div>
        </div>
        <div class="agent-row-controls">
          <button class="btn btn-sm btn-ghost js-verify-agent" type="button" ${item.needs_project ? "disabled" : ""} data-tip="实际运行一次 stdio MCP 握手并校验客户端接入状态">自检</button>
          <label class="switch" title="${item.needs_project ? "请先在顶部下拉框选择项目" : (item.enabled ? "关闭 / 还原配置" : "接入 SlopGuard")}">
            <input type="checkbox" ${item.enabled ? "checked" : ""} ${item.needs_project ? "disabled" : ""} aria-label="接入 ${escapeHTML(item.name)}">
            <span class="switch-slider"></span>
          </label>
          <button class="chevron-btn" type="button" title="预览 Diff 与还原" ${item.needs_project ? "disabled" : ""} aria-label="查看 ${escapeHTML(item.name)} 详情">›</button>
        </div>`;

      const verifyBtn = row.querySelector(".js-verify-agent");
      verifyBtn.addEventListener("click", () => verifyAgents(item.id, verifyBtn));

      const checkbox = row.querySelector('input[type="checkbox"]');
      checkbox.addEventListener("change", async () => {
        const wantEnable = checkbox.checked;
        if (item.scope === "global") {
          // Restore visual checkbox until user confirms in Diff modal.
          checkbox.checked = Boolean(item.enabled);
          try {
            await previewAgentChange(item, wantEnable ? "install" : "restore");
          } catch (error) {
            alert(`预览失败：${error.message}`);
          }
          return;
        }
        await applyAgentToggleDirect(item, wantEnable, checkbox);
      });

      row.querySelector(".chevron-btn").addEventListener("click", async () => {
        try {
          await previewAgentChange(item, item.enabled ? "restore" : "install");
        } catch (error) {
          alert(`预览失败：${error.message}`);
        }
      });

      container.appendChild(row);
    }
  }

  async function verifyAgents(agentID, triggerBtn) {
    const origText = triggerBtn ? triggerBtn.textContent : "";
    if (triggerBtn) {
      triggerBtn.disabled = true;
      triggerBtn.textContent = "自检中…";
    }
    try {
      const res = await api("/api/agents/verify", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ agent_id: agentID, project: state.project || "" }),
      });
      const reports = Array.isArray(res.reports) ? res.reports : [];
      for (const rep of reports) {
        if (rep.agent_id) {
          state.agentReports[rep.agent_id] = rep;
        }
      }
      if (state.agentsData?.agents) {
        renderAgentList($("agents-list"), state.agentsData.agents);
      } else {
        await loadAgents();
      }
    } catch (error) {
      alert(`自检失败：${error.message}`);
    } finally {
      if (triggerBtn) {
        triggerBtn.disabled = false;
        triggerBtn.textContent = origText || "自检";
      }
    }
  }

  async function loadAgents() {
    const listEl = $("agents-list");
    const hintEl = $("mcp-project-hint");
    if (!listEl) return;
    try {
      const data = await api(`/api/agents?${projectQuery()}`);
      state.agentsData = data;
      const agents = Array.isArray(data.agents) ? data.agents : [];
      if (hintEl) {
        if (!state.project) {
          hintEl.hidden = false;
          renderQuickProjectAction(hintEl, "先添加或选择项目即可启用「当前项目」一键接入");
        } else {
          hintEl.hidden = true;
          hintEl.innerHTML = "";
        }
      }
      renderSharedSkillBox(data.shared_skill || null);
      renderAgentList(listEl, agents);
    } catch (error) {
      listEl.innerHTML = `<p class="empty-state">加载失败：${escapeHTML(error.message)}</p>`;
    }
  }

  async function saveSharedSkill(reset = false) {
    const editor = $("shared-skill-editor");
    const feedback = $("shared-skill-feedback");
    const saveBtn = $("btn-save-shared-skill");
    const resetBtn = $("btn-reset-shared-skill");
    if (reset && !confirm("确认将共享护栏指令恢复为 SlopGuard 默认模板并同步已接入 Agent 吗？")) {
      return;
    }
    if (saveBtn) saveBtn.disabled = true;
    if (resetBtn) resetBtn.disabled = true;
    try {
      const preview = await api("/api/shared-skill/preview", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          content: editor ? editor.value : "",
          reset,
          sync_enabled: true,
          project: state.project || "",
        }),
      });
      preview.project = state.project || "";
      preview.reset = reset;
      openDiffModal(preview);
      if (feedback) feedback.textContent = "已生成 Diff 预览，请在弹窗中确认写入。";
    } catch (error) {
      if (feedback) feedback.textContent = `预览失败：${error.message}`;
    } finally {
      if (saveBtn) saveBtn.disabled = false;
      if (resetBtn) resetBtn.disabled = false;
    }
  }

  function openGenericMCPModal() {
    const bin = state.agentsData?.binary_path || "slopguard";
    const args = state.project ? ["mcp", "--root", state.project] : ["mcp"];
    const snippet = JSON.stringify({
      mcpServers: {
        slopguard: {
          command: bin,
          args,
        },
      },
    }, null, 2);
    $("generic-mcp-code").textContent = snippet;
    $("btn-copy-generic-mcp").textContent = "复制配置";
    $("generic-mcp-modal").hidden = false;
  }

  function closeGenericMCPModal() {
    $("generic-mcp-modal").hidden = true;
  }

  // 后台无感自动守护
  async function loadGuardStatus() {
    try {
      const data = await api(`/api/guard/status?${projectQuery()}`);
      state.guardStatus = data;
      renderGuardStatus(data);
    } catch (error) {
      console.error(error);
    }
  }

  function renderGuardStatus(data) {
    const topbarPill = $("topbar-guard-pill");
    const badge = $("guard-project-badge");
    const emptyEl = $("guard-empty-project");
    const controlsEl = $("guard-controls");
    const masterSwitchWrap = $("guard-master-switch-wrap");
    const masterSwitch = $("guard-master-switch");
    const masterPill = $("guard-master-pill");

    const guardedCount = Number(data?.guarded_projects || 0);
    const st = data?.status || null;

    if (!state.project || !st) {
      if (badge) badge.textContent = "未选项目";
      if (emptyEl) {
        emptyEl.hidden = false;
        renderQuickProjectAction(emptyEl, "尚未添加项目");
      }
      if (controlsEl) controlsEl.hidden = true;
      if (masterSwitchWrap) masterSwitchWrap.hidden = true;
      if (masterPill) masterPill.hidden = true;
      if (topbarPill) {
        if (guardedCount > 0) {
          topbarPill.hidden = false;
          topbarPill.textContent = `已守护 ${guardedCount}`;
          topbarPill.className = "pill pass";
        } else {
          topbarPill.hidden = true;
        }
      }
      return;
    }

    if (badge) badge.textContent = shortProject(state.project);
    if (emptyEl) emptyEl.hidden = true;
    if (controlsEl) controlsEl.hidden = false;
    if (masterSwitchWrap) masterSwitchWrap.hidden = false;

    const active = Boolean(st.enabled || st.hook_installed);
    if (topbarPill) {
      topbarPill.hidden = !active;
      topbarPill.textContent = st.running ? "体检中…" : "守护中";
      topbarPill.className = `pill ${st.running ? "accent" : "pass"}`;
    }

    if (masterSwitch) {
      masterSwitch.disabled = false;
      masterSwitch.checked = active;
    }
    if (masterPill) {
      masterPill.hidden = false;
      masterPill.textContent = st.running ? "体检中…" : (active ? "守护中" : "未开启");
      masterPill.className = `pill ${st.running ? "accent" : (active ? "pass" : "")}`;
    }

    const watchSwitch = $("guard-watch-switch");
    const watchPill = $("guard-watch-pill");
    const watchDetail = $("guard-watch-detail");
    if (watchSwitch) {
      watchSwitch.disabled = false;
      watchSwitch.checked = Boolean(st.enabled);
    }
    if (watchPill) {
      if (st.running) {
        watchPill.textContent = "体检中…";
        watchPill.className = "pill accent";
      } else if (st.enabled) {
        watchPill.textContent = "监听中";
        watchPill.className = "pill pass";
      } else {
        watchPill.textContent = "未开启";
        watchPill.className = "pill";
      }
    }
    if (watchDetail) {
      if (st.last_error) {
        watchDetail.hidden = false;
        watchDetail.textContent = `异常：${st.last_error}`;
      } else if (st.last_scan_at) {
        watchDetail.hidden = false;
        const outcomeText = st.last_passed === false ? `发现 ${st.last_findings || 0} 项` : "通过";
        watchDetail.textContent = `最近 (${formatShortTime(st.last_scan_at)})：${outcomeText}`;
      } else {
        watchDetail.hidden = true;
        watchDetail.textContent = "";
      }
    }

    const hookSwitch = $("guard-hook-switch");
    const hookPill = $("guard-hook-pill");
    const hookDetail = $("guard-hook-detail");
    if (!st.is_git_repo) {
      if (hookSwitch) {
        hookSwitch.checked = false;
        hookSwitch.disabled = true;
      }
      if (hookPill) {
        hookPill.textContent = "非 Git 仓库";
        hookPill.className = "pill";
      }
      if (hookDetail) {
        hookDetail.hidden = true;
        hookDetail.textContent = "";
      }
    } else {
      if (hookSwitch) {
        hookSwitch.disabled = false;
        hookSwitch.checked = Boolean(st.hook_installed);
      }
      if (st.hook_installed) {
        if (hookPill) {
          hookPill.textContent = "已安装";
          hookPill.className = "pill pass";
        }
        if (hookDetail) {
          hookDetail.hidden = false;
          hookDetail.textContent = shortPathDisplay(st.hook_path || ".git/hooks/post-commit");
          hookDetail.title = st.hook_path || "";
        }
      } else {
        if (hookPill) {
          hookPill.textContent = "未安装";
          hookPill.className = "pill warn";
        }
        if (hookDetail) {
          hookDetail.hidden = true;
          hookDetail.textContent = "";
        }
      }
    }
  }

  async function toggleGuard(patch) {
    if (!state.project) {
      alert("请先在左侧工作区选择一个具体项目。");
      return;
    }
    const switches = [$("guard-master-switch"), $("guard-watch-switch"), $("guard-hook-switch")].filter(Boolean);
    switches.forEach((s) => { s.disabled = true; });
    try {
      if (Object.prototype.hasOwnProperty.call(patch, "hook_enabled")) {
        const preview = await api("/api/guard/hook/preview", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ project: state.project, hook_enabled: Boolean(patch.hook_enabled) }),
        });
        preview.project = state.project;
        openDiffModal(preview);
        return;
      }
      await api("/api/guard/toggle", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ project: state.project, ...patch }),
      });
      await Promise.all([loadGuardStatus(), loadProjects()]);
      renderOnboardingCard();
    } catch (error) {
      alert(`守护设置失败：${error.message}`);
      await loadGuardStatus();
    } finally {
      renderGuardStatus(state.guardStatus);
    }
  }

  // 只读本地 Agent 会话与 Git 返工嗅探
  function selectReviewTab(reviewStatus, userInitiated = true) {
    if (userInitiated) {
      state.userPickedReviewTab = true;
    }
    state.reviewStatus = reviewStatus ?? "";
    const isRulesView = state.reviewStatus === "rules_view";
    document.querySelectorAll("#mistakes-review-tabs .seg-tab").forEach((b) => {
      b.classList.toggle("active", (b.dataset.reviewStatus ?? "") === state.reviewStatus);
    });
    const mFilters = $("mistakes-sub-filters");
    const rFilters = $("rules-sub-filters");
    const mView = $("mistakes-view-wrap");
    const rView = $("rules-view-wrap");
    if (mFilters) mFilters.hidden = isRulesView;
    if (rFilters) rFilters.hidden = !isRulesView;
    if (mView) mView.hidden = isRulesView;
    if (rView) rView.hidden = !isRulesView;

    if (isRulesView) {
      applyRuleFilters();
    } else {
      applyMistakeFilters();
    }
  }

  async function applyAIRecommendations(triggerBtn) {
    const banner = $("session-scan-banner");
    const btn = triggerBtn || banner?.querySelector(".js-apply-ai-rec");
    if (btn) {
      btn.disabled = true;
      btn.textContent = "处理中…";
    }
    try {
      const res = await api("/api/mistakes/apply-recommendations", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ project: state.project || "" }),
      });
      showInboxBanner(`<strong>已一键采纳建议：</strong><strong>${res.applied_rules || 0}</strong> 条已转为生效规则，<strong>${res.ignored || 0}</strong> 条已移入已忽略。`);
      await refresh();
    } catch (error) {
      showInboxBanner(`采纳失败：${escapeHTML(error.message)}`);
    } finally {
      if (btn) btn.disabled = false;
    }
  }

  // 立即体检（结果同步写入总览与错题）
  async function runVerify() {
    if (state.verifying) return;
    const pill = $("topbar-verify-pill");
    if (!state.project) {
      if (pill) {
        pill.hidden = false;
        pill.textContent = "请先在左侧选择项目";
        pill.className = "topbar-status-pill";
      }
      return;
    }

    state.verifying = true;
    updateTopbarProject();
    if (pill) {
      pill.hidden = false;
      pill.textContent = "体检中…";
      pill.className = "topbar-status-pill accent";
    }

    try {
      const result = await api("/api/verify", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          project: state.project,
          all: false,
          ui: false,
          record: true,
        }),
      });
      const passed = Boolean(result.passed);
      const findingsCount = Array.isArray(result.findings) ? result.findings.length : 0;
      const badge = $("badge-verify");
      if (badge) {
        badge.textContent = passed ? "" : String(findingsCount || "!");
        badge.hidden = passed;
      }
      if (pill) {
        pill.hidden = false;
        pill.textContent = passed
          ? `体检通过 · ${result.duration_ms || 0}ms`
          : `发现 ${findingsCount} 项（已记入错题栏）`;
        pill.className = `topbar-status-pill ${passed ? "pass" : "fail"}`;
      }
      await refresh();
    } catch (error) {
      if (pill) {
        pill.hidden = false;
        pill.textContent = `体检失败：${error.message}`;
        pill.className = "topbar-status-pill fail";
      }
    } finally {
      state.verifying = false;
      updateTopbarProject();
    }
  }

  // 刷新周期
  async function refreshOverview() {
    const overview = await api(`/api/overview?${projectQuery({ granularity: state.granularity })}`);
    renderOverview(overview);
  }

  async function refreshRadar() {
    const radar = await api(`/api/radar?${projectQuery({ top: state.radarTop })}`);
    renderRadar(radar);
  }

  async function refresh() {
    $("refresh-button").disabled = true;
    try {
      const reqs = [
        api(`/api/overview?${projectQuery({ granularity: state.granularity })}`),
        api(`/api/mistakes?${projectQuery()}`),
        api(`/api/events?${projectQuery({ limit: 100 })}`).catch(() => ({ events: [] })),
        api(`/api/rules?${projectQuery()}`),
        api(`/api/radar?${projectQuery({ top: state.radarTop })}`),
        api("/api/projects"),
      ];
      if (state.project) {
        reqs.push(api(`/api/project-meta?${projectQuery()}`).catch(() => null));
      }
      const [overview, mistakes, eventsData, rules, radar, projectsData, meta] = await Promise.all(reqs);

      if (projectsData && Array.isArray(projectsData.projects)) {
        state.projects = projectsData.projects;
        renderProjectsTable(state.projects);
        updateTopbarProject();
      }
      renderOverview(overview);
      renderMistakes(Array.isArray(mistakes.mistakes) ? mistakes.mistakes : []);
      renderEvents(Array.isArray(eventsData?.events) ? eventsData.events : []);
      renderRules(Array.isArray(rules.rules) ? rules.rules : []);
      renderRadar(radar);
      renderProjectMeta(meta || null);
      await Promise.all([loadAgents(), loadGuardStatus()]);
      renderOnboardingCard();
    } catch (error) {
      console.error(error);
    } finally {
      $("refresh-button").disabled = false;
    }
  }

  window.slopguardSwitchTab = (tab) => switchTab(tab);

  async function start() {
    if (navigator.userAgent.includes("SlopGuardApp")) {
      document.body.classList.add("macos-frameless");
    }
    initFloatingTooltips();
    applyTheme(state.theme);
    const hashMatch = (window.location.hash || "").match(/tab=([a-z]+)/i);
    if (hashMatch) {
      state.tab = hashMatch[1];
    }
    switchTab(state.tab);

    // 主题按钮
    document.querySelectorAll(".theme-btn").forEach((btn) => {
      btn.addEventListener("click", () => applyTheme(btn.dataset.themeChoice));
    });

    // 导航标签
    document.querySelectorAll(".nav-item").forEach((btn) => {
      btn.addEventListener("click", () => switchTab(btn.dataset.tab));
    });

    // 移动端侧边栏
    $("sidebar-toggle").addEventListener("click", openSidebar);
    $("sidebar-close").addEventListener("click", closeSidebar);
    $("sidebar-backdrop").addEventListener("click", closeSidebar);

    // 趋势粒度
    document.querySelectorAll(".seg-btn[data-granularity]").forEach((btn) => {
      btn.addEventListener("click", async () => {
        state.granularity = btn.dataset.granularity;
        document.querySelectorAll(".seg-btn[data-granularity]").forEach((b) => {
          b.classList.toggle("active", b === btn);
        });
        await refreshOverview().catch(console.error);
      });
    });

    // 雷达热点数量
    $("radar-top-select").addEventListener("change", async (e) => {
      state.radarTop = e.target.value;
      await refreshRadar().catch(console.error);
    });

    // 错题与规则视图切换、Agent 对话提示词与本地历史嗅探
    document.querySelectorAll("#mistakes-review-tabs .seg-tab").forEach((btn) => {
      btn.addEventListener("click", () => selectReviewTab(btn.dataset.reviewStatus ?? "", true));
    });
    $("btn-agent-prompt")?.addEventListener("click", () => openAgentPromptModal());
    document.querySelectorAll("#agent-prompt-modes .seg-btn").forEach((btn) => {
      btn.addEventListener("click", () => {
        state.agentPromptMode = btn.dataset.promptMode || "history";
        renderAgentPromptModalBody();
      });
    });
    $("agent-prompt-close")?.addEventListener("click", closeAgentPromptModal);
    $("agent-prompt-backdrop")?.addEventListener("click", closeAgentPromptModal);
    $("btn-copy-agent-prompt")?.addEventListener("click", copyAgentPromptText);
    $("agent-prompt-meta")?.addEventListener("click", (e) => {
      const btn = e.target.closest?.("#agent-prompt-go-mcp");
      if (!btn) return;
      const tipEl = $("global-tooltip");
      if (tipEl) tipEl.hidden = true;
      closeAgentPromptModal();
      switchTab("verify");
      loadAgents().catch(() => {});
      $("agent-connect-card")?.scrollIntoView?.({ behavior: "smooth", block: "start" });
    });

    // 总览 KPI 卡片跳转
    $("stat-card-escapes")?.addEventListener("click", (e) => {
      if (e.target.closest?.(".help-tip")) return;
      const details = $("overview-radar-details");
      if (details) details.open = true;
      $("overview-radar-card")?.scrollIntoView({ behavior: "smooth", block: "start" });
    });
    $("stat-card-mistakes")?.addEventListener("click", (e) => {
      if (e.target.closest?.(".help-tip")) return;
      switchTab("mistakes");
      selectReviewTab("pending", true);
    });
    $("stat-card-rules")?.addEventListener("click", (e) => {
      if (e.target.closest?.(".help-tip")) return;
      switchTab("mistakes");
      selectReviewTab("rules_view", true);
    });

    // 错题栏下拉与搜索筛选
    ["mistakes-filter-category", "mistakes-filter-source"].forEach((id) => {
      $(id).addEventListener("change", applyMistakeFilters);
    });
    $("mistakes-filter-q").addEventListener("input", applyMistakeFilters);

    // Agent 活动筛选
    $("events-filter-tool").addEventListener("change", applyEventFilters);
    $("events-filter-q").addEventListener("input", applyEventFilters);

    // 规则筛选
    ["rules-filter-engine", "rules-filter-scope", "rules-filter-status"].forEach((id) => {
      $(id).addEventListener("change", applyRuleFilters);
    });

    // 体检操作（统一由顶栏触发）
    $("btn-run-verify").addEventListener("click", runVerify);

    // 零命令行交互：添加项目、新手引导、服务开关、基线操作、自动守护、安全 MCP/Skill 预览与还原
    document.querySelectorAll(".js-open-add-project").forEach((btn) => {
      btn.addEventListener("click", openAddProjectModal);
    });
    $("btn-onboard-add-project")?.addEventListener("click", openAddProjectModal);
    $("btn-onboard-guard")?.addEventListener("click", () => {
      switchTab("verify");
      $("auto-guard-card")?.scrollIntoView({ behavior: "smooth", block: "start" });
    });
    $("btn-onboard-agent")?.addEventListener("click", () => {
      switchTab("verify");
      $("agent-connect-card")?.scrollIntoView({ behavior: "smooth", block: "start" });
    });
    $("btn-dismiss-onboarding")?.addEventListener("click", () => {
      state.onboardingDismissed = true;
      localStorage.setItem("slopguard.onboardingDismissed", "1");
      $("onboarding-card").hidden = true;
    });

    $("add-project-close").addEventListener("click", closeAddProjectModal);
    $("add-project-cancel").addEventListener("click", closeAddProjectModal);
    $("add-project-backdrop").addEventListener("click", closeAddProjectModal);
    $("btn-pick-folder").addEventListener("click", pickFolder);
    $("btn-submit-add-project").addEventListener("click", submitAddProject);
    $("service-switch")?.addEventListener("change", (e) => toggleService(e.currentTarget.checked));
    $("btn-baseline-tighten").addEventListener("click", () => runBaselineAction("tighten"));
    $("btn-baseline-reset").addEventListener("click", () => runBaselineAction("reset"));
    $("btn-verify-all-agents")?.addEventListener("click", (e) => verifyAgents("all", e.currentTarget));
    $("btn-refresh-mcp").addEventListener("click", loadAgents);
    $("btn-open-shared-skill")?.addEventListener("click", openSharedSkillModal);
    $("shared-skill-close")?.addEventListener("click", closeSharedSkillModal);
    $("shared-skill-cancel")?.addEventListener("click", closeSharedSkillModal);
    $("shared-skill-backdrop")?.addEventListener("click", closeSharedSkillModal);
    $("btn-save-shared-skill")?.addEventListener("click", () => saveSharedSkill(false));
    $("btn-reset-shared-skill")?.addEventListener("click", () => saveSharedSkill(true));
    $("btn-rules-goto-mistakes")?.addEventListener("click", () => selectReviewTab("", true));
    $("btn-generic-mcp")?.addEventListener("click", openGenericMCPModal);
    $("generic-mcp-close")?.addEventListener("click", closeGenericMCPModal);
    $("generic-mcp-cancel")?.addEventListener("click", closeGenericMCPModal);
    $("generic-mcp-backdrop")?.addEventListener("click", closeGenericMCPModal);
    $("btn-copy-generic-mcp")?.addEventListener("click", async () => {
      const btn = $("btn-copy-generic-mcp");
      try {
        await navigator.clipboard.writeText($("generic-mcp-code").textContent || "");
        btn.textContent = "已复制";
        setTimeout(() => { btn.textContent = "复制配置"; }, 1800);
      } catch {
        btn.textContent = "复制失败";
      }
    });
    $("guard-master-switch")?.addEventListener("change", (e) => {
      const checked = e.currentTarget.checked;
      const st = state.guardStatus?.status;
      const patch = { enabled: checked };
      if (st?.is_git_repo) patch.hook_enabled = checked;
      toggleGuard(patch);
    });
    $("guard-watch-switch")?.addEventListener("change", (e) => {
      toggleGuard({ enabled: e.currentTarget.checked });
    });
    $("guard-hook-switch")?.addEventListener("change", (e) => {
      toggleGuard({ hook_enabled: e.currentTarget.checked });
    });
    $("diff-preview-close").addEventListener("click", closeDiffModal);
    $("diff-preview-cancel").addEventListener("click", closeDiffModal);
    $("diff-preview-backdrop").addEventListener("click", closeDiffModal);
    $("btn-confirm-diff-apply").addEventListener("click", confirmDiffApply);

    // Esc 关闭弹窗
    document.addEventListener("keydown", (e) => {
      if (e.key !== "Escape") return;
      if (!$("diff-preview-modal").hidden) {
        closeDiffModal();
      } else if (!$("agent-prompt-modal")?.hidden) {
        closeAgentPromptModal();
      } else if (!$("shared-skill-modal").hidden) {
        closeSharedSkillModal();
      } else if (!$("generic-mcp-modal").hidden) {
        closeGenericMCPModal();
      } else if (!$("add-project-modal").hidden) {
        closeAddProjectModal();
      }
    });

    // 项目切换（含「+ 添加项目…」选项）与刷新
    $("project-select").addEventListener("change", (e) => {
      if (e.target.value === "__add__") {
        e.target.value = state.project || "";
        openAddProjectModal();
        return;
      }
      selectProject(e.target.value);
    });
    $("refresh-button").addEventListener("click", refresh);

    await Promise.all([loadProjects(), loadServiceStatus()]);
    await refresh();
  }

  document.addEventListener("DOMContentLoaded", start);
})();
