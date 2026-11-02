// main.ts — rup.ai companion UI bootstrap: token handling, theme
// persistence, polling loop and module wiring.

import { initApp, refreshStatus, loadProviders, saveConfig, fetchModels, msg } from "./app";
import { initPreview, refreshPreview, pollServers } from "./preview";

export const state = {
  token: "",
  theme: (localStorage.getItem("rupai.theme") ?? "dark") as "dark" | "light",
};

function readToken(): string {
  const url = new URL(location.href);
  const fromQuery = url.searchParams.get("token");
  if (fromQuery) {
    sessionStorage.setItem("rupai.token", fromQuery);
    // clean the token out of the address bar
    url.searchParams.delete("token");
    history.replaceState(null, "", url.toString());
    return fromQuery;
  }
  return sessionStorage.getItem("rupai.token") ?? "";
}

export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: {
      "Content-Type": "application/json",
      "X-Rupai-Token": state.token,
      ...(init?.headers ?? {}),
    },
  });
  if (!res.ok) {
    const text = await res.text().catch(() => res.statusText);
    throw new Error(`${res.status}: ${text}`);
  }
  return (await res.json()) as T;
}

export function setConnection(ok: boolean, text: string): void {
  const dot = document.getElementById("connection-dot");
  const label = document.getElementById("connection-text");
  if (dot) dot.classList.toggle("ok", ok);
  if (label) label.textContent = text;
}

function applyTheme(): void {
  document.documentElement.dataset.theme = state.theme;
  localStorage.setItem("rupai.theme", state.theme);
}

function toggleTheme(): void {
  state.theme = state.theme === "dark" ? "light" : "dark";
  applyTheme();
}

async function tick(): Promise<void> {
  try {
    await refreshStatus();
    setConnection(true, "connected");
  } catch (err) {
    setConnection(false, "backend unreachable");
    console.warn(err);
  }
  try {
    await pollServers();
  } catch {
    /* server polling is best-effort */
  }
}

async function main(): Promise<void> {
  state.token = readToken();
  applyTheme();

  const versionEl = document.getElementById("version");
  try {
    const health = await api<{ ok: boolean; version: string }>("/api/health");
    if (versionEl) versionEl.textContent = "v" + health.version;
  } catch {
    setConnection(false, "backend unreachable");
  }

  initApp();
  initPreview();

  document.getElementById("theme-toggle")?.addEventListener("click", toggleTheme);
  document.getElementById("save-config")?.addEventListener("click", () => {
    void saveConfig().catch((e) => msg((e as Error).message, false));
  });
  document.getElementById("model-fetch")?.addEventListener("click", () => {
    void fetchModels().catch((e) => msg((e as Error).message, false));
  });
  document.getElementById("preview-refresh")?.addEventListener("click", refreshPreview);
  document.getElementById("preview-open")?.addEventListener("click", () => {
    const sel = document.getElementById("preview-server") as HTMLSelectElement | null;
    const url = sel?.value;
    if (!url) return;
    void api("/api/open", { method: "POST", body: JSON.stringify({ url }) }).catch(() => {
      // browser opener refused (permissions) — the URL is shown anyway
      window.open(url, "_blank");
    });
  });

  await loadProviders();
  await tick();
  window.setInterval(tick, 3000);
}

void main();
