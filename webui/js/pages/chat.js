// Page: chat — an interactive test bench that talks to the live Gateway.
//
// Every turn goes through /api/debug/chat (streaming) or /api/debug/inference
// (buffered), so the page exercises the same routing, prompt injection, tool
// shaping and key selection as real traffic. Conversations, settings and the
// system prompt persist in localStorage.
import { api, apiStream } from "../api.js";
import { loadDebugModels } from "../data.js";
import * as store from "../store.js";
import { t, onChange } from "../i18n.js";
import {
  $,
  confirmDialog,
  copyText,
  el,
  toast,
} from "../ui.js";

const STORAGE_KEY = "opencode2api.chat.v1";
const MAX_CONTEXT_MESSAGES = 200;

const state = {
  conversations: [],
  activeId: "",
  settings: {
    protocol: "chat",
    model: "",
    key: "auto",
    relay: "auto",
    system: "",
    temperature: "",
    maxTokens: "",
    effort: "",
    stream: true,
    showReasoning: true,
  },
  sending: false,
  controller: null,
  editingId: "",
  previewOpen: false,
};

const uid = () => Date.now().toString(36) + Math.random().toString(36).slice(2, 8);

const current = () => state.conversations.find((conversation) => conversation.id === state.activeId);

function newConversation() {
  return { id: uid(), title: "", createdAt: Date.now(), updatedAt: Date.now(), messages: [] };
}

function ensureConversation() {
  if (!current()) {
    const conversation = newConversation();
    state.conversations.unshift(conversation);
    state.activeId = conversation.id;
  }
  return current();
}

function load() {
  try {
    const saved = JSON.parse(localStorage.getItem(STORAGE_KEY) || "null");
    if (saved && Array.isArray(saved.conversations)) {
      state.conversations = saved.conversations.filter(
        (conversation) => conversation && conversation.id && Array.isArray(conversation.messages),
      );
      state.activeId = saved.activeId || state.conversations[0]?.id || "";
      Object.assign(state.settings, saved.settings || {});
    }
  } catch {
    // A corrupted blob must not break the console; start clean instead.
  }
  ensureConversation();
  state.settings.stream = state.settings.stream !== false;
  state.settings.showReasoning = state.settings.showReasoning !== false;
}

function save() {
  try {
    localStorage.setItem(
      STORAGE_KEY,
      JSON.stringify({
        conversations: state.conversations.slice(0, 50),
        activeId: state.activeId,
        settings: state.settings,
      }),
    );
  } catch {
    // Storage can be full or disabled; the page still works for this session.
  }
}

// ---------------------------------------------------------------- rendering

