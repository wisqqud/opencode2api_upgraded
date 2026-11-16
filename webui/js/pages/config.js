// Page: 配置中心 — masked config editing with validation, hot reload and reveal.
import { api } from "../api.js";
import { loadConfig, loadDebugModels } from "../data.js";
import * as store from "../store.js";
import { t, onChange } from "../i18n.js";
import { $, $$, bindTabs, copyText, el, openDialog, toast } from "../ui.js";

// A long proxy list is rendered collapsed: the first CHIP_RENDER_LIMIT chips
// plus a toggle. A single flex-wrap row with thousands of chips is what froze
// the config page on large pools.
const CHIP_RENDER_LIMIT = 100;
const expandedChips = new Set();

function renderChips(name) {
  const host = $(`#${name}-chips`);
  host.replaceChildren();
  const items = store.get("config")?.[name] || [];
  const expanded = expandedChips.has(name);
  const visible = expanded ? items : items.slice(0, CHIP_RENDER_LIMIT);
  visible.forEach((item) => {
    const remove = el("button", { type: "button", title: t("移除", "common.remove"), text: "×" });
    const chip = el("span", { class: "chip", dataset: { id: item.id } });
    remove.addEventListener("click", () => chip.remove());
    chip.append(el("span", { text: item.display }), remove);
    host.append(chip);
  });
  if (items.length > visible.length)
    host.append(chipToggle(`config.chips.expand`, `展开全部（共 ${items.length} 个）`, items.length, () => {
      expandedChips.add(name);
      renderChips(name);
    }));
  else if (expanded)
    host.append(chipToggle(`config.chips.collapse`, "收起", null, () => {
      expandedChips.delete(name);
      renderChips(name);
    }));
}

function chipToggle(key, fallback, count, onClick) {
  const props = { type: "button", class: "chip chip-toggle", text: t(fallback, key, count) };
  if (count != null) props.dataset = { count };
  const button = el("button", props);
  button.addEventListener("click", onClick);
  return button;
}

function renderRestartNotice(value) {
  const notice = $("#restart-notice");
  if (value?.restart_required_fields?.length) {
    notice.textContent = t(
      `已保存但尚未生效：${value.restart_required_fields.join(", ")}。当前 API ${value.effective.listen}、WebUI ${value.effective.webui_listen}。请重启进程。`,
      "config.restartNotice",
      value.restart_required_fields.join(", "),
      value.effective.listen,
      value.effective.webui_listen,
    );
    notice.classList.remove("hidden");
  } else {
    notice.classList.add("hidden");
  }
}

function secretInputs(name) {
  const kept = [...$(`#${name}-chips`).children]
    .filter((node) => node.dataset.id !== undefined)
    .map((node) => ({ id: node.dataset.id }));
  const added = $(`#${name}-new`)
    .value.split(/\r?\n/)
    .map((value) => value.trim())
    .filter(Boolean)
    .map((value) => ({ value }));
  return kept.concat(added);
}

const numberValue = (selector) => Number($(selector).value);

// Prompt inject: each part is a built-in default unless its Override checkbox
// is on. The preview composes exactly what the gateway will prepend.
const PROMPT_PARTS = {
  base_v1: { editor: "#c-prompt-base-v1", field: "base_v1", custom: "custom_base_v1" },
  base_v2: { editor: "#c-prompt-base-v2", field: "base_v2", custom: "custom_base_v2" },
  environment: { editor: "#c-prompt-environment", field: "environment", custom: "custom_environment" },
  skills: { editor: "#c-prompt-skills", field: "skills", custom: "custom_skills" },
};
const promptPreviewSample = {
  model: "model-id",
  cwd: "C:\\Users\\you\\project",
  workspace: "C:\\Users\\you\\project",
  git: "yes",
  platform: "win32",
  date: new Date().toLocaleDateString("en-US", {
    weekday: "short",
    month: "short",
    day: "2-digit",
    year: "numeric",
  }),
};
let promptPreviewVersion = "v1";

const promptDefaults = () => store.get("config")?.prompt_defaults || {};

function promptEditor(part) {
  return $(PROMPT_PARTS[part].editor);
}

function promptCustom(part) {
  return $(`[data-prompt-custom="${part}"]`).checked;
}

