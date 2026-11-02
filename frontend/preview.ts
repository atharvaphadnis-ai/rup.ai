// preview.ts — rup.ai companion UI: live development-server preview.
// Shows the actual running application in an iframe (no screenshots), with
// responsive viewports, refresh and the server's real output tail.

import { api } from "./main";

interface DevServer {
  id: string;
  name: string;
  command: string;
  url: string;
  pid: number;
  alive: boolean;
  started: string;
}

let currentServerId = "";
let viewportWidth = "100%";

function el<T extends HTMLElement>(id: string): T | null {
  return document.getElementById(id) as T | null;
}

export function initPreview(): void {
  document.querySelectorAll<HTMLButtonElement>(".vp").forEach((btn) => {
    btn.addEventListener("click", () => {
      viewportWidth = btn.dataset.w ?? "100%";
      applyViewport();
      document.querySelectorAll<HTMLButtonElement>(".vp").forEach((b) => b.classList.remove("primary"));
      btn.classList.add("primary");
    });
  });
  el<HTMLSelectElement>("preview-server")?.addEventListener("change", () => {
    currentServerId = (el<HTMLSelectElement>("preview-server")?.selectedOptions[0]?.dataset.id) ?? "";
    refreshPreview();
    void showOutput();
  });
}

function applyViewport(): void {
  const frame = el<HTMLIFrameElement>("preview-frame");
  if (frame) frame.style.width = viewportWidth;
}

export async function pollServers(): Promise<void> {
  const res = await api<{ servers: DevServer[] }>("/api/servers");
  renderServers(res.servers ?? []);
}

function renderServers(servers: DevServer[]): void {
  const list = el<HTMLDivElement>("servers-list");
  const select = el<HTMLSelectElement>("preview-server");
  if (!list || !select) return;

  if (servers.length === 0) {
    list.innerHTML = `<div class="empty">No servers detected. Start one from the agent with run_command(background=true).</div>`;
    select.innerHTML = `<option value="">no running servers</option>`;
    return;
  }

  list.innerHTML = "";
  select.innerHTML = "";
  for (const s of servers) {
    const row = document.createElement("div");
    row.className = "server-row";
    row.innerHTML = `
      <span class="dot ${s.alive ? "ok" : ""}"></span>
      <span class="server-name">${escapeHtml(s.name)}</span>
      <span class="server-url mono">${escapeHtml(s.url || "url pending")}</span>
      <span class="server-pid">pid ${s.pid}</span>`;
    const stop = document.createElement("button");
    stop.className = "btn danger small";
    stop.textContent = "Stop";
    stop.addEventListener("click", () => void stopServer(s.id));
    row.appendChild(stop);
    list.appendChild(row);

    if (s.alive && s.url) {
      const opt = document.createElement("option");
      opt.value = s.url;
      opt.dataset.id = s.id;
      opt.textContent = `${s.url} — ${s.name}`;
      select.appendChild(opt);
    }
  }
  if (select.options.length === 0) {
    select.innerHTML = `<option value="">no running servers</option>`;
  } else if (currentServerId) {
    const found = Array.from(select.options).find((o) => o.dataset.id === currentServerId);
    if (found) select.value = found.value;
  }
  refreshPreview();
}

function escapeHtml(s: string): string {
  return s.replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c] ?? c,
  );
}

export function refreshPreview(): void {
  const select = el<HTMLSelectElement>("preview-server");
  const frame = el<HTMLIFrameElement>("preview-frame");
  if (!select || !frame) return;
  const url = select.value;
  if (url) {
    // reassigning src forces a real reload of the live application
    frame.src = "about:blank";
    window.setTimeout(() => {
      frame.src = url;
    }, 30);
  } else {
    frame.src = "about:blank";
    frame.srcdoc =
      `<!doctype html><html><body style="font-family:system-ui;background:#0b0e14;color:#9aa3b2;` +
      `display:grid;place-items:center;height:100vh;margin:0">` +
      `<p>No running development server.<br>Ask the agent to start one, then refresh.</p></body></html>`;
  }
}

async function showOutput(): Promise<void> {
  const out = el<HTMLPreElement>("server-output");
  if (!out || !currentServerId) return;
  const res = await api<{ output: string }>(`/api/servers/output?id=${encodeURIComponent(currentServerId)}`);
  out.hidden = false;
  out.textContent = res.output || "(no output yet)";
}

async function stopServer(id: string): Promise<void> {
  try {
    await api("/api/servers/stop", { method: "POST", body: JSON.stringify({ id }) });
    await pollServers();
  } catch (err) {
    console.warn(err);
  }
}

// keep the output pane fresh alongside polling
window.setInterval(() => {
  if (currentServerId) void showOutput().catch(() => undefined);
}, 5000);