const ESCAPES = { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" };
const escapeHTML = (text) => String(text).replace(/[&<>"']/g, (character) => ESCAPES[character]);

// inline applies the light markdown the console supports to already escaped
// text. Links are restricted to http(s) so an answer cannot smuggle script:.
function inline(text) {
  return text
    .replace(/`([^`\n]+)`/g, '<code class="chat-inline-code">$1</code>')
    .replace(/\*\*([^*\n]+)\*\*/g, "<strong>$1</strong>")
    .replace(/(^|[\s(])\*([^*\n]+)\*/g, "$1<em>$2</em>")
    .replace(/\[([^\]]+)\]\((https?:[^)\s]+)\)/g, '<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>');
}

// renderMarkdown is a small, line-based renderer: headings, fences, quotes and
// lists. Everything is escaped first, so model output cannot inject markup.
function renderMarkdown(source) {
  const lines = escapeHTML(source || "").split("\n");
  const out = [];
  let inCode = false;
  let code = [];
  let list = "";
  const flushCode = () => {
    out.push(`<pre class="chat-code"><code>${code.join("\n")}</code></pre>`);
    code = [];
  };
  const flushList = () => {
    if (list) {
      out.push(`</${list}>`);
      list = "";
    }
  };
  for (const raw of lines) {
    const fence = raw.match(/^```\s*([\w+-]*)\s*$/);
    if (fence) {
      if (inCode) {
        flushCode();
        inCode = false;
      } else {
        flushList();
        inCode = true;
      }
      continue;
    }
    if (inCode) {
      code.push(raw);
      continue;
    }
    if (!raw.trim()) {
      flushList();
      continue;
    }
    const heading = raw.match(/^(#{1,4})\s+(.+)$/);
    if (heading) {
      flushList();
      const level = Math.min(heading[1].length + 2, 6);
      out.push(`<h${level} class="chat-h">${inline(heading[2])}</h${level}>`);
      continue;
    }
    const quote = raw.match(/^&gt;\s?(.*)$/);
    if (quote) {
      flushList();
      out.push(`<blockquote class="chat-quote">${inline(quote[1])}</blockquote>`);
      continue;
    }
    const bullet = raw.match(/^\s*[-*+]\s+(.+)$/);
    if (bullet) {
      if (list !== "ul") {
        flushList();
        out.push('<ul class="chat-ul">');
        list = "ul";
      }
      out.push(`<li>${inline(bullet[1])}</li>`);
      continue;
    }
    const ordered = raw.match(/^\s*\d+[.)]\s+(.+)$/);
    if (ordered) {
      if (list !== "ol") {
        flushList();
        out.push('<ol class="chat-ol">');
        list = "ol";
      }
      out.push(`<li>${inline(ordered[1])}</li>`);
      continue;
    }
    flushList();
    out.push(`<p class="chat-p">${inline(raw)}</p>`);
  }
  if (inCode) flushCode();
  flushList();
  return out.join("");
}

const estimateTokens = (text) => Math.ceil(String(text || "").length / 4);

function metaLine(message) {
  const parts = [];
  if (message.model) parts.push(message.model);
  if (message.durationMs != null) parts.push(`${(message.durationMs / 1000).toFixed(1)}s`);
  if (message.content) parts.push(`${estimateTokens(message.content)} tok≈`);
  if (message.requestId) parts.push(message.requestId.slice(0, 18));
  if (message.stopped) parts.push(t("stopped", "chat.stopped"));
  return parts.join(" · ");
}

function actionButton(label, title, onClick, kind = "btn-ghost") {
  return el("button", {
    type: "button",
    class: `btn btn-sm ${kind} chat-action`,
    text: label,
    title,
    onclick: onClick,
  });
}

function renderMessage(message) {
  const conversation = current();
  const index = conversation.messages.indexOf(message);
  const node = el("article", {
    class: `chat-msg chat-msg-${message.role}${message.error ? " chat-msg-error" : ""}`,
    dataset: { messageId: message.id },
  });

  const avatar = el("div", { class: "chat-avatar", text: message.role === "user" ? "You" : "AI" });
  const body = el("div", { class: "chat-body" });

  const head = el("div", { class: "chat-msg-head" }, [
    el("span", { class: "chat-role", text: message.role === "user" ? "You" : "Assistant" }),
    el("span", { class: "chat-msg-time", text: new Date(message.createdAt).toLocaleTimeString() }),
  ]);
  body.append(head);

  if (state.editingId === message.id) {
    const textarea = el("textarea", { class: "chat-edit", rows: "4" });
    textarea.value = message.content;
    const actionsRow = () =>
      el("div", { class: "chat-edit-actions" }, [
        actionButton(t("Save", "chat.save"), t("Save this message", "chat.saveHint"), () => {
          message.content = textarea.value;
          message.editedAt = Date.now();
          state.editingId = "";
          save();
          renderLog();
        }, "btn-primary"),
        actionButton(t("Cancel", "chat.cancel"), "", () => {
          state.editingId = "";
          renderLog();
        }),
      ]);
    body.append(textarea, actionsRow());
    textarea.addEventListener("keydown", (event) => {
      if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) {
        event.preventDefault();
        message.content = textarea.value;
        message.editedAt = Date.now();
        state.editingId = "";
        save();
        renderLog();
      }
    });
    node.append(avatar, body);
    requestAnimationFrame(() => {
      textarea.focus();
      textarea.selectionStart = textarea.value.length;
    });
    return node;
  }

  if (message.reasoning && state.settings.showReasoning) {
    const details = el("details", { class: "chat-reasoning" });
    details.append(
      el("summary", { text: `${t("Reasoning", "chat.reasoning")} · ${estimateTokens(message.reasoning)} tok≈` }),
      el("div", { class: "chat-reasoning-body" }, [el("div", { class: "chat-reasoning-text", text: message.reasoning })]),
    );
    if (message.streaming) details.open = true;
    body.append(details);
  }

  const text = el("div", { class: "chat-text" });
  if (message.role === "assistant") text.innerHTML = message.content ? renderMarkdown(message.content) : "";
  else text.textContent = message.content || "";
  if (!message.content && message.streaming) text.append(el("span", { class: "chat-cursor", text: "▍" }));
  body.append(text);

  if (message.error) body.append(el("div", { class: "chat-error", text: message.error }));

  const actions = el("div", { class: "chat-actions" });
  if (!message.streaming) {
    actions.append(
      actionButton(t("Copy", "chat.copy"), t("Copy text", "chat.copyHint"), () =>
        copyText(message.content, t("Message copied", "chat.copied")),
      ),
    );
    actions.append(
      actionButton(t("Edit", "chat.edit"), t("Edit this message", "chat.editHint"), () => {
        state.editingId = message.id;
        renderLog();
      }),
    );
    if (message.role === "assistant") {
      actions.append(
        actionButton(t("Regenerate", "chat.regenerate"), t("Regenerate from here", "chat.regenerateHint"), () =>
          regenerate(index),
        ),
      );
    }
    actions.append(
      actionButton(t("Delete", "chat.delete"), t("Delete this message", "chat.deleteHint"), () => removeMessage(index)),
    );
    if (message.editedAt) head.append(el("span", { class: "chat-edited", text: t("edited", "chat.edited") }));
  }
  const meta = metaLine(message);
  if (meta) body.append(el("div", { class: "chat-msg-meta", text: meta }));
  body.append(actions);
  node.append(avatar, body);
  return node;
}

