// Page: 概览 — readiness checklist, metrics, resources and token usage.
import * as store from "../store.js";
import { $, $$, bindTabs, el, emptyRow, formatNumber, timeOnly } from "../ui.js";
import { t, onChange } from "../i18n.js";

function renderChecklist() {
  const config = store.get("config");
  const monitor = store.get("monitor");
  if (!config || !monitor) return;
  const resources = monitor.resources;
  const models = resources.models || {};
  const modelDescription = models.stale
    ? t(
        `目录来自${models.cache_source || "磁盘"}快取，等待后台刷新。`,
        "guide.models.cached",
        models.cache_source || "磁盘",
      )
    : t("上游目录已完成首次刷新且有可暴露模型。", "guide.models.ready");
  const checks = [
    [
      t("配置本地 Key", "guide.step.serverkey"),
      t("客户端使用 Server Key 访问 Gateway。", "guide.step.serverkey.desc"),
      (config.server_keys || []).length > 0,
    ],
    [
      t("选择上游通道", "guide.step.upstream"),
      t("至少有 Zen / Go Key，或启用匿名通道。", "guide.step.upstream.desc"),
      (config.zen_keys || []).length + (config.go_keys || []).length > 0 || config.anonymous,
    ],
    [
      t("检查 Proxy", "guide.step.proxy"),
      t("至少一个代理节点健康，或 WARP 正在承载免费流量。", "guide.step.proxy.desc"),
      (resources.warp?.active || 0) > 0 ||
        (resources.proxies || []).some((proxy) => proxy.healthy),
    ],
    [
      t("等待模型目录", "guide.step.models"),
      modelDescription,
      models.exposed > 0 && !!models.updated_at,
    ],
  ];
  const card = $("#overview-checklist");
  const ready = checks.every(([, , done]) => done);
  card.classList.toggle("hidden", ready);
  if (ready) return;
  const host = $("#guide-steps");
  host.replaceChildren();
  checks.forEach(([title, description, done]) => {
    host.append(
      el("article", { class: "guide-step" }, [
        el("h3", { text: title }),
        el("p", { text: description }),
        el("div", {
          class: `step-state ${done ? "ok" : "warn"}`,
          text: done ? t("✓ 已就绪", "guide.done") : t("○ 待处理", "guide.todo"),
        }),
      ]),
    );
  });
}

function renderResources(resources) {
  const proxies = $("#proxy-table");
  proxies.replaceChildren();
  (resources.proxies || []).forEach((proxy) => {
    proxies.append(
      el("tr", {}, [
        el("td", { text: proxy.address }),
        el("td", {
          class: `status ${proxy.healthy ? "ok" : "bad"}`,
          text: proxy.healthy ? t("正常", "state.healthy") : t("不可用", "state.unavailable"),
        }),
        el("td", {
          text: `Z ${proxy.zen_keys} · G ${proxy.go_keys} · A ${
            proxy.anonymous ? t("开", "common.on") : t("关", "common.off")
          }`,
        }),
      ]),
    );
  });
  const warp = resources.warp;
  if (warp?.enabled) {
    // WARP is not a proxy: show it beside them so the egress that carries
    // free-model traffic is visible without pretending it is a bound node.
    proxies.append(
      el("tr", {}, [
        el("td", { text: `WARP · ${warp.stack}` }),
        el("td", {
          class: `status ${warp.active > 0 ? "ok" : "bad"}`,
          text: warp.active > 0 ? t("正常", "state.healthy") : t("不可用", "state.unavailable"),
        }),
        el("td", {
          text: `${warp.active}/${warp.size} · ${(warp.egress || []).join(", ") || "-"}`,
        }),
      ]),
    );
  }
  if (!proxies.children.length) emptyRow(proxies, 3);
  const keys = $("#key-table");
  keys.replaceChildren();
  if (resources.anonymous) {
    keys.append(
      el("tr", {}, [
        el("td", { text: "anonymous" }),
        el("td", { text: "zen" }),
        el("td", { class: "status ok", text: t("已启用", "state.enabled") }),
      ]),
    );
  }
  (resources.keys || []).forEach((key) => {
    keys.append(
      el("tr", {}, [
        el("td", { text: key.id }),
        el("td", { text: key.tier }),
        el("td", {
          class: `status ${key.cooldown_until ? "warn" : "ok"}`,
          text: key.cooldown_until ? t("冷却中", "state.cooling") : t("可用", "state.available"),
        }),
      ]),
    );
  });
  if (!keys.children.length) emptyRow(keys, 3);
}