function promptText(part) {
  return promptEditor(part)?.value || "";
}

function updatePromptCount(part) {
  const editor = part === "extra" ? $("#c-prompt-extra") : promptEditor(part);
  const count = $(`[data-prompt-count="${part}"]`);
  if (editor && count) count.textContent = `${editor.value.length.toLocaleString("en-US")} chars`;
}

function setPromptPart(part, custom, text) {
  const spec = PROMPT_PARTS[part];
  const checkbox = $(`[data-prompt-custom="${part}"]`);
  const editor = promptEditor(part);
  checkbox.checked = !!custom;
  editor.value = custom ? text || "" : promptDefaults()[spec.field] || "";
  editor.disabled = !custom;
  updatePromptCount(part);
}

function renderPromptPreview() {
  const enabled = $("#c-prompt-enabled").checked;
  const chip = $("#prompt-mode");
  const custom =
    Object.keys(PROMPT_PARTS).some(promptCustom) || $("#c-prompt-extra").value.trim() !== "";
  chip.textContent = !enabled ? "Disabled" : custom ? "Custom" : "Built-in";
  chip.classList.toggle("is-off", !enabled);
  if (!enabled) {
    $("#prompt-preview").textContent =
      "Prompt injection is disabled. Requests go upstream without the injected system prompt.";
    return;
  }
  const base = promptPreviewVersion === "v1" ? promptText("base_v1") : promptText("base_v2");
  const environment = promptText("environment").replace(
    /\{\{(\w+)\}\}/g,
    (match, key) => promptPreviewSample[key] ?? match,
  );
  const parts = [base, environment, promptText("skills"), $("#c-prompt-extra").value.trim()];
  $("#prompt-preview").textContent = parts.filter(Boolean).join("\n\n");
}

function fillPrompt(value) {
  const prompt = value.prompt || {};
  $("#c-prompt-enabled").checked = !prompt.disabled;
  Object.entries(PROMPT_PARTS).forEach(([part, spec]) => {
    setPromptPart(part, prompt[spec.custom], prompt[spec.field]);
  });
  $("#c-prompt-extra").value = prompt.extra || "";
  updatePromptCount("extra");
  renderPromptPreview();
}

function forcePromptDefaults() {
  $("#c-prompt-enabled").checked = true;
  Object.keys(PROMPT_PARTS).forEach((part) => setPromptPart(part, false, ""));
  $("#c-prompt-extra").value = "";
  updatePromptCount("extra");
  renderPromptPreview();
}

function promptUpdate() {
  const prompt = { disabled: !$("#c-prompt-enabled").checked, extra: $("#c-prompt-extra").value };
  Object.entries(PROMPT_PARTS).forEach(([part, spec]) => {
    prompt[spec.custom] = promptCustom(part);
    prompt[spec.field] = promptCustom(part) ? promptText(part) : "";
  });
  return prompt;
}

function fillConfig(value) {
  if (!value) return;
  $("#c-listen").value = value.listen;
  $("#c-prefer").value = value.prefer;
  $("#c-web-listen").value = value.webui.listen;
  $("#c-session").value = value.webui.session_ttl_minutes;
  $("#c-web-enabled").checked = value.webui.enabled;
  $("#c-anonymous").checked = !!value.anonymous;
  const warp = value.warp || {};
  $("#c-warp-enabled").checked = !!warp.enabled;
  $("#c-warp-pool").value = warp.pool_size || 2;
  $("#c-warp-stack").value = warp.stack || "ipv6";
  $("#c-warp-source").value = warp.source_ipv6 || "";
  $("#c-warp-connect").value = warp.connect_via || "";
  $("#c-proxyfile").value = value.proxyfile;
  $("#c-up-zen").value = value.upstream.zen;
  $("#c-up-go").value = value.upstream.go;
  $("#c-attempts").value = value.retry.max_attempts;
  $("#c-timeout").value = value.retry.timeout_seconds;
  $("#c-refresh").value = value.models.refresh_seconds;
  $("#c-protocols").value = JSON.stringify(value.models.protocols || {}, null, 2);
  $("#c-free-only").checked = !!value.models.free_only;
  $("#c-active-probes").checked = !!value.models.active_probes;
  const reasoning = value.reasoning || {};
  $("#c-reasoning-effort").value = reasoning.effort || "";
  $("#c-reasoning-by-model").value = JSON.stringify(reasoning.effort_by_model || {}, null, 2);
  $("#c-idle").value = value.performance.max_idle_conns;
  $("#c-idle-host").value = value.performance.max_idle_conns_per_host;
  $("#c-max-host").value = value.performance.max_conns_per_host;
  $("#c-idle-timeout").value = value.performance.idle_conn_timeout_seconds;
  $("#c-connect").value = value.performance.connect_timeout_seconds;
  $("#c-cooldown").value = value.performance.failure_cooldown_seconds;
  $("#c-attempt-timeout").value = value.performance.attempt_timeout_seconds;
  $("#c-first-event-timeout").value = value.performance.first_event_timeout_seconds || 0;
  $("#c-level").value = value.logging.level;
  $("#c-ring").value = value.logging.ring_size;
  $("#c-dump-bodies").checked = !!value.logging.dump_request_bodies;
  fillPrompt(value);
  ["server_keys", "zen_keys", "go_keys", "proxies"].forEach(renderChips);
  renderRestartNotice(value);
}