function renderLog() {
  const conversation = ensureConversation();
  const log = $("#chat-log");
  log.replaceChildren(...conversation.messages.map(renderMessage));
  const empty = conversation.messages.length === 0;
  $("#chat-empty").classList.toggle("hidden", !empty);
  $("#chat-title").textContent =
    conversation.title || t("New conversation", "chat.newConversation");
  const total = conversation.messages.reduce((sum, message) => sum + (message.content || "").length, 0);
  $("#chat-meta").textContent = t(
    `${conversation.messages.length} messages · ~${estimateTokens(total)} tokens`,
    "chat.meta",
    conversation.messages.length,
    estimateTokens(total),
  );
  $("#chat-retry-last").disabled =
    state.sending || !conversation.messages.some((message) => message.role === "user");
  updateConversationList();
  renderPreview();
}

function updateConversationList() {
  const host = $("#chat-conversations");
  host.replaceChildren();
  const ordered = [...state.conversations].sort((a, b) => (b.updatedAt || 0) - (a.updatedAt || 0));
  for (const conversation of ordered) {
    const title = conversation.title || t("Untitled chat", "chat.untitled");
    const pick = () => {
      if (state.sending) return;
      state.activeId = conversation.id;
      state.editingId = "";
      save();
      renderAll();
    };
    const pickButton = el("button", { type: "button", class: "chat-conversation-pick", title }, [
      el("span", { class: "chat-conversation-title", text: title }),
      el("span", {
        class: "chat-conversation-count",
        text: String(conversation.messages.length),
      }),
    ]);
    pickButton.addEventListener("click", pick);
    const item = el("div", {
      class: `chat-conversation${conversation.id === state.activeId ? " active" : ""}`,
    }, [pickButton]);
    const remove = el("button", {
      type: "button",
      class: "chat-conversation-remove",
      text: "×",
      title: t("Delete conversation", "chat.deleteConversation"),
      onclick: async (event) => {
        event.stopPropagation();
        if (state.sending) return;
        const ok = await confirmDialog({
          title: t("Delete this conversation?", "chat.deleteConversationTitle"),
          message: t("Messages in this chat are removed from this browser.", "chat.deleteConversationBody"),
          danger: true,
          confirmLabel: t("Delete", "chat.delete"),
        });
        if (!ok) return;
        state.conversations = state.conversations.filter((entry) => entry.id !== conversation.id);
        if (state.activeId === conversation.id) state.activeId = state.conversations[0]?.id || "";
        ensureConversation();
        save();
        renderAll();
      },
    });
    item.append(remove);
    host.append(item);
  }
}

function renderAll() {
  renderLog();
  syncSettings();
}

function syncSettings() {
  $("#chat-protocol").value = state.settings.protocol;
  $("#chat-system").value = state.settings.system;
  $("#chat-system-count").textContent = `${state.settings.system.length} chars`;
  $("#chat-temperature").value = state.settings.temperature;
  $("#chat-max-tokens").value = state.settings.maxTokens;
  $("#chat-effort").value = state.settings.effort;
  $("#chat-stream").checked = state.settings.stream;
  $("#chat-show-reasoning").checked = state.settings.showReasoning;
  renderKeys();
  renderRelays();
  renderModels();
}

