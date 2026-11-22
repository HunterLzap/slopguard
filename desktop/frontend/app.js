import { Call, Clipboard, Events, Window } from "/wails/runtime.js";

const bind = method => (...args) => Call.ByName("main.AppService." + method, ...args);
const api = {
  dashboard: bind("GetDashboardState"),
  register: bind("PickAndRegisterProject"),
  remove: bind("RemoveProject"),
  verify: bind("VerifyProject"),
  details: bind("GetProjectDetails"),
  toggleRule: bind("ToggleProjectRule"),
  deleteRule: bind("DeleteProjectRule"),
  toggleHook: bind("ToggleProjectHook"),
  approve: bind("ApproveProposal"),
  dismiss: bind("DismissProposal"),
  toggleAgent: bind("ToggleAgent"),
  snippets: bind("GetGenericMCPSnippets"),
  settings: bind("GetSettings"),
  save: bind("SaveSettings"),
  test: bind("TestLLMConnection"),
};

const $ = id => document.getElementById(id);
const languages = { typescript: "TypeScript", python: "Python", go: "Go", rust: "Rust", jvm: "JVM", dart: "Dart", cpp: "C / C++" };
const agents = { codex: "Codex", claude: "Claude Code", cursor: "Cursor", antigravity: "Antigravity", gemini: "Gemini CLI", dsh: "DeepSeek Harness", openclaw: "OpenClaw" };
const statuses = {
  PASS: ["pass", "检查通过"],
  FAIL: ["fail", "发现问题"],
  MISSING_TOOL: ["missing", "缺少检查工具"],
  UNSUPPORTED: ["neutral", "暂不支持此项目"],
  NO_CHANGES: ["unchanged", "暂无变更"],
};

let dashboard = null;
let dashboardRequest = 0;
let expandedPath = "";
let toastTimer;
let deleteSelection;
const cards = new Map();
const details = new Map();
const pending = new Set();
const switchValues = new Map();

function element(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text != null) node.textContent = text;
  return node;
}

function button(label, style, key, action) {
  const node = element("button", "button " + style, label);
  node.type = "button";
  node.disabled = pending.has(key);
  node.dataset.focus = key;
  node.addEventListener("click", action);
  return node;
}

function setSwitch(node, value) {
  node.setAttribute("aria-checked", String(value));
}

function toggle(label, enabled, key, action, unavailable = false) {
  const node = element("button", "switch");
  node.type = "button";
  node.setAttribute("aria-label", label);
  node.setAttribute("role", "switch");
  node.dataset.focus = key;
  node.disabled = unavailable || pending.has(key);
  setSwitch(node, switchValues.get(key) ?? enabled);
  node.addEventListener("click", () => {
    const next = node.getAttribute("aria-checked") !== "true";
    switchValues.set(key, next);
    void act(key, () => action(next));
  });
  return node;
}

function describe(error) {
  return typeof error === "string" ? error : error?.message || String(error);
}

function toast(message, tone = "success") {
  const node = $("toast");
  node.textContent = message;
  node.dataset.tone = tone;
  node.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { node.hidden = true; }, tone === "error" ? 5800 : 2800);
}

async function refreshDashboard() {
  const request = ++dashboardRequest;
  const state = await api.dashboard();
  if (request !== dashboardRequest) return;
  dashboard = state;
  if (!state.projects.some(project => project.path === expandedPath)) expandedPath = "";
  render();
}

function refresh() {
  refreshDashboard().then(() => expandedPath && readDetails(expandedPath)).catch(error => {
    if (!dashboard) {
      $("guard-state").lastElementChild.textContent = "读取失败";
    }
    toast(describe(error), "error");
  });
}

async function act(key, action, options = {}) {
  if (pending.has(key)) return;
  pending.add(key);
  render();
  try {
    const value = await action();
    if (!value?.cancelled && options.refresh !== false) await refreshDashboard();
    if (options.message && !value?.cancelled) {
      toast(typeof options.message === "function" ? options.message(value) : options.message);
    }
  } catch (error) {
    toast(describe(error), "error");
  } finally {
    pending.delete(key);
    switchValues.delete(key);
    render();
  }
}

