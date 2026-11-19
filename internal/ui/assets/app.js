(() => {
  "use strict";

  const state = {
    projects: [],
    project: localStorage.getItem("slopguard.project") || "",
    tab: localStorage.getItem("slopguard.tab") || "overview",
    theme: localStorage.getItem("slopguard.theme") || "system",
    granularity: "day",
    radarTop: "10",
    reviewStatus: "pending_user",
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
    diffPreview: null,
    userPickedReviewTab: false,
    onboardingDismissed: localStorage.getItem("slopguard.onboardingDismissed") === "1",
  };

  const $ = (id) => document.getElementById(id);

  async function api(path, options) {
    const response = await fetch(path, options);
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

  // 导航与侧边栏
  function switchTab(tab) {
    const validTabs = ["overview", "mistakes", "events", "rules", "radar", "verify"];
    const target = validTabs.includes(tab) ? tab : "overview";
    state.tab = target;
    localStorage.setItem("slopguard.tab", target);
    document.querySelectorAll(".nav-item").forEach((btn) => {
      btn.classList.toggle("active", btn.dataset.tab === target);
    });
    document.querySelectorAll(".tab-panel").forEach((panel) => {
      panel.hidden = panel.id !== `tab-${target}`;
    });
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
    const btn = $("btn-toggle-service");
    try {
      const data = await api("/api/service-status");
      serviceInstalled = Boolean(data && data.installed);
      if (serviceInstalled) {
        dot.className = "health-dot pass";
        text.textContent = "后台常驻";
        text.title = `已安装开机后台常驻 (${data.platform || "local"}${data.port ? ` · :${data.port}` : ""})`;
        if (btn) btn.textContent = "关闭常驻";
      } else {
        dot.className = "health-dot neutral";
        text.textContent = "按需运行";
        text.title = "未安装后台常驻服务，当前仅在打开应用时运行";
        if (btn) btn.textContent = "开启常驻";
      }
    } catch {
      dot.className = "health-dot neutral";
      text.textContent = "按需运行";
    }
  }

  async function toggleService() {
    const btn = $("btn-toggle-service");
    if (!btn) return;
    btn.disabled = true;
    try {
      await api("/api/service/toggle", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ enable: !serviceInstalled }),
      });
      await loadServiceStatus();
    } catch (error) {
      alert(`操作失败：${error.message}`);
    } finally {
      btn.disabled = false;
    }
  }

  // 项目列表
  function updateAddProjectButtonStyle() {
    const addBtn = $("btn-open-add-project");
    if (!addBtn) return;
    const isEmpty = state.projects.length === 0;
    addBtn.className = `btn ${isEmpty ? "btn-primary" : "btn-secondary"} btn-add-project`;
  }

  async function loadProjects() {
    try {
      const data = await api("/api/projects");
      state.projects = Array.isArray(data.projects) ? data.projects : [];
      const select = $("project-select");
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

      if (state.project && !state.projects.some((p) => p.project_path === state.project)) {
        state.project = "";
      }
      select.value = state.project;
      updateAddProjectButtonStyle();
      updateTopbarProject();
      renderProjectsTable(state.projects);
    } catch (error) {
      console.error(error);
    }
  }

  function updateTopbarProject() {
    const pathEl = $("current-project-path");
    const dot = $("project-health-dot");
    const verifyBtn = $("btn-run-verify");

    if (!state.project) {
      pathEl.textContent = "全部项目";
      pathEl.title = "全部项目";
      dot.className = "health-dot neutral";
      if (verifyBtn) {
        verifyBtn.disabled = true;
        verifyBtn.dataset.tip = "请先选择或添加项目";
        verifyBtn.title = "请先选择或添加项目";
      }
      return;
    }

    pathEl.textContent = shortPathDisplay(state.project);
    pathEl.title = state.project;
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

  // 错题栏（含 Agent 分析建议 + 人工校验）
  function populateSelectOptions(selectEl, values, currentVal) {
    const existing = Array.from(new Set(values.filter(Boolean))).sort();
    selectEl.innerHTML = '<option value="">全部</option>';
    for (const val of existing) {
      const opt = document.createElement("option");
      opt.value = val;
      opt.textContent = val;
      selectEl.appendChild(opt);
    }
    selectEl.value = existing.includes(currentVal) ? currentVal : "";
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
    let candidateCount = 0;
    for (const m of mistakes) {
      const rs = normalizeReviewStatus(m);
      if (counts[rs] !== undefined) counts[rs]++;
      if ((rs === "pending_user" || rs === "pending_ai") && (m.is_candidate || Number(m.count || 0) >= 2)) {
        candidateCount++;
      }
    }

    $("count-pending-user").textContent = String(counts.pending_user);
    $("count-pending-ai").textContent = String(counts.pending_ai);
    $("count-all-mistakes").textContent = String(mistakes.length);
    $("count-approved").textContent = String(counts.approved);
    $("count-ignored").textContent = String(counts.ignored);

    const actionNeeded = counts.pending_user + counts.pending_ai;
    $("stat-candidates").textContent = actionNeeded.toLocaleString();
    $("stat-candidates").className = "stat-value " + (actionNeeded > 0 ? "warn" : "");
    $("stat-candidates-sub").textContent = `待校验 ${counts.pending_user} · 待分析 ${counts.pending_ai}`;

    const badge = $("badge-mistakes");
    badge.textContent = String(counts.pending_user || actionNeeded);
    badge.hidden = actionNeeded === 0;

    const batchBtn = $("btn-copy-batch-analyze");
    if (batchBtn) {
      batchBtn.hidden = counts.pending_ai === 0;
      batchBtn.textContent = `批量复制分析指令 (${counts.pending_ai})`;
    }

    populateSelectOptions($("mistakes-filter-category"), mistakes.map((m) => m.category), $("mistakes-filter-category").value);
    populateSelectOptions($("mistakes-filter-source"), mistakes.map((m) => m.source), $("mistakes-filter-source").value);

    if (!state.userPickedReviewTab && state.reviewStatus === "pending_user" && counts.pending_user === 0 && counts.pending_ai > 0) {
      selectReviewTab("pending_ai", false);
      return;
    }

    applyMistakeFilters();
  }

  function buildAnalyzePrompt(m) {
    const fp = m.fingerprint || "";
    const summary = m.summary || "未命名错题";
    return `请调用 SlopGuard MCP 工具 sg_list_inbox 查看错题详情（fingerprint: ${fp}，摘要：${summary}），分析其是否值得沉淀为防线规则，并用通俗中文说明原因后调用 sg_submit_analysis 提交分析建议。`;
  }

  function applyMistakeFilters() {
    const reviewF = state.reviewStatus;
    const catF = $("mistakes-filter-category").value;
    const srcF = $("mistakes-filter-source").value;
    const q = $("mistakes-filter-q").value.trim().toLowerCase();

    const filtered = state.mistakes.filter((m) => {
      const rs = normalizeReviewStatus(m);
      if (reviewF && rs !== reviewF) return false;
      if (catF && m.category !== catF) return false;
      if (srcF && m.source !== srcF) return false;
      if (q) {
        const hay = `${m.summary || ""} ${m.fingerprint || ""} ${m.bad_snippet || ""} ${m.good_snippet || ""} ${m.ai_reason || ""} ${m.category || ""}`.toLowerCase();
        if (!hay.includes(q)) return false;
      }
      return true;
    });

    $("mistakes-summary").textContent = `显示 ${filtered.length} / 共 ${state.mistakes.length} 条`;
    const list = $("mistakes-list");
    const empty = $("mistakes-empty");
    list.innerHTML = "";
    if (!filtered.length) {
      empty.hidden = false;
      return;
    }
    empty.hidden = true;

    const statusBadgeMap = {
      pending_user: { cls: "pending", text: "待我校验" },
      pending_ai: { cls: "", text: "待 Agent 分析" },
      approved: { cls: "pass", text: "已确认入库" },
      ignored: { cls: "ignored", text: "已忽略" },
    };

    for (const m of filtered) {
      const count = Number(m.count || 0);
      const isCandidate = Boolean(m.is_candidate) || count >= 2;
      const rs = normalizeReviewStatus(m);
      const badgeInfo = statusBadgeMap[rs] || { cls: "", text: rs };

      const card = document.createElement("article");
      card.className = "mistake-card" + (rs === "pending_user" || (isCandidate && rs === "pending_ai") ? " candidate" : "");

      const snippets = [];
      if (m.bad_snippet) {
        snippets.push(`<div class="snippet-box"><span class="snippet-label">错误示例</span><pre class="snippet bad">${escapeHTML(m.bad_snippet)}</pre></div>`);
      }
      if (m.good_snippet) {
        snippets.push(`<div class="snippet-box"><span class="snippet-label">修复参考</span><pre class="snippet good">${escapeHTML(m.good_snippet)}</pre></div>`);
      }

      // Agent 分析建议区
      let aiAdviceHTML = "";
      if (rs === "pending_user" || m.ai_decision || m.ai_reason) {
        const decision = String(m.ai_decision || "").toLowerCase();
        const isApproveRec = decision === "recommend" || decision === "approve" || decision === "promote" || decision === "recommend_approve";
        const decisionBadge = decision
          ? `<span class="pill ${isApproveRec ? "warn" : "ignored"}">${isApproveRec ? "建议入库" : "建议忽略"}</span>`
          : "";
        const ruleDraftHTML = m.ai_rule_draft
          ? `<pre class="code-block">${escapeHTML(m.ai_rule_draft)}</pre>`
          : "";
        aiAdviceHTML = `
          <div class="ai-advice">
            <div class="ai-advice-head">
              <span class="ai-advice-title">Agent 分析建议 ${decisionBadge}</span>
              ${m.ai_analyzed_at ? `<span class="muted mono">${escapeHTML(formatShortTime(m.ai_analyzed_at))}</span>` : ""}
            </div>
            <p class="ai-advice-reason">${escapeHTML(m.ai_reason || "Agent 已完成分析，请结合代码上下文确认是否入库为规则。")}</p>
            ${ruleDraftHTML}
          </div>`;
      } else if (rs === "pending_ai") {
        const hintHTML = m.local_hint
          ? `<p class="local-hint-line"><strong>初步解读：</strong>${escapeHTML(m.local_hint)}</p>`
          : "";
        aiAdviceHTML = `
          <div class="ai-advice pending-ai">
            <div class="ai-advice-head">
              <div class="title-with-tip">
                <span class="ai-advice-title">待 Agent 深入分析</span>
                <button type="button" class="help-tip" data-tip="若已接入 MCP 并配合 Agent 调用 sg_verify，Agent 会自动分析；也可复制指令主动让 Agent 评估是否沉淀为规则" aria-label="分析说明">?</button>
              </div>
              <button class="btn btn-secondary btn-sm copy-prompt-btn" type="button">复制指令</button>
            </div>
            ${hintHTML}
          </div>`;
      }

      // 人工校验操作按钮
      const actionBtns = [];
      if (rs !== "approved") {
        actionBtns.push(`<button class="btn btn-primary btn-sm" data-action="approve" data-fp="${escapeHTML(m.fingerprint)}" type="button">确认入库</button>`);
      }
      if (rs !== "ignored") {
        actionBtns.push(`<button class="btn btn-ghost btn-sm" data-action="ignore" data-fp="${escapeHTML(m.fingerprint)}" type="button">忽略</button>`);
      } else {
        actionBtns.push(`<button class="btn btn-secondary btn-sm" data-action="restore" data-fp="${escapeHTML(m.fingerprint)}" type="button">恢复</button>`);
      }

      card.innerHTML = `
        <div class="mistake-count" title="出现 ${count} 次">${count}</div>
        <div class="mistake-main">
          <div class="mistake-title-row">
            <h3 class="mistake-title">${escapeHTML(m.summary || "未命名错题")}</h3>
            <span class="pill ${badgeInfo.cls}">${escapeHTML(badgeInfo.text)}</span>
            ${isCandidate ? `<span class="pill warn">高频候选 &ge;2</span>` : ""}
          </div>
          <div class="mistake-meta">
            <span>${escapeHTML(m.category || "general")}</span>
            <span class="pill">${escapeHTML(formatSourceLabel(m.source))}</span>
            <span class="pill">出现 ${count} 次</span>
            ${m.fingerprint ? `<code class="pill mono" title="${escapeHTML(m.fingerprint)}">${escapeHTML(m.fingerprint.slice(0, 12))}</code>` : ""}
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

      const copyBtn = card.querySelector(".copy-prompt-btn");
      if (copyBtn) {
        copyBtn.addEventListener("click", async () => {
          const prompt = buildAnalyzePrompt(m);
          try {
            await navigator.clipboard.writeText(prompt);
            copyBtn.textContent = "已复制";
            setTimeout(() => { copyBtn.textContent = "复制指令"; }, 1800);
          } catch {
            copyBtn.textContent = "复制失败";
          }
        });
      }

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

    populateSelectOptions($("events-filter-tool"), events.map((e) => e.tool || e.tool_name), $("events-filter-tool").value);
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
        <td><code class="pill mono">${escapeHTML(tool)}</code></td>
        <td class="args-summary" title="${escapeHTML(detailText)}">${escapeHTML(detailText)}</td>
        <td><span class="pill ${passed ? "pass" : "fail"}">${passed ? "通过" : "已拦截"}</span></td>
        <td class="num">${findings}</td>
        <td class="num">${duration}ms</td>`;
      body.appendChild(tr);
    }
  }

  // 规则台账
  function renderRules(rules) {
    state.rules = rules;
    const badge = $("badge-rules");
    badge.textContent = String(rules.length);
    badge.hidden = rules.length === 0;

    populateSelectOptions($("rules-filter-engine"), rules.map((r) => r.engine), $("rules-filter-engine").value);
    populateSelectOptions($("rules-filter-status"), rules.map((r) => r.status), $("rules-filter-status").value);

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
      skipBanner.innerHTML = `<strong>跳过 ${skipped.length} 个维度：</strong> ` +
        skipped.map((s) => `<span class="mono">${escapeHTML(s.dimension || "?")}/${escapeHTML(s.tool || "?")}</span> (${escapeHTML(s.reason || "不可用")})`).join(" · ");
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

    const badgeRadar = $("badge-radar");
    if (baseline.available) {
      badgeRadar.textContent = baseline.passed ? "正常" : "超标";
      badgeRadar.hidden = false;
    } else {
      badgeRadar.hidden = true;
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
          <span class="mono">${escapeHTML(m.name)}</span>
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
      : preview.kind === "skill" ? "Skill / 规则" : "MCP 配置";
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
    const preview = await api("/api/agents/preview", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        agent_id: agent.id,
        project: state.project || "",
        action,
      }),
    });
    openDiffModal(preview);
  }

  async function applyAgentToggleDirect(agent, enable, checkbox) {
    if (checkbox) checkbox.disabled = true;
    try {
      await api("/api/agents/toggle", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          agent_id: agent.id,
          project: state.project || "",
          enable: Boolean(enable),
          confirmed: true,
        }),
      });
      await loadAgents();
      renderOnboardingCard();
    } catch (error) {
      alert(`操作失败：${error.message}`);
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

    try {
      if (preview.kind === "agent") {
        await api("/api/agents/toggle", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            agent_id: preview.agent_id,
            project: state.project || "",
            enable: !isRestore,
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
            project: state.project || "",
            confirmed: true,
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
    const badge = $("shared-skill-badge");
    const pathEl = $("shared-skill-path");
    const editor = $("shared-skill-editor");
    if (!shared) return;
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

      const statusPill = item.enabled
        ? '<span class="pill pass">已接入</span>'
        : (item.partial ? '<span class="pill warn">部分接入</span>' : '');
      const recPill = item.recommended ? '<span class="pill accent">推荐</span>' : "";
      const backupPill = item.has_backup ? '<span class="pill">已备份</span>' : "";
      const subtitle = item.note
        ? `${shortPathDisplay(item.primary_path || "")} · ${item.note}`
        : shortPathDisplay(item.primary_path || "");

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
            <div class="agent-path mono" title="${escapeHTML(item.primary_path || "")}">${escapeHTML(subtitle)}</div>
          </div>
        </div>
        <div class="agent-row-controls">
          <label class="switch" title="${item.needs_project ? "请先在左侧选择项目" : (item.enabled ? "关闭 / 还原配置" : "接入 SlopGuard")}">
            <input type="checkbox" ${item.enabled ? "checked" : ""} ${item.needs_project ? "disabled" : ""} aria-label="接入 ${escapeHTML(item.name)}">
            <span class="switch-slider"></span>
          </label>
          <button class="chevron-btn" type="button" title="预览 Diff 与还原" ${item.needs_project ? "disabled" : ""} aria-label="查看 ${escapeHTML(item.name)} 详情">›</button>
        </div>`;

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
      const res = await api("/api/shared-skill", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          content: editor ? editor.value : "",
          reset,
          sync_enabled: true,
          project: state.project || "",
        }),
      });
      if (editor && res.content !== undefined) {
        editor.value = res.content;
      }
      const syncedCount = Array.isArray(res.synced) ? res.synced.length : 0;
      if (feedback) {
        feedback.textContent = reset
          ? `已恢复默认模板${syncedCount ? `并同步 ${syncedCount} 处规则文件` : ""}`
          : `已保存自定义指令${syncedCount ? `并同步 ${syncedCount} 处规则文件` : ""}`;
      }
      await loadAgents();
    } catch (error) {
      if (feedback) feedback.textContent = `保存失败：${error.message}`;
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

    const guardedCount = Number(data?.guarded_projects || 0);
    const st = data?.status || null;

    if (!state.project || !st) {
      if (badge) badge.textContent = "未选项目";
      if (emptyEl) {
        emptyEl.hidden = false;
        renderQuickProjectAction(emptyEl, "尚未添加项目");
      }
      if (controlsEl) controlsEl.hidden = true;
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

    const active = Boolean(st.enabled || st.hook_installed);
    if (topbarPill) {
      topbarPill.hidden = !active;
      topbarPill.textContent = st.running ? "体检中…" : "守护中";
      topbarPill.className = `pill ${st.running ? "accent" : "pass"}`;
    }

    const masterSwitch = $("guard-master-switch");
    const masterPill = $("guard-master-pill");
    const masterDetail = $("guard-master-detail");
    if (masterSwitch) masterSwitch.checked = active;
    if (masterPill) {
      masterPill.textContent = st.running ? "体检中…" : (active ? "守护中" : "未开启");
      masterPill.className = `pill ${st.running ? "accent" : (active ? "pass" : "")}`;
    }
    if (masterDetail) {
      if (st.last_error) {
        masterDetail.textContent = `最近异常：${st.last_error}`;
      } else if (st.last_scan_at) {
        const outcomeText = st.last_passed === false ? `发现 ${st.last_findings || 0} 项` : "通过";
        masterDetail.textContent = `最近触发 (${formatShortTime(st.last_scan_at)})：${outcomeText}`;
      } else if (active) {
        const parts = [];
        if (st.enabled) parts.push("文件监听已就绪");
        if (st.hook_installed) parts.push("Git 钩子已安装");
        masterDetail.textContent = parts.join(" · ");
      } else {
        masterDetail.textContent = "开启后将在代码改动或提交时静默留痕错题";
      }
    }

    const watchSwitch = $("guard-watch-switch");
    const watchPill = $("guard-watch-pill");
    const watchDetail = $("guard-watch-detail");
    if (watchSwitch) watchSwitch.checked = Boolean(st.enabled);
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
    document.querySelectorAll("#mistakes-review-tabs .review-pill").forEach((b) => {
      b.classList.toggle("active", (b.dataset.reviewStatus ?? "") === state.reviewStatus);
    });
    applyMistakeFilters();
  }

  async function scanLocalSessions() {
    const btn = $("btn-scan-sessions");
    const banner = $("session-scan-banner");
    if (!btn) return;
    btn.disabled = true;
    btn.textContent = "嗅探中…";
    try {
      const res = await api("/api/sessions/scan", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ project: state.project || "", days: 14 }),
      });
      const sc = res.scanned || {};
      if (banner) {
        banner.hidden = false;
        banner.innerHTML = `<strong>嗅探完成（近 ${res.days || 14} 天）：</strong>` +
          `Git ${sc.git_commits || 0} 提交 · Claude ${sc.claude_files || 0} · Codex ${sc.codex_files || 0} · Antigravity ${sc.antigravity_files || 0}；` +
          `提取 <strong>${res.found || 0}</strong> 条，入库 <strong>${res.imported || 0}</strong> 条。`;
      }
      await refresh();
      if ((res.imported || 0) > 0 && state.reviewStatus === "pending_user") {
        const hasPendingUser = state.mistakes.some((m) => normalizeReviewStatus(m) === "pending_user");
        if (!hasPendingUser) {
          selectReviewTab("pending_ai", false);
        }
      }
    } catch (error) {
      if (banner) {
        banner.hidden = false;
        banner.textContent = `嗅探失败：${error.message}`;
      }
    } finally {
      btn.disabled = false;
      btn.textContent = "嗅探会话";
    }
  }

  async function copyBatchAnalyzePrompt() {
    const pending = state.mistakes.filter((m) => normalizeReviewStatus(m) === "pending_ai");
    if (!pending.length) return;
    const sample = pending.slice(0, 15).map((m, idx) => `${idx + 1}. [${m.fingerprint || ""}] ${m.summary || "未命名错题"}`).join("\n");
    const prompt = `请调用 SlopGuard MCP 工具 sg_list_inbox 获取待分析错题，逐条评估是否值得沉淀为防线规则，并用通俗中文说明原因后调用 sg_submit_analysis 提交分析建议（当前待分析 ${pending.length} 条）：\n${sample}`;
    const btn = $("btn-copy-batch-analyze");
    try {
      await navigator.clipboard.writeText(prompt);
      if (btn) {
        btn.textContent = `已复制 ${pending.length} 条指令`;
        setTimeout(() => {
          btn.textContent = `批量复制分析指令 (${pending.length})`;
        }, 2000);
      }
    } catch {
      if (btn) btn.textContent = "复制失败";
    }
  }

  // 立即体检（结果同步写入总览与错题栏）
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
        updateAddProjectButtonStyle();
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

      $("last-updated").textContent = `更新于 ${new Date().toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit", second: "2-digit" })}`;
    } catch (error) {
      console.error(error);
      $("last-updated").textContent = `错误：${error.message}`;
    } finally {
      $("refresh-button").disabled = false;
    }
  }

  window.slopguardSwitchTab = (tab) => switchTab(tab);

  async function start() {
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

    // 错题栏审核状态筛选胶囊与会话嗅探
    document.querySelectorAll("#mistakes-review-tabs .review-pill").forEach((btn) => {
      btn.addEventListener("click", () => selectReviewTab(btn.dataset.reviewStatus ?? "", true));
    });
    $("btn-scan-sessions").addEventListener("click", scanLocalSessions);

    // 总览 KPI 卡片跳转
    $("stat-card-escapes")?.addEventListener("click", (e) => {
      if (e.target.closest?.(".help-tip")) return;
      switchTab("radar");
    });
    $("stat-card-mistakes")?.addEventListener("click", (e) => {
      if (e.target.closest?.(".help-tip")) return;
      switchTab("mistakes");
      const pendUser = state.mistakes.filter((m) => normalizeReviewStatus(m) === "pending_user").length;
      const pendAI = state.mistakes.filter((m) => normalizeReviewStatus(m) === "pending_ai").length;
      if (pendUser === 0 && pendAI > 0) {
        selectReviewTab("pending_ai", false);
      }
    });
    $("stat-card-rules")?.addEventListener("click", (e) => {
      if (e.target.closest?.(".help-tip")) return;
      switchTab("rules");
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
    $("btn-copy-batch-analyze")?.addEventListener("click", copyBatchAnalyzePrompt);

    // 零命令行交互：添加项目、新手引导、服务开关、基线操作、自动守护、安全 MCP/Skill 预览与还原
    $("btn-open-add-project").addEventListener("click", openAddProjectModal);
    $("btn-verify-add-project")?.addEventListener("click", openAddProjectModal);
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
    $("btn-toggle-service").addEventListener("click", toggleService);
    $("btn-baseline-tighten").addEventListener("click", () => runBaselineAction("tighten"));
    $("btn-baseline-reset").addEventListener("click", () => runBaselineAction("reset"));
    $("btn-refresh-mcp").addEventListener("click", loadAgents);
    $("btn-toggle-shared-skill")?.addEventListener("click", () => {
      const drawer = $("shared-skill-drawer");
      const btn = $("btn-toggle-shared-skill");
      if (!drawer) return;
      drawer.hidden = !drawer.hidden;
      if (btn) btn.textContent = drawer.hidden ? "查看 / 编辑 ›" : "收起编辑 ⌄";
    });
    $("btn-save-shared-skill")?.addEventListener("click", () => saveSharedSkill(false));
    $("btn-reset-shared-skill")?.addEventListener("click", () => saveSharedSkill(true));
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
      } else if (!$("generic-mcp-modal").hidden) {
        closeGenericMCPModal();
      } else if (!$("add-project-modal").hidden) {
        closeAddProjectModal();
      }
    });

    // 项目切换与刷新
    $("project-select").addEventListener("change", (e) => selectProject(e.target.value));
    $("refresh-button").addEventListener("click", refresh);

    await Promise.all([loadProjects(), loadServiceStatus()]);
    await refresh();
  }

  document.addEventListener("DOMContentLoaded", start);
})();
