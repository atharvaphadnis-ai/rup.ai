// app.ts — rup.ai companion UI: state rendering and configuration actions
// (provider/model selection, API key storage, approval policy). This talks to
// the real backend API served by preview.go — there is no fake chat here.

import { api, setConnection } from "./main";
interface StatusPayload {
  version: string;
  provider: string;
  model: string;
  mode: string;
  workspace: string;
  uptime_sec: number;
  usage_tokens: number;
  steps: number;
  providers: string[];
}

interface ConfigPayload {
  active_provider: string;
  active_model: string;
  policies: { approval: string };
  providers: Record<string, { base_url: string; model: string; kind: string }>;
}


export function msg(text: string, ok: boolean): void {
  const el = document.getElementById("config-msg");
  if (!el) return;
  el.textContent = text;
  el.classList.toggle("ok", ok);
  el.classList.toggle("err", !ok);
  window.setTimeout(() => (el.textContent = ""), 6000);
}

function setText(id: string, value: string): void {
  const el = document.getElementById(id);
  if (el) el.textContent = value;
}

export async function refreshStatus(): Promise<void> {
  const st = await api<StatusPayload>("/api/status");
  setText("st-provider", st.provider || "—");
  setText("st-model", st.model || "—");
  setText("st-mode", st.mode || "—");
  setText("st-tokens", st.usage_tokens ? String(st.usage_tokens) : "0");
  setText("st-steps", String(st.steps ?? 0));
  const up = st.uptime_sec ?? 0;
  setText("st-uptime", `${Math.floor(up / 60)}m ${Math.floor(up % 60)}s`);
  setText("st-workspace", st.workspace || "—");
  document.title = `rup.ai — ${st.provider || "not configured"}/${st.model || ""}`;
}

export async function loadProviders(): Promise<void> {
  const sel = document.getElementById("provider-select") as HTMLSelectElement | null;
  if (!sel) return;
  try {
    const cfg = await api<ConfigPayload>("/api/config");
    sel.innerHTML = "";
    const ids = new Set<string>([...Object.keys(cfg.providers ?? {}), ...knownPresets()]);
    for (const id of Array.from(ids).sort()) {
      const opt = document.createElement("option");
      opt.value = id;
      opt.textContent = id;
      sel.appendChild(opt);
    }
    if (cfg.active_provider) sel.value = cfg.active_provider;
    const modelInput = document.getElementById("model-input") as HTMLInputElement | null;
    if (modelInput && cfg.active_model) modelInput.value = cfg.active_model;
    const approval = document.getElementById("approval-select") as HTMLSelectElement | null;
    if (approval && cfg.policies?.approval) approval.value = cfg.policies.approval;
    setConnection(true, "connected");
  } catch (err) {
    setConnection(false, "token required — open the URL printed by /preview");
    console.warn(err);
  }
}

// The backend preset catalog (kept in sync with backend/providers.go).
function knownPresets(): string[] {
  return [
    "openai", "anthropic", "deepseek", "qwen", "kimi", "zai", "minimax",
    "openrouter", "groq", "together", "fireworks", "siliconflow",
    "ollama", "llamacpp", "koboldcpp", "custom",
  ];
}

export async function saveConfig(): Promise<void> {
  const providerSel = document.getElementById("provider-select") as HTMLSelectElement | null;
  const modelInput = document.getElementById("model-input") as HTMLInputElement | null;
  const keyInput = document.getElementById("api-key") as HTMLInputElement | null;
  const approval = document.getElementById("approval-select") as HTMLSelectElement | null;
  if (!providerSel || !modelInput) return;
  const body: Record<string, string> = {
    provider: providerSel.value,
    model: modelInput.value.trim(),
  };
  if (approval?.value) body.approval = approval.value;
  if (keyInput?.value) {
    body.api_key = keyInput.value;
    keyInput.value = "";
  }
  await api("/api/config", { method: "PUT", body: JSON.stringify(body) });
  msg("saved — provider, model and policy updated", true);
  await loadProviders();
  await refreshStatus();
}

export async function fetchModels(): Promise<void> {
  const providerSel = document.getElementById("provider-select") as HTMLSelectElement | null;
  if (!providerSel) return;
  msg("discovering models…", true);
  const res = await api<{ models: string[]; error?: string }>(
    `/api/models?provider=${encodeURIComponent(providerSel.value)}`,
  );
  const list = document.getElementById("model-list");
  if (!list) return;
  list.innerHTML = "";
  for (const m of res.models ?? []) {
    const opt = document.createElement("option");
    opt.value = m;
    list.appendChild(opt);
  }
  if (res.error) {
    msg(`discovery problem: ${res.error}`, false);
  } else {
    msg(`found ${res.models?.length ?? 0} models — pick one from the model field dropdown`, true);
  }
}

export function initApp(): void {
  // all wiring happens in main.ts; reserved for future extensions
}