function render() {
  $("add-project").disabled = pending.has("add-project");
  if (!dashboard) return;
  const focused = document.activeElement?.dataset.focus;
  const scroll = $("scroller").scrollTop;
  const guard = $("guard-state");
  guard.classList.toggle("running", dashboard.guard_running);
  guard.lastElementChild.textContent = dashboard.guard_running ? "守护运行中" : "守护未运行";

  $("proposal-section").hidden = dashboard.proposals.length === 0;
  $("proposals").replaceChildren(...dashboard.proposals.map(proposalCard));
  renderAgents();

  cards.clear();
  $("projects").replaceChildren(...(dashboard.projects.length
    ? dashboard.projects.map(projectCard)
    : [element("p", "empty", "暂无项目，添加文件夹后即可检查。")]));
  $("scroller").scrollTop = scroll;
  if (focused) {
    const match = [...document.querySelectorAll("[data-focus]")].find(node => node.dataset.focus === focused);
    match?.focus({ preventScroll: true });
  }
}

function proposalCard(proposal) {
  const project = dashboard.projects.find(item => item.path === proposal.project_path);
  const card = element("article", "proposal-card");
  const title = element("p", "proposal-project", project?.name ?? proposal.project_path);
  title.title = proposal.project_path;
  card.append(title, element("p", "proposal-summary", proposal.summary_zh));
  if (proposal.decision === "HARD_RULE") {
    card.append(element("span", "evidence",
      "精准拦截 " + proposal.sandbox_positive_hits + " 处 · 项目命中 " + proposal.sandbox_project_hits + " 处"));
  } else if (proposal.hint_line) {
    card.append(element("span", "evidence", proposal.hint_line));
  }

  const key = "proposal:" + proposal.id;
  const commands = element("div", "proposal-actions");
  commands.append(
    button("采纳写入", "primary", key, () => act(key, async () => {
      await api.approve(proposal.project_path, proposal.id);
      if (expandedPath === proposal.project_path) await readDetails(proposal.project_path);
    }, { message: "已采纳并写入" })),
    button("暂缓", "plain", key, () => act(key,
      () => api.dismiss(proposal.project_path, proposal.id, "snooze"), { message: "已暂缓" })),
    button("忽略", "plain", key, () => act(key,
      () => api.dismiss(proposal.project_path, proposal.id, "dismiss"), { message: "已忽略" })),
  );
  card.append(commands);
  return card;
}

function renderAgents() {
  const contents = $("agents");
  if (dashboard.agent_error) {
    contents.replaceChildren(element("p", "empty message-error", dashboard.agent_error));
    return;
  }
  if (dashboard.agents.length === 0) {
    contents.replaceChildren(element("p", "empty", "未探测到可接入的 Agent。"));
    return;
  }
  contents.replaceChildren(...dashboard.agents.map(agent => {
    const row = element("div", "agent-row");
    const name = agents[agent.id] ?? agent.id;
    row.title = agent.config_path;
    row.append(element("span", "agent-name", name),
      toggle(name + " 接入", agent.enabled, "agent:" + agent.id,
        enabled => api.toggleAgent(agent.id, enabled)));
    return row;
  }));
}

function projectCard(project, index) {
  const key = project.path;
  const card = element("article", "project-card");
  const title = element("button", "project-toggle");
  title.type = "button";
  title.title = key;
  title.dataset.focus = "details:" + key;
  const name = element("span", "project-name-group");
  const [tone, label] = statuses[project.last_status] ?? ["neutral", "尚未检查"];
  const status = element("span", "project-status " + tone);
  status.title = project.last_status || label;
  status.append(element("i", "dot"), element("span", "", label));
  name.append(element("span", "project-name", project.name), status);
  const chevron = element("i", "chevron");
  chevron.setAttribute("aria-hidden", "true");
  title.append(name, chevron);
  title.addEventListener("click", () => openProject(key));

  const tags = element("div", "project-languages");
  for (const language of project.languages ?? []) tags.append(element("span", "language-tag", languages[language] ?? language));

  const actions = element("div", "project-actions");
  const hook = element("span", "inline-switch");
  hook.append(element("span", "", "Git Hook"),
    toggle(project.name + " Git Hook", project.hook_enabled, "hook:" + key,
      enabled => api.toggleHook(key, enabled), Boolean(project.error)));
  if (project.hook_error) hook.title = project.hook_error;
  const verify = button(pending.has("scan:" + key) ? "检查中…" : "立即检查", "", "scan:" + key, () => act("scan:" + key, async () => {
    const result = await api.verify(key, "all");
    if (expandedPath === key) await readDetails(key);
    return result;
  }, { message: result => statuses[result.status]?.[1] ?? result.status }));
  verify.disabled ||= Boolean(project.error);
  actions.append(element("span", "rule-count", project.error ? "规则状态不可用" : project.applied_rules + " 条生效规则"), hook, verify);

  const box = element("div", "details");
  box.id = "project-detail-" + index;
  title.setAttribute("aria-controls", box.id);
  const clip = element("div", "detail-clip");
  const body = element("div", "detail-body");
  clip.append(body);
  box.append(clip);
  card.append(title, tags, actions, box);
  cards.set(key, { project, title, box, body });
  displayDetails(key);
  return card;
}