async function save(event) {
  event.preventDefault();
  const current = store.get("config");
  if (!current) return;
  try {
    const update = {
      listen: $("#c-listen").value,
      server_keys: secretInputs("server_keys"),
      zen_keys: secretInputs("zen_keys"),
      go_keys: secretInputs("go_keys"),
      anonymous: $("#c-anonymous").checked,
      warp: {
        enabled: $("#c-warp-enabled").checked,
        pool_size: numberValue("#c-warp-pool"),
        stack: $("#c-warp-stack").value,
        source_ipv6: $("#c-warp-source").value.trim(),
        connect_via: $("#c-warp-connect").value.trim(),
      },
      proxies: secretInputs("proxies"),
      proxyfile: $("#c-proxyfile").value,
      prefer: $("#c-prefer").value,
      upstream: { zen: $("#c-up-zen").value, go: $("#c-up-go").value },
      retry: { max_attempts: numberValue("#c-attempts"), timeout_seconds: numberValue("#c-timeout") },
      models: {
        refresh_seconds: numberValue("#c-refresh"),
        protocols: JSON.parse($("#c-protocols").value || "{}"),
        free_only: $("#c-free-only").checked,
        active_probes: $("#c-active-probes").checked,
      },
      reasoning: {
        effort: $("#c-reasoning-effort").value,
        effort_by_model: JSON.parse($("#c-reasoning-by-model").value || "{}"),
      },
      prompt: promptUpdate(),
      performance: {
        max_idle_conns: numberValue("#c-idle"),
        max_idle_conns_per_host: numberValue("#c-idle-host"),
        max_conns_per_host: numberValue("#c-max-host"),
        idle_conn_timeout_seconds: numberValue("#c-idle-timeout"),
        connect_timeout_seconds: numberValue("#c-connect"),
        failure_cooldown_seconds: numberValue("#c-cooldown"),
        attempt_timeout_seconds: numberValue("#c-attempt-timeout"),
        first_event_timeout_seconds: numberValue("#c-first-event-timeout"),
      },
      logging: {
        level: $("#c-level").value,
        ring_size: numberValue("#c-ring"),
        dump_request_bodies: $("#c-dump-bodies").checked,
      },
      webui: {
        enabled: $("#c-web-enabled").checked,
        listen: $("#c-web-listen").value,
        username: current.webui.username,
        session_ttl_minutes: numberValue("#c-session"),
      },
    };
    const result = await api("/api/config", { method: "PUT", body: JSON.stringify(update) });
    store.set("config", result.config);
    ["server_keys", "zen_keys", "go_keys", "proxies"].forEach(
      (name) => ($(`#${name}-new`).value = ""),
    );
    await loadDebugModels();
    toast(
      result.result.restart_required
        ? t(
            `已应用；${result.result.restart_fields.join(", ")} 需重启`,
            "toast.config.restart",
            result.result.restart_fields.join(", "),
          )
        : t("配置已保存并立即应用", "toast.config.saved"),
    );
  } catch (error) {
    toast(error.message, true);
  }
}