function readSettings() {
  state.settings.protocol = $("#chat-protocol").value;
  state.settings.model = $("#chat-model").value;
  state.settings.key = $("#chat-key").value;
  state.settings.relay = $("#chat-relay").value;
  state.settings.system = $("#chat-system").value;
  state.settings.temperature = $("#chat-temperature").value;
  state.settings.maxTokens = $("#chat-max-tokens").value;
  state.settings.effort = $("#chat-effort").value;
  state.settings.stream = $("#chat-stream").checked;
  state.settings.showReasoning = $("#chat-show-reasoning").checked;
  save();
}

function renderModels() {
  const select = $("#chat-model");
  const selected = select.value || state.settings.model;
  select.replaceChildren();
  const models = store.get("debug")?.models || [];
  if (!models.length) select.append(el("option", { value: "", text: t("No models loaded", "chat.noModels") }));
  models.forEach((item) => select.append(el("option", { value: item.model, text: item.model })));
  const usable = models.find((item) => !item.route_error && (item.available_zen || item.available_go));
  const selectedDiag = models.find((item) => item.model === selected);
  if (selected && selectedDiag && !selectedDiag.route_error) select.value = selected;
  else if (usable) select.value = usable.model;
  else if (models[0]) select.value = models[0].model;
  state.settings.model = select.value;
}

function renderKeys() {
  const select = $("#chat-key");
  const selected = select.value || state.settings.key;
  select.replaceChildren();
  select.append(el("option", { value: "auto", text: t("Auto routing", "chat.key.auto") }));
  const keys = store.get("debug")?.keys || {};
  for (const [tier, label] of [
    ["zen", "Zen Keys"],
    ["go", "Go Keys"],
  ]) {
    const items = keys[tier] || [];
    if (!items.length) continue;
    const group = el("optgroup", { label });
    for (const item of items) {
      group.append(
        el("option", {
          value: `${tier}:${item.id}`,
          text: `${tier.toUpperCase()} · ${item.display} · ${item.id}`,
        }),
      );
    }
    select.append(group);
  }
  if (selected && [...select.options].some((option) => option.value === selected)) select.value = selected;
  else select.value = "auto";
  state.settings.key = select.value;
}

function renderRelays() {
  const select = $("#chat-relay");
  const selected = select.value || state.settings.relay;
  select.replaceChildren();
  select.append(el("option", { value: "auto", text: t("Any relay", "chat.relay.auto") }));
  (store.get("debug")?.relays || []).forEach((name, index) => {
    select.append(el("option", { value: String(index), text: `${index} · ${name}` }));
  });
  if (selected && [...select.options].some((option) => option.value === selected)) select.value = selected;
  else select.value = "auto";
  state.settings.relay = select.value;
}

function currentKey() {
  const value = state.settings.key || "auto";
  if (value === "auto") return { mode: "auto" };
  const [tier, ...rest] = value.split(":");
  return { mode: "selected", tier, id: rest.join(":") };
}

function currentRelay() {
  const value = state.settings.relay || "auto";
  if (value === "auto") return undefined;
  const index = Number(value);
  const name = (store.get("debug")?.relays || [])[index];
  return name ? { index, name } : undefined;
}

// ------------------------------------------------------------ request build

function buildPayload(history) {
  readSettings();
  const model = state.settings.model;
  const protocol = state.settings.protocol;
  const system = state.settings.system.trim();
  const temperature = state.settings.temperature === "" ? null : Number(state.settings.temperature);
  const maxTokens = state.settings.maxTokens === "" ? null : Number(state.settings.maxTokens);
  const effort = state.settings.effort;
  const turns = history.filter((message) => message.role === "user" || message.role === "assistant");
  if (protocol === "anthropic") {
    const payload = {
      model,
      max_tokens: maxTokens || 1024,
      messages: turns.map((message) => ({ role: message.role, content: message.content })),
    };
    if (system) payload.system = system;
    if (temperature != null) payload.temperature = temperature;
    return payload;
  }
  if (protocol === "responses") {
    const payload = {
      model,
      input: turns.map((message) => ({ role: message.role, content: message.content })),
    };
    if (system) payload.instructions = system;
    if (temperature != null) payload.temperature = temperature;
    if (maxTokens != null) payload.max_output_tokens = maxTokens;
    if (effort) payload.reasoning = { effort };
    return payload;
  }
  const messages = [];
  if (system) messages.push({ role: "system", content: system });
  for (const message of turns) messages.push({ role: message.role, content: message.content });
  const payload = { model, messages };
  if (temperature != null) payload.temperature = temperature;
  if (maxTokens != null) payload.max_tokens = maxTokens;
  if (effort) payload.reasoning_effort = effort;
  return payload;
}