function openProject(path) {
  const previous = expandedPath;
  expandedPath = previous === path ? "" : path;
  if (previous) displayDetails(previous);
  if (!expandedPath) return;
  displayDetails(path);
  void readDetails(path);
}

async function readDetails(path) {
  const request = {};
  details.set(path, request);
  displayDetails(path);
  try {
    const data = await api.details(path);
    if (details.get(path) === request) details.set(path, { data });
  } catch (error) {
    if (details.get(path) === request) details.set(path, { error: describe(error) });
  }
  displayDetails(path);
}

function displayDetails(path) {
  const card = cards.get(path);
  if (!card) return;
  const open = expandedPath === path;
  card.title.setAttribute("aria-expanded", String(open));
  card.box.classList.toggle("open", open);
  card.box.inert = !open;
  if (!open) return;
  const { body, project } = card;
  const view = details.get(path);
  body.replaceChildren();

  if (project.error || view?.error) body.append(element("p", "message-error", view?.error ?? project.error));
  if (project.hook_error) body.append(element("p", "message-error", project.hook_error));
  if (!view?.data && !view?.error && !project.error) body.append(element("p", "message-muted", "正在读取项目详情…"));
  if (view?.data) {
    body.append(recentDiagnostics(view.data));
    body.append(element("h3", "", "项目规则"));
    if (view.data.rules.length) {
      const list = element("ul");
      for (const rule of view.data.rules) list.append(ruleRow(path, rule));
      body.append(list);
    } else {
      body.append(element("p", "message-muted", "暂无规则。"));
    }
  }
  body.append(button("移除登记", "plain remove-project", "remove:" + path, () => act("remove:" + path,
    () => api.remove(path), { message: "已移除项目登记" })));
}

function recentDiagnostics(project) {
  const container = document.createDocumentFragment();
  container.append(element("h3", "", "最近诊断"));
  const diagnosticList = element("ul");
  for (const missing of project.missing_tools) {
    const row = element("li", "missing-tool", "缺少 " + missing.name);
    row.append(element("code", "", missing.install_command));
    diagnosticList.append(row);
  }
  for (const item of project.diagnostics) {
    const row = element("li", "", item.message);
    const location = item.file ? item.file + (item.line > 0 ? ":" + item.line : "") : "";
    const detail = [item.checker, location].filter(Boolean).join(" · ");
    if (detail) row.append(element("span", "diagnostic-file", detail));
    diagnosticList.append(row);
  }
  if (diagnosticList.childElementCount) {
    container.append(diagnosticList);
  } else {
    const label = project.last_status === "MISSING_TOOL" ? "本次扫描未保存工具详情。"
      : project.last_scan_at ? "暂无诊断。" : "尚未检查。";
    container.append(element("p", "message-muted", label));
  }
  if (project.omitted_diagnostics > 0) {
    container.append(element("p", "message-muted", "另有 " + project.omitted_diagnostics + " 条诊断"));
  }
  for (const warning of project.warnings) container.append(element("p", "message-muted", warning));
  return container;
}

function ruleRow(path, rule) {
  const key = "rule:" + path + ":" + rule.rule_id;
  const row = element("li", "rule-row");
  row.append(element("span", "rule-name", rule.rule_id),
    toggle(rule.rule_id + " 规则启用", rule.enabled, key, async enabled => {
      applyRules(path, await api.toggleRule(path, rule.rule_id, enabled));
    }),
    button("删除", "plain danger", key, () => {
      deleteSelection = { key, path, id: rule.rule_id };
      $("confirm-name").textContent = rule.rule_id;
      $("confirm-rule").returnValue = "cancel";
      $("confirm-rule").showModal();
    }));
  return row;
}

function applyRules(path, rules) {
  const data = details.get(path)?.data;
  if (data) details.set(path, { data: { ...data, rules } });
}

$("confirm-rule").addEventListener("close", () => {
  const item = deleteSelection;
  deleteSelection = undefined;
  if (item && $("confirm-rule").returnValue === "delete") {
    void act(item.key, async () => {
      applyRules(item.path, await api.deleteRule(item.path, item.id));
    }, { message: "已删除规则" });
  }
});

$("add-project").addEventListener("click", () => act("add-project", api.register, { message: result =>
  result.already_exists ? "已登记，保留原项目配置" : "已添加项目" }));