async function reload() {
  try {
    const result = await api("/api/config/reload", { method: "POST", body: "{}" });
    await loadConfig();
    await loadDebugModels();
    toast(
      result.restart_required
        ? t("磁盘配置已重载，监听字段需重启", "toast.config.reloadedRestart")
        : t("磁盘配置已重载", "toast.config.reloaded"),
    );
  } catch (error) {
    toast(error.message, true);
  }
}

function reveal() {
  const input = el("input", { type: "password", autocomplete: "current-password" });
  const submit = async (close) => {
    try {
      const values = await api("/api/config/reveal", {
        method: "POST",
        body: JSON.stringify({ password: input.value }),
      });
      close();
      openDialog({
        title: t("敏感配置", "modal.title"),
        size: "dialog-lg",
        body: el("pre", { text: JSON.stringify(values, null, 2) }),
        actions: [{ label: t("关闭", "modal.close") }],
      });
    } catch (error) {
      toast(error.message, true);
    }
  };
  const dialog = openDialog({
    title: t("查看敏感配置", "modal.revealTitle"),
    body: el("div", { class: "field" }, [
      el("label", { text: t("请再次输入管理密码", "config.revealPrompt") }),
      input,
    ]),
    actions: [
      { label: t("取消", "common.cancel") },
      { label: t("查看", "common.view"), kind: "btn-primary", onClick: submit },
    ],
  });
  input.addEventListener("keydown", (event) => {
    if (event.key === "Enter") {
      event.preventDefault();
      submit(dialog.close);
    }
  });
}

export default {
  id: "config",
  title: "配置中心",
  sub: "验证后原子保存，并热应用可变字段。",
  init() {
    store.on("config", fillConfig);
    bindTabs($("#page-config"));
    $("#config-form").addEventListener("submit", save);
    $("#reload-config").addEventListener("click", reload);
    $("#reveal").addEventListener("click", reveal);
    $("#c-prompt-enabled").addEventListener("change", renderPromptPreview);
    $("#prompt-reset").addEventListener("click", forcePromptDefaults);
    $("#prompt-copy").addEventListener("click", () =>
      copyText($("#prompt-preview").textContent, "Prompt preview copied"),
    );
    Object.entries(PROMPT_PARTS).forEach(([part, spec]) => {
      $(`[data-prompt-custom="${part}"]`).addEventListener("change", (event) => {
        const editor = promptEditor(part);
        if (event.target.checked) {
          if (!editor.value) editor.value = promptDefaults()[spec.field] || "";
          editor.disabled = false;
        } else {
          editor.value = promptDefaults()[spec.field] || "";
          editor.disabled = true;
        }
        updatePromptCount(part);
        renderPromptPreview();
      });
      $(`[data-prompt-reset="${part}"]`).addEventListener("click", () => {
        setPromptPart(part, false, "");
        renderPromptPreview();
      });
    });
    $('[data-prompt-reset="extra"]').addEventListener("click", () => {
      $("#c-prompt-extra").value = "";
      updatePromptCount("extra");
      renderPromptPreview();
    });
    $$(".prompt-editor").forEach((editor) => {
      editor.addEventListener("input", () => {
        const part = Object.entries(PROMPT_PARTS).find(
          ([, spec]) => spec.editor === `#${editor.id}`,
        )?.[0];
        updatePromptCount(part || "extra");
        renderPromptPreview();
      });
    });
    $$("[data-prompt-preview]").forEach((button) => {
      button.addEventListener("click", () => {
        promptPreviewVersion = button.dataset.promptPreview;
        $$("[data-prompt-preview]").forEach((item) =>
          item.classList.toggle("active", item === button),
        );
        renderPromptPreview();
      });
    });
    onChange(() => {
      const value = store.get("config");
      if (value) renderRestartNotice(value);
      // Update chip tooltips in place so pending unsaved chip removals survive a language switch.
      $$(".chips .chip > button").forEach((button) => (button.title = t("移除", "common.remove")));
      $$(".chip-toggle").forEach((node) => {
        node.textContent = node.dataset.count
          ? t(`展开全部（共 ${node.dataset.count} 个）`, "config.chips.expand", node.dataset.count)
          : t("收起", "config.chips.collapse");
      });
    });
  },
};
