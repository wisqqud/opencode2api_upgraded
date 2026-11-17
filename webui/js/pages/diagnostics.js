// Page: diagnostics — request ledgers, upstream attempts, routing and the live
// event stream. The interactive request bench lives on the #chat page.
import * as logstream from "../logstream.js";
import { loadDebugModels } from "../data.js";
import * as store from "../store.js";
import { t, dateLocale, onChange } from "../i18n.js";
import { $, bindTabs, el, emptyRow, formatNumber, renderFacts, timeOnly } from "../ui.js";

function paginate(name, items) {
  const state = pagerState[name];
  const totalPages = Math.max(1, Math.ceil(items.length / state.size));
  state.page = Math.min(Math.max(1, state.page), totalPages);
  $(`#${name}-pager-info`).textContent = t(
    `第 ${state.page} / ${totalPages} 页 · 共 ${items.length} 条`,
    "diag.pager.info",
    state.page,
    totalPages,
    items.length,
  );
  $(`#${name}-pager-prev`).disabled = state.page <= 1;
  $(`#${name}-pager-next`).disabled = state.page >= totalPages;
  return items.slice((state.page - 1) * state.size, state.page * state.size);
}

function renderRequests() {
  const host = $("#upstream-requests");
  host.replaceChildren();
  const requests = paginate("requests", (store.get("monitor")?.upstream?.requests || []).slice().reverse());
  requests.forEach((request) => {
    const terminal = {
      stream_error: t("流式响应失败", "diag.streamError"),
      client_canceled: t("客户端已取消", "diag.clientCanceled"),
    };
    const result = terminal[request.outcome]
      ? `HTTP ${request.status} · ${terminal[request.outcome]}`
      : request.status || request.outcome;
    host.append(
      el("tr", {}, [
        el("td", { class: "mono", text: timeOnly(request.time) }),
        el("td", { class: "mono", text: request.request_id }),
        el("td", { text: request.model }),
        el("td", { text: request.tier || "—" }),
        el("td", { text: request.channel }),
        el("td", { text: request.key_id || "—" }),
        el("td", { class: "num", text: request.attempts }),
        el("td", { class: request.success ? "ok" : "bad", text: result }),
        el("td", { class: "num", text: `${request.duration_ms} ms` }),
      ]),
    );
  });
  if (!host.children.length) emptyRow(host, 9, t("尚无请求路由", "diag.noRequests"));
}

function renderAttempts() {
  const host = $("#upstream-attempts");
  host.replaceChildren();
  const attempts = paginate("attempts", (store.get("monitor")?.upstream?.recent || []).slice().reverse());
  attempts.forEach((attempt) => {
    host.append(
      el("tr", {}, [
        el("td", { class: "mono", text: timeOnly(attempt.time) }),
        el("td", { class: "mono", text: attempt.request_id }),
        el("td", { text: attempt.model }),
        el("td", { text: `${attempt.tier} / ${attempt.channel}` }),
        el("td", { text: attempt.key_id }),
        el("td", { text: attempt.proxy_node }),
        el("td", { class: attempt.success ? "ok" : "bad", text: attempt.status || attempt.outcome }),
        el("td", { class: "num", text: `${attempt.duration_ms} ms` }),
      ]),
    );
  });
  if (!host.children.length) emptyRow(host, 8, t("尚无上游尝试", "diag.noAttempts"));
}

