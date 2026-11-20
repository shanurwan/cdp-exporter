// Load generator for the browserless soak. Raw CDP over Node's global
// WebSocket, so it needs no npm install.
//
//   node load.mjs <ws-endpoint> <workers> <seconds>   supervisor
//   node load.mjs <ws-endpoint> session <kind>         one session (child)
//
// The supervisor keeps <workers> sessions running and serves the test pages
// on 127.0.0.1:8099. Each session is a child process, so an "abandon" session
// can be SIGKILLed the way a crashed client dies: the socket closes without a
// goodbye and the browser is left to the pool to clean up.
import { spawn } from "node:child_process";
import http from "node:http";

const [endpoint, mode, arg, arg2] = process.argv.slice(2);
const PAGES = "http://127.0.0.1:8099";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const rand = (a, b) => a + Math.random() * (b - a);

// Session mix, by weight. WITHOUT=hog,crash drops kinds.
const KINDS = [
  ["clean", 55],    // open 1-3 tabs, work, disconnect
  ["abandon", 15],  // client SIGKILLed mid-session
  ["overrun", 8],   // holds the browser past the pool's TIMEOUT
  ["crash", 10],    // a tab navigates to chrome://crash
  ["leaky", 7],     // a page that keeps allocating for the whole session
  ["hog", 5],       // a page that allocates until something gets killed
].filter(([k]) => !(process.env.WITHOUT || "").split(",").includes(k));

const page = {
  "/heavy": `<!doctype html><title>heavy</title><body><canvas id=c width=800 height=600></canvas>
<iframe src="/frame"></iframe><script>
const ul = document.createElement('ul');
for (let i = 0; i < 20000; i++) { const li = document.createElement('li'); li.textContent = 'row ' + i; ul.appendChild(li); }
document.body.appendChild(ul);
const w = new Worker(URL.createObjectURL(new Blob(['let x=[];setInterval(()=>{x=new Array(1e5).fill(Math.random())},100)'])));
const g = c.getContext('2d'); setInterval(() => { for (let i = 0; i < 500; i++) { g.fillStyle = '#' + (Math.random()*0xffffff|0).toString(16); g.fillRect(Math.random()*800, Math.random()*600, 20, 20); } }, 50);
</script>`,
  "/frame": `<!doctype html><body><script>let a=[];setInterval(()=>{a.push(new Array(1e4).fill(1));if(a.length>200)a=[]},100)</script>`,
  "/leaky": `<!doctype html><title>leaky</title><script>const keep=[];setInterval(()=>keep.push(new Float64Array(6e5)),200)</script>`,
  // About 200 MB/s until the renderer is killed. Much faster than this and
  // the cgroup OOM killer takes the pool's Node process down with it.
  "/hog": `<!doctype html><title>hog</title><script>const keep=[];setInterval(()=>keep.push(new Float64Array(2.5e6).fill(1)),100)</script>`,
};

// ---------------------------------------------------------------- session
class CDP {
  constructor(url) { this.url = url; this.id = 0; this.wait = new Map(); }
  open() {
    return new Promise((ok, err) => {
      this.ws = new WebSocket(this.url);
      this.ws.onopen = ok;
      this.ws.onerror = (e) => err(new Error("ws error " + (e.message || "")));
      this.ws.onclose = () => { for (const [, w] of this.wait) w.err(new Error("closed")); this.wait.clear(); };
      this.ws.onmessage = (e) => {
        const m = JSON.parse(e.data);
        const w = m.id && this.wait.get(m.id);
        if (w) { this.wait.delete(m.id); m.error ? w.err(new Error(m.error.message)) : w.ok(m.result); }
      };
    });
  }
  send(method, params = {}, sessionId) {
    const id = ++this.id;
    this.ws.send(JSON.stringify({ id, method, params, sessionId }));
    return new Promise((ok, err) => {
      this.wait.set(id, { ok, err });
      setTimeout(() => { if (this.wait.delete(id)) err(new Error("timeout " + method)); }, 30000);
    });
  }
  async tab(url) {
    const { targetId } = await this.send("Target.createTarget", { url: "about:blank" });
    const { sessionId } = await this.send("Target.attachToTarget", { targetId, flatten: true });
    this.send("Page.navigate", { url }, sessionId).catch(() => {});
    return sessionId;
  }
}

async function session(kind) {
  const c = new CDP(endpoint);
  await c.open();
  const tabs = 1 + Math.floor(Math.random() * 3);
  switch (kind) {
    case "clean":
    case "abandon":
      for (let i = 0; i < tabs; i++) await c.tab(PAGES + "/heavy");
      await sleep(rand(3000, 12000));
      break;
    case "overrun":
      await c.tab(PAGES + "/heavy");
      await sleep(120000); // the pool's TIMEOUT ends it first
      break;
    case "crash":
      await c.tab(PAGES + "/heavy");
      await sleep(rand(1000, 3000));
      await c.tab("chrome://crash");
      await sleep(rand(2000, 5000));
      break;
    case "leaky":
      await c.tab(PAGES + "/leaky");
      await sleep(rand(15000, 40000));
      break;
    case "hog":
      await c.tab(PAGES + "/hog");
      await sleep(30000);
      break;
  }
  c.ws.close();
}

if (mode === "session") {
  session(arg).then(() => process.exit(0), (e) => { console.error(arg, e.message); process.exit(2); });
} else {
  // ------------------------------------------------------------- supervisor
  const workers = Number(mode), seconds = Number(arg);
  const stats = {};
  const count = (k, r) => { (stats[k] ??= { ok: 0, failed: 0, killed: 0 })[r]++; };
  http.createServer((req, res) => {
    const body = page[req.url.split("?")[0]];
    res.writeHead(body ? 200 : 404, { "content-type": "text/html" });
    res.end(body || "");
  }).listen(8099, "127.0.0.1");

  const pick = () => {
    let r = Math.random() * KINDS.reduce((s, [, w]) => s + w, 0);
    for (const [k, w] of KINDS) if ((r -= w) < 0) return k;
    return "clean";
  };
  const end = Date.now() + seconds * 1000;
  const loop = async () => {
    while (Date.now() < end) {
      const kind = pick();
      await new Promise((done) => {
        const child = spawn(process.execPath, [new URL(import.meta.url).pathname, endpoint, "session", kind], { stdio: ["ignore", "ignore", "pipe"] });
        let killed = false;
        if (kind === "abandon") setTimeout(() => { killed = true; child.kill("SIGKILL"); }, rand(1500, 6000));
        let err = "";
        child.stderr.on("data", (d) => (err += d));
        child.on("exit", (code) => {
          count(kind, killed ? "killed" : code === 0 ? "ok" : "failed");
          if (code && !killed && err) console.error(new Date().toISOString(), err.trim().slice(0, 200));
          done();
        });
      });
      await sleep(rand(200, 1500));
    }
  };
  const report = setInterval(() => console.log(new Date().toISOString(), JSON.stringify(stats)), 60000);
  await Promise.all(Array.from({ length: workers }, loop));
  clearInterval(report);
  console.log("final", JSON.stringify(stats));
  process.exit(0);
}