function renderBreakdowns(metrics) {
  const host = $("#breakdowns");
  host.replaceChildren();
  [
    [t("端点", "breakdown.endpoint"), metrics.endpoints],
    [t("状态码", "breakdown.status"), metrics.statuses],
    [t("模型", "breakdown.model"), metrics.models],
    ["Tier", metrics.tiers],
  ].forEach(([title, data]) => {
    const body = el("tbody");
    Object.entries(data || {})
      .sort((a, b) => b[1] - a[1])
      .slice(0, 10)
      .forEach(([key, value]) => {
        body.append(el("tr", {}, [el("td", { text: key }), el("td", { class: "num", text: value })]));
      });
    if (!body.children.length) emptyRow(body, 2, t("暂无请求", "breakdown.empty"));
    host.append(
      el("div", { class: "breakdown" }, [
        el("h3", { text: title }),
        el("div", { class: "table-wrap" }, [el("table", { class: "table" }, [body])]),
      ]),
    );
  });
}

function renderSummary(monitor) {
  const metrics = monitor.metrics;
  $("#m-total").textContent = formatNumber(metrics.last_hour.total);
  $("#m-rate").textContent = `${((metrics.last_hour.success_rate || 0) * 100).toFixed(1)}%`;
  $("#m-p95").textContent = `${metrics.last_hour.p95_ms || 0} ms`;
  $("#m-active").textContent = `${metrics.active_requests || 0} / ${metrics.active_streams || 0}`;
  renderResources(monitor.resources);
  renderBreakdowns(metrics);
}

function renderTrend(metrics) {
  const trend = $("#usage-trend");
  const series = metrics?.series || [];
  const maximum = Math.max(1, ...series.map((point) => point.total_tokens || 0));
  trend.replaceChildren();
  series.forEach((point) => {
    const bar = el("div", { class: "trend-bar" });
    bar.style.height = `${Math.max(2, Math.round(((point.total_tokens || 0) / maximum) * 100))}%`;
    bar.title = `${timeOnly(point.minute)} · input ${formatNumber(point.input_tokens)} · output ${formatNumber(point.output_tokens)} · total ${formatNumber(point.total_tokens)}`;
    trend.append(bar);
  });
  $("#usage-trend-meta").textContent = series.length
    ? t(
        `峰值 ${formatNumber(maximum)} tokens / 分钟 · 近 ${series.length} 分钟`,
        "usage.trend.peak",
        formatNumber(maximum),
        series.length,
      )
    : t("尚无趋势数据", "usage.trend.empty");
}

function renderUsageModels(usage) {
  const host = $("#usage-models");
  host.replaceChildren();
  Object.entries(usage.models || {})
    .sort((a, b) => (b[1].total_tokens || 0) - (a[1].total_tokens || 0))
    .slice(0, 15)
    .forEach(([model, value]) => {
      host.append(
        el("tr", {}, [
          el("td", { class: "mono", text: model }),
          el("td", { class: "num", text: formatNumber(value.input_tokens) }),
          el("td", { class: "num", text: formatNumber(value.output_tokens) }),
          el("td", { class: "num", text: formatNumber(value.total_tokens) }),
        ]),
      );
    });
  if (!host.children.length) emptyRow(host, 4, t("尚无 Token 用量", "usage.empty.models"));
}

function renderTiers(usage) {
  const host = $("#usage-tiers");
  const entries = Object.entries(usage.tiers || {}).sort(
    (a, b) => (b[1].total_tokens || 0) - (a[1].total_tokens || 0),
  );
  const total = Math.max(1, ...entries.map(([, value]) => value.total_tokens || 0));
  host.replaceChildren();
  entries.forEach(([tier, value]) => {
    const fill = el("div", { class: "tier-fill" });
    fill.style.width = `${Math.round(((value.total_tokens || 0) / total) * 100)}%`;
    host.append(
      el("div", { class: "tier-row" }, [
        el("strong", { text: tier }),
        el("div", { class: "tier-track" }, [fill]),
        el("span", { class: "tier-value", text: formatNumber(value.total_tokens) }),
      ]),
    );
  });
  if (!entries.length) host.append(el("div", { class: "empty", text: t("尚无 Tier 用量", "usage.empty.tiers") }));
}