function renderPreview() {
  const details = $("#chat-preview");
  details.classList.toggle("hidden", !state.previewOpen);
  if (!state.previewOpen) return;
  const conversation = ensureConversation();
  $("#chat-preview-body").textContent = JSON.stringify(buildPayload(conversation.messages), null, 2);
}

// --------------------------------------------------------------- streaming

function applyChunk(protocol, event, message) {
  if (!event) return false;
  if (protocol === "anthropic") {
    if (event.type === "content_block_delta") {
      const delta = event.delta || {};
      if (delta.type === "text_delta" && delta.text) {
        message.content += delta.text;
        return true;
      }
      if (delta.type === "thinking_delta" && delta.thinking) {
        message.reasoning += delta.thinking;
        return true;
      }
    }
    if (event.type === "content_block_start") {
      const block = event.content_block || {};
      if (block.type === "text" && block.text) {
        message.content += block.text;
        return true;
      }
    }
    return false;
  }
  if (protocol === "responses") {
    if (event.type === "response.output_text.delta" && typeof event.delta === "string") {
      message.content += event.delta;
      return true;
    }
    if (
      (event.type === "response.reasoning_summary_text.delta" ||
        event.type === "response.reasoning_text.delta") &&
      typeof event.delta === "string"
    ) {
      message.reasoning += event.delta;
      return true;
    }
    if (event.type === "response.output_item.done" && event.item) {
      const item = event.item;
      if (item.type === "message" && Array.isArray(item.content)) {
        for (const part of item.content) {
          if (part.type === "output_text" && part.text && !message.content) message.content = part.text;
        }
      }
    }
    return false;
  }
  const choice = event.choices?.[0];
  const delta = choice?.delta || {};
  let changed = false;
  if (typeof delta.content === "string" && delta.content) {
    message.content += delta.content;
    changed = true;
  }
  const reasoning = delta.reasoning_content || delta.reasoning;
  if (typeof reasoning === "string" && reasoning) {
    message.reasoning += reasoning;
    changed = true;
  }
  return changed;
}

function paintMessage(message) {
  const node = $(`#chat-log [data-message-id="${message.id}"]`);
  if (!node) return;
  const text = node.querySelector(".chat-text");
  if (text) {
    if (message.role === "assistant") text.innerHTML = renderMarkdown(message.content);
    else text.textContent = message.content;
    if (!message.content && message.streaming) text.append(el("span", { class: "chat-cursor", text: "▍" }));
  }
  const reasoning = node.querySelector(".chat-reasoning-text");
  if (reasoning && message.reasoning) reasoning.textContent = message.reasoning;
}

function atBottom() {
  const log = $("#chat-log");
  return log.scrollHeight - log.scrollTop - log.clientHeight < 80;
}

function scrollToBottom(force = false) {
  const log = $("#chat-log");
  if (force || atBottom()) {
    log.scrollTop = log.scrollHeight;
    $("#chat-scroll-bottom").classList.add("hidden");
  }
}

async function consumeStream(response, protocol, message) {
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let framePaint = false;
  while (true) {
    const { value, done } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    let split;
    while ((split = buffer.indexOf("\n\n")) >= 0) {
      const frame = buffer.slice(0, split);
      buffer = buffer.slice(split + 2);
      for (const line of frame.split("\n")) {
        if (!line.startsWith("data:")) continue;
        const data = line.slice(5).trim();
        if (!data || data === "[DONE]") continue;
        let event;
        try {
          event = JSON.parse(data);
        } catch {
          continue;
        }
        if (applyChunk(protocol, event, message)) framePaint = true;
      }
      if (framePaint) {
        framePaint = false;
        paintMessage(message);
      }
    }
  }
  if (buffer.trim().startsWith("data:")) {
    try {
      const event = JSON.parse(buffer.trim().slice(5).trim());
      if (applyChunk(protocol, event, message)) paintMessage(message);
    } catch {
      // A truncated trailing frame holds no useful text.
    }
  }
}