function renderMetadata() {
  const monitor = store.get("monitor");
  const debug = store.get("debug");
  const metadata = debug?.metadata || monitor?.resources?.metadata || {};
  const stamp = (value) => new Date(value).toLocaleString(dateLocale(), { hour12: false });
  renderFacts($("#metadata-facts"), [
    [
      t("就绪", "diag.ready"),
      metadata.ready ? t("是", "common.yes") : t("否", "common.no"),
      metadata.ready ? "ok" : "warn",
    ],
    [t("模型数", "diag.modelCount"), formatNumber(metadata.models)],
    [
      t("更新时间", "diag.updatedAt"),
      metadata.updated_at ? stamp(metadata.updated_at) : t("尚未更新", "diag.notUpdated"),
    ],
    [t("是否过期", "diag.stale"), metadata.stale ? t("是", "common.yes") : t("否", "common.no"), metadata.stale ? "warn" : "ok"],
    [t("下次刷新", "diag.nextRefresh"), metadata.next_refresh ? stamp(metadata.next_refresh) : "—"],
    [
      t("最近错误", "diag.lastError"),
      metadata.last_error || t("无", "common.none"),
      metadata.last_error ? "bad" : "ok",
    ],
  ]);
}

function renderRouteModels() {
  const debug = store.get("debug");
  const host = $("#diagnostic-models");
  if (!host || !debug) return;
  const query = $("#diagnostic-model-search").value.trim().toLowerCase();
  host.replaceChildren();
  (debug.models || [])
    .filter((item) => !query || item.model.toLowerCase().includes(query))
    .forEach((item) => {
      const eligibility = item.anonymous_eligibility || {};
      const plan = [...(item.anonymous ? ["anonymous"] : []), ...(item.key_tiers || [])];
      host.append(
        el("tr", {}, [
          el("td", { class: "mono", text: item.model }),
          el("td", { text: `${item.native_protocol} · ${item.protocol_source}` }),
          el("td", {
            class: item.route_error ? "bad" : "",
            text: item.route_error || (plan.length ? plan.join(" → ") : "—"),
          }),
          el("td", {
            class: eligibility.allowed ? "ok" : "warn",
            text: eligibility.allowed ? t("允许", "common.allowed") : t("拒绝", "common.denied"),
          }),
          el("td", { text: eligibility.source || "—" }),
          el("td", {
            class: "num",
            text: `${eligibility.input_cost == null ? "?" : eligibility.input_cost} / ${
              eligibility.output_cost == null ? "?" : eligibility.output_cost
            }`,
          }),
        ]),
      );
    });
  if (!host.children.length) emptyRow(host, 6, t("没有匹配模型", "diag.noMatch"));
}

const pagerState = {
  requests: { page: 1, size: 100 },
  attempts: { page: 1, size: 100 },
};

let activeTab = "ledger";

function renderLedger() {
  renderRequests();
  renderAttempts();
}

function renderRoute() {
  renderMetadata();
  renderRouteModels();
}

export default {
  id: "diagnostics",
  title: "诊断",
  sub: "路由资格与运行流水。",
  init() {
    bindTabs($("#page-diagnostics"), (id) => {
      activeTab = id;
      logstream.setVisible(id === "logs");
      if (id === "ledger") renderLedger();
      if (id === "route") renderRoute();
    });
    logstream.bind();
    $("#diagnostic-model-search").addEventListener("input", renderRouteModels);
    for (const name of ["requests", "attempts"]) {
      $(`#${name}-pager-prev`).addEventListener("click", () => {
        pagerState[name].page -= 1;
        renderLedger();
      });
      $(`#${name}-pager-next`).addEventListener("click", () => {
        pagerState[name].page += 1;
        renderLedger();
      });
      $(`#${name}-pager-size`).addEventListener("change", (event) => {
        pagerState[name].size = Number(event.target.value);
        pagerState[name].page = 1;
        renderLedger();
      });
    }
    store.on("debug", renderRoute);
    store.on("monitor", () => {
      renderLedger();
      renderMetadata();
    });
    onChange(() => {
      renderLedger();
      renderMetadata();
      renderRoute();
    });
  },
  show() {
    if (!store.get("debug")) loadDebugModels();
    renderLedger();
    renderRoute();
    if (activeTab === "logs") logstream.setVisible(true);
  },
  hide() {
    logstream.setVisible(false);
  },
  refresh() {
    if (activeTab === "logs") logstream.render();
  },
};