function renderUsage(monitor) {
  const usage = monitor.usage?.last_hour || {};
  const tokens = usage.tokens || {};
  $("#u-coverage").textContent = `${((usage.coverage || 0) * 100).toFixed(1)}%`;
  $("#u-total").textContent = formatNumber(tokens.total_tokens);
  $("#u-cached").textContent = formatNumber(tokens.cached_tokens);
  $("#u-reasoning").textContent = formatNumber(tokens.reasoning_tokens);
  renderTrend(monitor.metrics);
  renderUsageModels(usage);
  renderTiers(usage);
}

// Statistic tab: durable lifetime totals, a year heatmap, a daily trend and
// model bars. The server persists the aggregates in statistics.json.
const compactTokens = new Intl.NumberFormat("en-US", {
  notation: "compact",
  maximumFractionDigits: 1,
});
const statDayFormat = new Intl.DateTimeFormat("en-US", { month: "short", day: "numeric" });
const statMonthFormat = new Intl.DateTimeFormat("en-US", { month: "short" });
const reducedMotion = () => matchMedia("(prefers-reduced-motion: reduce)").matches;

let heatmapMode = "daily";
let trendRange = 7;
let heatmapCells = [];
let trendRangeBuilt = 0;
const modelRowParts = new Map();

function isoDay(date) {
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, "0")}-${String(
    date.getDate(),
  ).padStart(2, "0")}`;
}

function dayTotals(stats) {
  const totals = new Map();
  (stats.days || []).forEach((day) => totals.set(day.date, day.tokens?.total_tokens || 0));
  return totals;
}

// Counts up to target unless it is already shown; a newer call invalidates the
// previous animation so a poll cannot land a stale final value.
function animateNumber(node, target, format = formatNumber) {
  target = Number(target || 0);
  if (node.dataset.target === String(target)) return;
  const from = Number(node.dataset.target || 0);
  node.dataset.target = String(target);
  if (reducedMotion()) {
    node.textContent = format(target);
    return;
  }
  const sequence = (node.animationSequence = (node.animationSequence || 0) + 1);
  const started = performance.now();
  const step = (now) => {
    if (node.animationSequence !== sequence) return;
    const progress = Math.min(1, (now - started) / 650);
    const eased = 1 - Math.pow(1 - progress, 3);
    node.textContent = format(Math.round(from + (target - from) * eased));
    if (progress < 1) requestAnimationFrame(step);
  };
  requestAnimationFrame(step);
}

function renderStatMetrics(stats, entrance) {
  const lifetime = stats.lifetime || {};
  const total = lifetime.total_tokens || 0;
  $("#stat-empty").hidden = total > 0;
  if (entrance) {
    ["s-total", "s-peak", "s-sessions", "s-current", "s-longest"].forEach((id) => {
      const node = $(`#${id}`);
      node.dataset.target = "0";
      node.textContent = "0";
    });
  }
  animateNumber($("#s-total"), total, (value) => compactTokens.format(value));
  animateNumber($("#s-peak"), stats.peak_tokens || 0, (value) => compactTokens.format(value));
  animateNumber($("#s-sessions"), stats.useful_sessions || 0);
  animateNumber($("#s-current"), stats.current_streak || 0);
  animateNumber($("#s-longest"), stats.longest_streak || 0);
  $("#s-peak-note").textContent = stats.peak_day
    ? `${statDayFormat.format(new Date(`${stats.peak_day}T00:00:00`))} · busiest day`
    : "Busiest day";
}

function levelScale(values) {
  const sorted = values.filter((value) => value > 0).sort((a, b) => a - b);
  if (!sorted.length) return () => 0;
  const at = (percentile) => sorted[Math.min(sorted.length - 1, Math.floor(sorted.length * percentile))];
  const thresholds = [at(0.25), at(0.5), at(0.75)];
  return (value) =>
    value <= 0 ? 0 : value <= thresholds[0] ? 1 : value <= thresholds[1] ? 2 : value <= thresholds[2] ? 3 : 4;
}