function extractBuffered(protocol, response) {
  if (protocol === "anthropic") {
    const blocks = Array.isArray(response?.content) ? response.content : [];
    return {
      content: blocks.filter((block) => block.type === "text").map((block) => block.text).join(""),
      reasoning: blocks.filter((block) => block.type === "thinking").map((block) => block.thinking).join(""),
    };
  }
  if (protocol === "responses") {
    let content = typeof response?.output_text === "string" ? response.output_text : "";
    let reasoning = "";
    for (const item of Array.isArray(response?.output) ? response.output : []) {
      if (item.type === "reasoning" && Array.isArray(item.summary)) {
        reasoning += item.summary.map((part) => part.text || "").join("");
      }
      if (item.type === "message" && Array.isArray(item.content)) {
        for (const part of item.content) {
          if (part.type === "output_text" && typeof part.text === "string") content += part.text;
        }
      }
    }
    return { content, reasoning };
  }
  const message = response?.choices?.[0]?.message || {};
  return {
    content: typeof message.content === "string" ? message.content : "",
    reasoning: message.reasoning_content || message.reasoning || "",
  };
}

// ------------------------------------------------------------------- turns

function truncateTo(conversation, index) {
  conversation.messages = conversation.messages.slice(0, index);
}

async function send(text, options = {}) {
  if (state.sending) return;
  const conversation = ensureConversation();
  if (text != null) {
    conversation.messages.push({
      id: uid(),
      role: "user",
      content: text,
      createdAt: Date.now(),
    });
    if (!conversation.title) conversation.title = text.trim().slice(0, 60) || t("Untitled chat", "chat.untitled");
  }
  const history = conversation.messages.slice(-MAX_CONTEXT_MESSAGES);
  const payload = buildPayload(history);
  if (!payload.model) {
    toast(t("Select a model first", "chat.error.noModel"), true);
    return;
  }
  const protocol = state.settings.protocol;
  const key = currentKey();
  const relay = currentRelay();

  const assistant = {
    id: uid(),
    role: "assistant",
    content: "",
    reasoning: "",
    createdAt: Date.now(),
    model: payload.model,
    streaming: true,
  };
  conversation.messages.push(assistant);
  conversation.updatedAt = Date.now();
  state.sending = true;
  state.controller = new AbortController();
  $("#chat-send").disabled = true;
  $("#chat-stop").classList.remove("hidden");
  $("#chat-input").disabled = true;
  $("#chat-composer-hint").textContent = t("Waiting for the gateway…", "chat.waiting");
  save();
  renderLog();
  scrollToBottom(true);

  const started = performance.now();
  const body = JSON.stringify({ protocol, key, ...(relay ? { relay } : {}), request: payload });
  try {
    if (state.settings.stream) {
      const response = await apiStream("/api/debug/chat", {
        method: "POST",
        body,
        signal: state.controller.signal,
      });
      assistant.requestId = response.headers.get("x-request-id") || "";
      const contentType = response.headers.get("content-type") || "";
      if (!contentType.includes("text/event-stream")) {
        // The Gateway answered an error as JSON; surface it instead of an
        // empty bubble.
        const data = await response.json().catch(() => ({}));
        throw new Error(data.error?.message || JSON.stringify(data).slice(0, 400) || "unexpected response");
      }
      await consumeStream(response, protocol, assistant);
    } else {
      const result = await api("/api/debug/inference", {
        method: "POST",
        body,
        signal: state.controller.signal,
      });
      assistant.requestId = result.request_id || "";
      const extracted = extractBuffered(protocol, result.response);
      assistant.content = extracted.content || "";
      assistant.reasoning = extracted.reasoning || "";
      assistant.route = result.route;
      if (!result.ok) {
        assistant.error =
          result.response?.error?.message || `HTTP ${result.http_status} · ${JSON.stringify(result.response).slice(0, 300)}`;
      }
    }
  } catch (error) {
    if (error.name === "AbortError") {
      assistant.stopped = true;
    } else {
      assistant.error = error.message;
    }
  } finally {
    assistant.streaming = false;
    assistant.durationMs = Math.round(performance.now() - started);
    if (!assistant.content && !assistant.error && assistant.reasoning) {
      assistant.content = assistant.reasoning;
      assistant.reasoning = "";
    }
    if (!assistant.content && !assistant.error) {
      assistant.error = t("The gateway returned an empty answer.", "chat.error.empty");
    }
    state.sending = false;
    state.controller = null;
    $("#chat-send").disabled = false;
    $("#chat-stop").classList.add("hidden");
    $("#chat-input").disabled = false;
    $("#chat-composer-hint").textContent = "";
    conversation.updatedAt = Date.now();
    save();
    renderLog();
    scrollToBottom();
  }
}

