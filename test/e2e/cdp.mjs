// Tiny CDP driver for the e2e checks (Node >= 22 has a global WebSocket).
// usage: node cdp.mjs <http-base> open <n> | crash
const [base, cmd, arg] = process.argv.slice(2);
const host = { Host: "localhost" };
const json = (p, m = "GET") => fetch(base + p, { method: m, headers: host }).then((r) => r.json());

async function browserCall(method, params = {}) {
  const v = await json("/json/version");
  const ws = new WebSocket(v.webSocketDebuggerUrl.replace(/ws:\/\/[^/]+/, base.replace("http", "ws")));
  await new Promise((ok, err) => { ws.onopen = ok; ws.onerror = err; });
  const res = await new Promise((ok) => {
    ws.onmessage = (e) => { const m = JSON.parse(e.data); if (m.id === 1) ok(m); };
    ws.send(JSON.stringify({ id: 1, method, params }));
  });
  ws.close();
  return res;
}

if (cmd === "open") {
  for (let i = 0; i < Number(arg || 1); i++) {
    // a new browser context per session, the way Playwright and browserless isolate clients
    const ctx = await browserCall("Target.createBrowserContext");
    await browserCall("Target.createTarget", { url: "about:blank", browserContextId: ctx.result.browserContextId });
  }
} else if (cmd === "crash") {
  const pages = (await json("/json/list")).filter((t) => t.type === "page");
  const t = await browserCall("Target.createTarget", { url: "about:blank" });
  // Navigating a tab to chrome://crash makes its renderer crash on purpose.
  const ws = new WebSocket(`${base.replace("http", "ws")}/devtools/page/${t.result.targetId}`);
  await new Promise((ok) => (ws.onopen = ok));
  ws.send(JSON.stringify({ id: 1, method: "Page.navigate", params: { url: "chrome://crash" } }));
  await new Promise((r) => setTimeout(r, 1500));
  ws.close();
  console.log("crashed", t.result.targetId, "pages before", pages.length);
}
process.exit(0);