function buildHeatmap() {
  const host = $("#token-heatmap");
  const months = $("#heatmap-months");
  const today = new Date();
  today.setHours(0, 0, 0, 0);
  const start = new Date(today);
  start.setDate(start.getDate() - 364);
  start.setDate(start.getDate() - start.getDay());
  const cells = [];
  const labels = [];
  let week = -1;
  let previousMonth = -1;
  for (
    let cursor = new Date(start);
    cursor <= today || cells.length % 7 !== 0;
    cursor.setDate(cursor.getDate() + 1)
  ) {
    if (cells.length % 7 === 0) {
      week++;
      if (cursor <= today && cursor.getMonth() !== previousMonth) {
        labels.push({ week, label: statMonthFormat.format(cursor) });
        previousMonth = cursor.getMonth();
      }
    }
    const valid = cursor <= today;
    cells.push({ date: valid ? isoDay(cursor) : "", node: el("i", { class: "heat-cell" }) });
  }
  host.replaceChildren();
  for (let index = 0; index < cells.length; index += 7) {
    const column = el("div", { class: "heatmap-week" });
    cells.slice(index, index + 7).forEach((cell) => column.append(cell.node));
    host.append(column);
  }
  cells.forEach((cell, index) => cell.node.style.setProperty("--i", index));
  months.replaceChildren();
  months.style.gridTemplateColumns = `repeat(${cells.length / 7}, minmax(0, 1fr))`;
  labels.forEach((item) => {
    const label = el("span", { text: item.label });
    label.style.gridColumn = String(item.week + 1);
    months.append(label);
  });
  heatmapCells = cells;
}

function renderHeatmap(stats, entrance) {
  const host = $("#token-heatmap");
  const todayKey = isoDay(new Date());
  if (!heatmapCells.length || host.dataset.day !== todayKey) {
    buildHeatmap();
    host.dataset.day = todayKey;
    entrance = true;
  }
  const totals = dayTotals(stats);
  const values = heatmapCells.map((cell) => (cell.date ? totals.get(cell.date) || 0 : 0));
  if (heatmapMode === "weekly") {
    for (let index = 0; index < heatmapCells.length; index += 7) {
      let sum = 0;
      for (let offset = index; offset < Math.min(index + 7, heatmapCells.length); offset++) {
        sum += values[offset];
      }
      for (let offset = index; offset < Math.min(index + 7, heatmapCells.length); offset++) {
        values[offset] = sum;
      }
    }
  } else if (heatmapMode === "cumulative") {
    let running = 0;
    heatmapCells.forEach((cell, index) => {
      running += values[index];
      values[index] = cell.date ? running : 0;
    });
  }
  const level = levelScale(values);
  heatmapCells.forEach((cell, index) => {
    cell.node.dataset.level = String(level(values[index]));
    cell.node.classList.toggle("is-today", cell.date === todayKey);
    cell.node.title = cell.date ? `${cell.date} · ${formatNumber(values[index])} tokens` : "";
  });
  const active = heatmapCells.filter((cell) => cell.date && (totals.get(cell.date) || 0) > 0).length;
  const windowTotal = heatmapCells.reduce(
    (sum, cell) => sum + (cell.date ? totals.get(cell.date) || 0 : 0),
    0,
  );
  $("#heatmap-caption").textContent = active
    ? `${formatNumber(windowTotal)} tokens across ${active} active days`
    : "No activity in this range yet.";
  if (entrance) {
    host.classList.remove("is-entering");
    void host.offsetWidth;
    host.classList.add("is-entering");
  }
}

function renderStatTrend(stats, entrance) {
  const host = $("#stat-trend");
  const totals = dayTotals(stats);
  const today = new Date();
  today.setHours(0, 0, 0, 0);
  const days = [];
  for (let offset = trendRange - 1; offset >= 0; offset--) {
    const date = new Date(today);
    date.setDate(today.getDate() - offset);
    days.push({
      date: isoDay(date),
      total: totals.get(isoDay(date)) || 0,
      label: statDayFormat.format(date),
    });
  }
  if (trendRangeBuilt !== trendRange) {
    trendRangeBuilt = trendRange;
    host.replaceChildren();
    days.forEach((day, index) => {
      const bar = el("div", { class: "stat-bar" });
      const label = el("span", { class: "stat-x", text: day.label });
      if (trendRange > 7 && index % 5 !== 0 && index !== days.length - 1) {
        label.classList.add("empty");
        label.textContent = day.label;
      }
      host.append(el("div", { class: "stat-col" }, [el("div", { class: "stat-barwrap" }, [bar]), label]));
    });
    entrance = true;
  }
  const columns = [...host.children];
  const maximum = Math.max(1, ...days.map((day) => day.total));
  days.forEach((day, index) => {
    const bar = columns[index]?.querySelector(".stat-bar");
    if (!bar) return;
    const height = day.total > 0 ? Math.max(2, (day.total / maximum) * 100) : 0;
    bar.title = `${day.label} · ${formatNumber(day.total)} tokens`;
    if (entrance) {
      bar.style.height = "0%";
      requestAnimationFrame(() => {
        bar.style.height = `${height}%`;
      });
    } else {
      bar.style.height = `${height}%`;
    }
  });
  const sum = days.reduce((total, day) => total + day.total, 0);
  $("#stat-trend-caption").textContent = `${formatNumber(sum)} tokens · last ${trendRange} days · peak ${compactTokens.format(maximum)}`;
}