async function regenerate(index) {
  if (state.sending) return;
  const conversation = ensureConversation();
  const target = conversation.messages[index];
  if (!target || target.role !== "assistant") return;
  const tail = conversation.messages.slice(index + 1);
  if (tail.length) {
    const ok = await confirmDialog({
      title: t("Regenerate this answer?", "chat.regenerateTitle"),
      message: t("Every message after it is removed from the conversation.", "chat.regenerateBody"),
      confirmLabel: t("Regenerate", "chat.regenerate"),
    });
    if (!ok) return;
  }
  truncateTo(conversation, index);
  await send(null);
}

async function regenerateLast() {
  if (state.sending) return;
  const conversation = ensureConversation();
  let index = -1;
  for (let cursor = conversation.messages.length - 1; cursor >= 0; cursor -= 1) {
    if (conversation.messages[cursor].role === "user") {
      index = cursor;
      break;
    }
  }
  if (index < 0) return;
  truncateTo(conversation, index + 1);
  await send(null);
}

async function removeMessage(index) {
  if (state.sending) return;
  const conversation = ensureConversation();
  const message = conversation.messages[index];
  if (!message) return;
  const ok = await confirmDialog({
    title: t("Delete this message?", "chat.deleteMessageTitle"),
    message: t("It is removed from this browser only.", "chat.deleteMessageBody"),
    danger: true,
    confirmLabel: t("Delete", "chat.delete"),
  });
  if (!ok) return;
  conversation.messages.splice(index, 1);
  save();
  renderLog();
}

// --------------------------------------------------------------- composer

function autoGrow() {
  const input = $("#chat-input");
  input.style.height = "auto";
  input.style.height = `${Math.min(input.scrollHeight, 220)}px`;
}

function submitComposer(event) {
  event.preventDefault();
  const input = $("#chat-input");
  const text = input.value.trim();
  if (!text || state.sending) return;
  input.value = "";
  autoGrow();
  send(text);
}

function stop() {
  state.controller?.abort();
}

// ------------------------------------------------------------------ files

function exportChats() {
  const blob = new Blob(
    [
      JSON.stringify(
        { version: 1, exportedAt: new Date().toISOString(), settings: state.settings, conversations: state.conversations },
        null,
        2,
      ),
    ],
    { type: "application/json" },
  );
  const url = URL.createObjectURL(blob);
  const anchor = el("a", { href: url, download: `opencode2api-chat-${Date.now()}.json` });
  document.body.append(anchor);
  anchor.click();
  anchor.remove();
  URL.revokeObjectURL(url);
}

function importChats(file) {
  const reader = new FileReader();
  reader.onload = () => {
    try {
      const data = JSON.parse(String(reader.result));
      const incoming = Array.isArray(data.conversations)
        ? data.conversations
        : data.messages
          ? [{ ...newConversation(), messages: data.messages }]
          : null;
      if (!incoming) throw new Error(t("Unknown file shape", "chat.importBadShape"));
      const cleaned = incoming
        .filter((conversation) => conversation && Array.isArray(conversation.messages))
        .map((conversation) => ({
          id: uid(),
          title: conversation.title || "",
          createdAt: conversation.createdAt || Date.now(),
          updatedAt: Date.now(),
          messages: conversation.messages
            .filter((message) => message && (message.role === "user" || message.role === "assistant"))
            .map((message) => ({
              id: uid(),
              role: message.role,
              content: String(message.content || ""),
              reasoning: String(message.reasoning || ""),
              createdAt: message.createdAt || Date.now(),
              model: message.model || "",
              durationMs: message.durationMs ?? null,
              requestId: message.requestId || "",
            })),
        }))
        .filter((conversation) => conversation.messages.length);
      if (!cleaned.length) throw new Error(t("No messages found", "chat.importEmpty"));
      state.conversations = [...cleaned, ...state.conversations];
      state.activeId = cleaned[0].id;
      save();
      renderAll();
      toast(t(`Imported ${cleaned.length} conversation(s)`, "chat.imported", cleaned.length));
    } catch (error) {
      toast(t(`Import failed: ${error.message}`, "chat.importFailed", error.message), true);
    }
  };
  reader.readAsText(file);
}