const copy = $("mcp-copy");
$("copy-mcp").addEventListener("click", () => copy.showModal());
for (const kind of ["json", "toml"]) {
  const node = $("copy-" + kind);
  node.addEventListener("click", async () => {
    const buttons = [...copy.querySelectorAll("button")];
    for (const button of buttons) button.disabled = true;
    try {
      const snippets = await api.snippets();
      await Clipboard.SetText(snippets[kind]);
      copy.close();
      toast("已复制 " + kind.toUpperCase());
    } catch (error) {
      copy.close();
      toast(describe(error), "error");
    } finally {
      for (const button of buttons) button.disabled = false;
    }
  });
}
copy.addEventListener("click", event => { if (event.target === copy) copy.close(); });

const settings = $("settings-drawer");
const settingsForm = $("settings-form");
const settingsControls = [...settingsForm.querySelectorAll("input, button")];
let settingsOpen = 0;
let autostartUnavailable = false;

function settingsBusy(busy) {
  for (const control of settingsControls) control.disabled = busy;
  $("autostart").disabled ||= autostartUnavailable;
}

function settingsMessage(message, tone) {
  const node = $("settings-message");
  node.hidden = !message;
  node.textContent = message || "";
  node.dataset.tone = tone || "";
}

function applySettings(settings) {
  $("base-url").value = settings.base_url;
  $("api-key").value = settings.api_key;
  $("model").value = settings.model;
  setSwitch($("autostart"), settings.autostart);
  autostartUnavailable = Boolean(settings.autostart_error);
  settingsMessage(settings.autostart_error, settings.autostart_error ? "error" : "");
}

function setKeyVisibility(visible) {
  $("api-key").type = visible ? "text" : "password";
  $("show-key").textContent = visible ? "隐藏" : "显示";
  $("show-key").setAttribute("aria-pressed", String(visible));
}

$("open-settings").addEventListener("click", async () => {
  const request = ++settingsOpen;
  autostartUnavailable = false;
  settings.showModal();
  settingsBusy(true);
  settingsMessage("正在读取设置…");
  try {
    const value = await api.settings();
    if (request !== settingsOpen) return;
    applySettings(value);
    settingsBusy(false);
    $("base-url").focus();
  } catch (error) {
    if (request === settingsOpen) settingsMessage(describe(error), "error");
  }
});

$("close-settings").addEventListener("click", () => settings.close());
settings.addEventListener("close", () => {
  settingsOpen++;
  setKeyVisibility(false);
  $("api-key").value = "";
});
settings.addEventListener("click", event => { if (event.target === settings) settings.close(); });
$("show-key").addEventListener("click", () => setKeyVisibility($("api-key").type === "password"));
$("autostart").addEventListener("click", () =>
  setSwitch($("autostart"), $("autostart").getAttribute("aria-checked") !== "true"));

function readSettingsForm() {
  return [$("base-url").value, $("api-key").value, $("model").value];
}

$("test-connection").addEventListener("click", async () => {
  if (!settingsForm.reportValidity()) return;
  const request = settingsOpen;
  settingsBusy(true);
  settingsMessage("正在测试连通性…");
  try {
    await api.test(...readSettingsForm());
    if (request === settingsOpen) settingsMessage("连接成功", "success");
  } catch (error) {
    if (request === settingsOpen) settingsMessage(describe(error), "error");
  } finally {
    if (request === settingsOpen) settingsBusy(false);
  }
});

settingsForm.addEventListener("submit", async event => {
  event.preventDefault();
  const request = settingsOpen;
  settingsBusy(true);
  try {
    const value = await api.save(...readSettingsForm(), $("autostart").getAttribute("aria-checked") === "true");
    if (request !== settingsOpen) return;
    if (value.autostart_error) {
      applySettings(value);
      settingsMessage("API 设置已保存。开机自启：" + value.autostart_error, "error");
    } else {
      settings.close();
      toast("设置已保存");
    }
  } catch (error) {
    if (request === settingsOpen) settingsMessage(describe(error), "error");
  } finally {
    if (request === settingsOpen) settingsBusy(false);
  }
});

let desiredHeight = 560;
let resizeTimer;
const resize = new ResizeObserver(() => {
  clearTimeout(resizeTimer);
  resizeTimer = setTimeout(() => {
    const measured = $("topbar").offsetHeight + $("sections").offsetHeight;
    const height = Math.max(520, Math.min(640, measured));
    if (height === desiredHeight) return;
    desiredHeight = height;
    Window.SetSize(420, height).catch(error => toast(describe(error), "error"));
  }, 80);
});
resize.observe($("sections"));

Events.On("slopguard:dashboard-changed", refresh);
refresh();