function renderStatModels(stats, entrance) {
  const host = $("#stat-models");
  const models = stats.models || [];
  if (!models.length) {
    modelRowParts.clear();
    host.replaceChildren(el("div", { class: "empty", text: "No token usage recorded yet." }));
    return;
  }
  const total = Math.max(1, stats.lifetime?.total_tokens || 0);
  const rows = [];
  models.slice(0, 10).forEach((model) => {
    let parts = modelRowParts.get(model.model);
    if (!parts) {
      const fill = el("div", { class: "model-fill" });
      const value = el("span", { class: "model-value" });
      const share = el("span", { class: "model-share" });
      const requests = el("span", { class: "model-requests" });
      const row = el("div", { class: "model-row" }, [
        el("span", { class: "model-name mono", text: model.model || "unknown" }),
        el("div", { class: "model-track" }, [fill]),
        value,
        share,
        requests,
      ]);
      parts = { row, fill, value, share, requests };
      modelRowParts.set(model.model, parts);
    }
    const tokens = model.total_tokens || 0;
    const width = Math.min(100, (tokens / total) * 100);
    parts.value.textContent = `${compactTokens.format(tokens)} tok`;
    parts.share.textContent = `${Math.round(width)}%`;
    parts.requests.textContent = `${formatNumber(model.requests || 0)} req`;
    parts.row.title = `${model.model} · ${formatNumber(tokens)} tokens · ${formatNumber(model.requests || 0)} requests`;
    if (entrance) {
      parts.fill.style.width = "0%";
      requestAnimationFrame(() => {
        parts.fill.style.width = `${width}%`;
      });
    } else {
      parts.fill.style.width = `${width}%`;
    }
    rows.push(parts.row);
  });
  host.replaceChildren(...rows);
}

// `/api/monitor` nests the snapshot under `metrics`, like `usage` and
// `upstream` are promoted beside it.
const statisticsOf = (monitor) => monitor?.metrics?.statistics;

function renderStatistic(stats, entrance = false) {
  if (!stats) return;
  renderStatMetrics(stats, entrance);
  renderHeatmap(stats, entrance);
  renderStatTrend(stats, entrance);
  renderStatModels(stats, entrance);
}

function render() {
  renderChecklist();
  const monitor = store.get("monitor");
  if (!monitor) return;
  renderSummary(monitor);
  renderUsage(monitor);
  renderStatistic(statisticsOf(monitor));
}

export default {
  id: "overview",
  title: "概览",
  sub: "运行指标、Token 用量与资源状态。",
  init() {
    bindTabs($("#page-overview"), (id) => {
      if (id === "statistic") renderStatistic(statisticsOf(store.get("monitor")), true);
    });
    $$("[data-heatmap-mode]").forEach((button) => {
      button.addEventListener("click", () => {
        heatmapMode = button.dataset.heatmapMode;
        $$("[data-heatmap-mode]").forEach((item) => item.classList.toggle("active", item === button));
        const stats = statisticsOf(store.get("monitor"));
        if (stats) renderHeatmap(stats);
      });
    });
    $$("[data-trend-range]").forEach((button) => {
      button.addEventListener("click", () => {
        trendRange = Number(button.dataset.trendRange);
        $$("[data-trend-range]").forEach((item) => item.classList.toggle("active", item === button));
        const stats = statisticsOf(store.get("monitor"));
        if (stats) renderStatTrend(stats);
      });
    });
    store.on("monitor", render);
    store.on("config", renderChecklist);
    onChange(render);
  },
  show: render,
};