// -------------------------------------------------------------------- init

export default {
  id: "chat",
  title: "Chat",
  sub: "A live test conversation through the real Gateway.",
  init() {
    load();
    $("#chat-composer").addEventListener("submit", submitComposer);
    $("#chat-stop").addEventListener("click", stop);
    $("#chat-input").addEventListener("input", autoGrow);
    $("#chat-input").addEventListener("keydown", (event) => {
      if (event.key === "Enter" && !event.shiftKey && !event.isComposing) {
        event.preventDefault();
        submitComposer(event);
      }
    });
    $("#chat-new").addEventListener("click", () => {
      if (state.sending) return;
      const conversation = newConversation();
      state.conversations.unshift(conversation);
      state.activeId = conversation.id;
      state.editingId = "";
      save();
      renderAll();
      $("#chat-input").focus();
    });
    $("#chat-clear").addEventListener("click", async () => {
      if (state.sending) return;
      const ok = await confirmDialog({
        title: t("Clear this conversation?", "chat.clearTitle"),
        message: t("Its messages are deleted; settings stay.", "chat.clearBody"),
        danger: true,
        confirmLabel: t("Clear", "chat.clear"),
      });
      if (!ok) return;
      const conversation = ensureConversation();
      conversation.messages = [];
      conversation.title = "";
      conversation.updatedAt = Date.now();
      save();
      renderLog();
    });
    $("#chat-retry-last").addEventListener("click", regenerateLast);
    $("#chat-scroll-bottom").addEventListener("click", () => scrollToBottom(true));
    $("#chat-log").addEventListener("scroll", () => {
      if (atBottom()) $("#chat-scroll-bottom").classList.add("hidden");
    });
    $("#chat-system").addEventListener("input", () => {
      state.settings.system = $("#chat-system").value;
      $("#chat-system-count").textContent = `${state.settings.system.length} chars`;
      save();
      renderPreview();
    });
    for (const id of ["#chat-protocol", "#chat-temperature", "#chat-max-tokens", "#chat-effort", "#chat-stream"]) {
      $(id).addEventListener("change", () => {
        readSettings();
        renderPreview();
      });
    }
    for (const id of ["#chat-model", "#chat-key", "#chat-relay"]) {
      $(id).addEventListener("change", () => {
        readSettings();
        renderPreview();
      });
    }
    $("#chat-show-reasoning").addEventListener("change", () => {
      readSettings();
      renderLog();
    });
    $("#chat-export").addEventListener("click", exportChats);
    $("#chat-import").addEventListener("click", () => $("#chat-import-file").click());
    $("#chat-import-file").addEventListener("change", (event) => {
      const file = event.target.files?.[0];
      if (file) importChats(file);
      event.target.value = "";
    });
    $("#chat-preview-toggle").addEventListener("click", () => {
      state.previewOpen = !state.previewOpen;
      $("#chat-preview").open = state.previewOpen;
      renderPreview();
    });
    $("#chat-preview").addEventListener("toggle", () => {
      state.previewOpen = $("#chat-preview").open;
    });
    $("#chat-suggestions").replaceChildren(
      ...[
        t("Explain what this gateway does in two sentences.", "chat.suggestion.1"),
        t("Write a Go function that reverses a string.", "chat.suggestion.2"),
        t("List three risks of routing free-tier traffic.", "chat.suggestion.3"),
        t("Reply with a JSON object describing today.", "chat.suggestion.4"),
      ].map((text) =>
        el("button", {
          type: "button",
          class: "chat-suggestion",
          text,
          onclick: () => {
            $("#chat-input").value = text;
            autoGrow();
            $("#chat-input").focus();
          },
        }),
      ),
    );

    store.on("debug", () => {
      syncSettings();
      renderPreview();
    });
    onChange(() => {
      renderAll();
      autoGrow();
    });
  },
  show() {
    if (!store.get("debug")) loadDebugModels();
    syncSettings();
    renderLog();
    scrollToBottom(true);
    requestAnimationFrame(autoGrow);
  },
  hide() {
    // A stream keeps running so a navigation does not lose the answer; the
    // log is rebuilt from state when the page returns.
  },
  refresh() {
    syncSettings();
    renderPreview();
  },
};
