(() => {
  "use strict";

  const state = {
    projects: [],
    project: localStorage.getItem("slopguard.project") || "",
    overview: null,
    mistakes: [],
    rules: [],
    radar: null,
  };

  const $ = (id) => document.getElementById(id);

  async function api(path, options) {
    const response = await fetch(path, options);
    const data = await response.json().catch(() => ({}));
    if (!response.ok) {
      throw new Error(data.error || response.statusText || "request failed");
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
    return date.toLocaleString();
  }

  function shortProject(path) {
    if (!path) return "全部项目";
    const parts = String(path).split(/[\\/]/).filter(Boolean);
    return parts.length ? parts[parts.length - 1] : path;
  }

  function projectQuery(extra = {}) {
    const params = new URLSearchParams(extra);
    if (state.project) params.set("project", state.project);
    return params.toString();
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
        option.textContent = `${shortProject(project.project_path)}  ·  ${project.scan_count} 次`;
        option.title = project.project_path;
        select.appendChild(option);
      }
      if (state.project && !state.projects.some((p) => p.project_path === state.project)) {
        state.project = "";
      }
      select.value = state.project;
    } catch (error) {
      console.error(error);
    }
  }

  function renderOverview(overview) {
    state.overview = overview;
    $("stat-runs").textContent = Number(overview.total_runs || 0).toLocaleString();
    $("stat-failed").textContent = Number(overview.failed_runs || 0).toLocaleString();
    $("stat-rules").textContent = Number(overview.rule_count || 0).toLocaleString();
    $("stat-duration").textContent = Number(overview.avg_duration_ms || 0).toFixed(1);
    $("overview-scope").textContent = state.project ? shortProject(state.project) : "全部项目";
    renderTrend(Array.isArray(overview.trend) ? overview.trend : []);
  }

  function renderTrend(points) {
    const svg = $("trend-chart");
    const empty = $("trend-empty");
    svg.innerHTML = "";
    if (!points.length) {
      empty.hidden = false;
      return;
    }
    empty.hidden = true;

    const width = 1000;
    const height = 180;
    const left = 34;
    const right = 16;
    const top = 12;
    const bottom = 26;
    const plotW = width - left - right;
    const plotH = height - top - bottom;
    const maxEscapes = Math.max(1, ...points.map((p) => Number(p.escapes || 0)));
    const maxRuns = Math.max(1, ...points.map((p) => Number(p.runs || 0)));
    const n = points.length;
    const barW = Math.min(34, Math.max(7, (plotW / n) * 0.48));

    const grid = [];
    for (let i = 0; i <= 3; i++) {
      const y = top + (plotH * i) / 3;
      grid.push(`<line x1="${left}" y1="${y}" x2="${width - right}" y2="${y}" class="chart-grid" />`);
    }

    const bars = points.map((point, index) => {
      const x = left + (plotW / n) * (index + 0.5) - barW / 2;
      const value = Number(point.escapes || 0);
      const h = Math.max(1, (value / maxEscapes) * plotH);
      const y = top + plotH - h;
      return `<rect x="${x.toFixed(1)}" y="${y.toFixed(1)}" width="${barW.toFixed(1)}" height="${h.toFixed(1)}" rx="3" class="chart-bar"><title>${escapeHTML(point.bucket)} · escapes ${value}</title></rect>`;
    }).join("");

    const linePoints = points.map((point, index) => {
      const x = left + (plotW / n) * (index + 0.5);
      const value = Number(point.runs || 0);
      const y = top + plotH - (value / maxRuns) * plotH * 0.72;
      return `${x.toFixed(1)},${y.toFixed(1)}`;
    }).join(" ");
    const line = `<polyline points="${linePoints}" class="chart-line" fill="none" />`;

    const labels = points.map((point, index) => {
      if (n > 9 && index % Math.ceil(n / 8) !== 0 && index !== n - 1) return "";
      const x = left + (plotW / n) * (index + 0.5);
      return `<text x="${x.toFixed(1)}" y="${height - 7}" text-anchor="middle" class="chart-label">${escapeHTML(point.bucket)}</text>`;
    }).join("");

    svg.setAttribute("viewBox", `0 0 ${width} ${height}`);
    svg.innerHTML = `${grid}${bars}${line}${labels}`;
  }

  function renderMistakes(mistakes) {
    state.mistakes = mistakes;
    const list = $("mistakes-list");
    const empty = $("mistakes-empty");
    list.innerHTML = "";
    if (!mistakes.length) {
      empty.hidden = false;
      return;
    }
    empty.hidden = true;
    for (const mistake of mistakes) {
      const count = Number(mistake.count || 0);
      const isHot = count >= 2;
      const article = document.createElement("article");
      article.className = "mistake-card" + (isHot ? " hot" : "");
      const snippets = [];
      if (mistake.bad_snippet) {
        snippets.push(`<pre class="snippet bad">${escapeHTML(mistake.bad_snippet)}</pre>`);
      }
      if (mistake.good_snippet) {
        snippets.push(`<pre class="snippet good">${escapeHTML(mistake.good_snippet)}</pre>`);
      }
      const status = mistake.status || "pending";
      const statusClass = status === "pending" ? "pending" : status === "ignored" ? "ignored" : "active";
      article.innerHTML = `
        <div class="mistake-count">${count}</div>
        <div class="mistake-main">
          <p class="mistake-title">${escapeHTML(mistake.summary || "未命名错题")}</p>
          <div class="mistake-meta">
            <span class="pill ${statusClass}">${escapeHTML(status)}</span>
            ${escapeHTML(mistake.category || "other")} · ${escapeHTML(shortProject(mistake.project_path))}
            · ${formatDate(mistake.created_at)}
          </div>
          ${snippets.length ? `<div class="snippet-row">${snippets.join("")}</div>` : ""}
        </div>
        <div class="mistake-actions">
          ${status === "pending" ? `<button class="button button-ghost" data-fingerprint="${escapeHTML(mistake.fingerprint)}" type="button">标记 ignored</button>` : ""}
        </div>`;
      const ignore = article.querySelector("button[data-fingerprint]");
      if (ignore) {
        ignore.addEventListener("click", () => ignoreMistake(mistake.fingerprint, ignore));
      }
      list.appendChild(article);
    }
  }

  async function ignoreMistake(fingerprint, button) {
    if (!fingerprint) return;
    button.disabled = true;
    try {
      await api(`/api/mistakes/${encodeURIComponent(fingerprint)}/ignore`, { method: "POST" });
      await refresh();
    } catch (error) {
      button.disabled = false;
      button.textContent = "操作失败";
      button.title = error.message;
    }
  }

  function renderRules(rules) {
    state.rules = rules;
    const body = $("rules-body");
    const empty = $("rules-empty");
    body.innerHTML = "";
    if (!rules.length) {
      empty.hidden = false;
      return;
    }
    empty.hidden = true;
    for (const rule of rules) {
      const row = document.createElement("tr");
      const scope = rule.scope || "project";
      row.innerHTML = `
        <td><span class="pill ${scope === "global" ? "active" : ""}">${escapeHTML(scope)}</span></td>
        <td>${escapeHTML(rule.engine || "—")}</td>
        <td><code>${escapeHTML(rule.rule_id || "—")}</code></td>
        <td class="rule-reason">${escapeHTML(rule.rationale || "—")}</td>
        <td>${formatDate(rule.created_at)}</td>
        <td class="num">${Number(rule.hit_count || 0).toLocaleString()}</td>`;
      body.appendChild(row);
    }
  }

  function renderRadar(data) {
    state.radar = data;
    $("radar-project").textContent = state.project ? shortProject(state.project) : "选择项目查看";
    const baseline = data.baseline || {};
    const statePill = $("baseline-state");
    if (!baseline.available) {
      statePill.textContent = "未配置";
      statePill.className = "pill";
    } else {
      statePill.textContent = baseline.passed ? "PASS" : "FAIL";
      statePill.className = `pill ${baseline.passed ? "active" : "bad"}`;
    }
    const metricList = $("baseline-list");
    metricList.innerHTML = "";
    const metrics = Array.isArray(baseline.metrics) ? baseline.metrics : [];
    if (!metrics.length) {
      metricList.innerHTML = `<p class="empty">${baseline.available ? "baseline 中没有逃生舱指标。" : "尚未找到项目 baseline.json，先运行 slopguard init。"}</p>`;
    } else {
      const max = Math.max(1, ...metrics.map((metric) => Math.max(Number(metric.baseline || 0), Number(metric.current || 0))));
      for (const metric of metrics) {
        const width = Math.min(100, (Number(metric.current || 0) / max) * 100);
        const row = document.createElement("div");
        row.className = "metric-row";
        row.innerHTML = `<span class="metric-name">${escapeHTML(metric.name)}</span>
          <span class="metric-num">${Number(metric.baseline || 0)}</span>
          <span class="metric-num">${Number(metric.current || 0)}</span>
          <span class="metric-bar"><span class="${Number(metric.delta || 0) > 0 ? "over" : ""}" style="width:${width.toFixed(1)}%"></span></span>`;
        metricList.appendChild(row);
      }
    }

    const hotspots = Array.isArray(data.hotspots) ? data.hotspots : [];
    const hotspotList = $("hotspot-list");
    hotspotList.innerHTML = "";
    if (!hotspots.length) {
      hotspotList.innerHTML = `<p class="empty">暂无热点数据。</p>`;
    } else {
      for (const hotspot of hotspots) {
        const row = document.createElement("div");
        row.className = "hotspot-row";
        row.innerHTML = `<span class="hotspot-file" title="${escapeHTML(hotspot.file)}">${escapeHTML(hotspot.file)}</span>
          <span class="hotspot-score">${Number(hotspot.risk_score || 0)}</span>`;
        hotspotList.appendChild(row);
      }
    }

    const screenshots = Array.isArray(data.screenshots) ? data.screenshots : [];
    const grid = $("screenshot-grid");
    grid.innerHTML = "";
    if (!screenshots.length) {
      grid.innerHTML = `<p class="empty">暂无 UI 截图。安装 Playwright 后运行 <code>slopguard verify --ui</code>。</p>`;
    } else {
      for (const shot of screenshots) {
        const card = document.createElement("div");
        card.className = "screenshot-card";
        const img = document.createElement("img");
        img.alt = shot.path;
        img.loading = "lazy";
        img.src = `/api/screenshot?${projectQuery({ path: shot.path })}`;
        const label = document.createElement("span");
        label.textContent = `${shot.path} · ${formatDate(shot.modified_at)}`;
        card.append(img, label);
        grid.appendChild(card);
      }
    }
  }

  async function refresh() {
    $("refresh-button").disabled = true;
    try {
      const query = projectQuery();
      const [overview, mistakes, rules, radar] = await Promise.all([
        api(`/api/overview?${query}`),
        api(`/api/mistakes?${query}`),
        api(`/api/rules?${query}`),
        api(`/api/radar?${query}`),
      ]);
      renderOverview(overview);
      renderMistakes(Array.isArray(mistakes.mistakes) ? mistakes.mistakes : []);
      renderRules(Array.isArray(rules.rules) ? rules.rules : []);
      renderRadar(radar);
      $("last-updated").textContent = `更新于 ${new Date().toLocaleTimeString()}`;
    } catch (error) {
      console.error(error);
      $("last-updated").textContent = `加载失败：${error.message}`;
    } finally {
      $("refresh-button").disabled = false;
    }
  }

  async function start() {
    await loadProjects();
    await refresh();
    $("project-select").addEventListener("change", async (event) => {
      state.project = event.target.value;
      if (state.project) {
        localStorage.setItem("slopguard.project", state.project);
      } else {
        localStorage.removeItem("slopguard.project");
      }
      await refresh();
    });
    $("refresh-button").addEventListener("click", refresh);
  }

  document.addEventListener("DOMContentLoaded", start);
})();
