// Starts N headless Chrome browsers the way Puppeteer's default launch does
// (full Chrome in new headless mode; BROWSER=shell uses headless_shell): a fresh
// /tmp/puppeteer_dev_chrome_profile-* directory and --remote-debugging-port=0
// for each, all children of this Node process. It then stays alive, like a
// scraping worker. When it is PID 1 without an init, it never reaps the
// Chrome children that get reparented to it.
import { spawn } from "node:child_process";
import { mkdtempSync, readdirSync } from "node:fs";

const n = Number(process.argv[2] || 3);
const shell = process.env.BROWSER === "shell";
const dir = readdirSync("/ms-playwright").find((d) => d.startsWith(shell ? "chromium_headless_shell-" : "chromium-"));
const bin = `/ms-playwright/${dir}/chrome-linux/${shell ? "headless_shell" : "chrome"}`;
for (let i = 0; i < n; i++) {
  const profile = mkdtempSync("/tmp/puppeteer_dev_chrome_profile-");
  const c = spawn(bin, ["--headless", "--no-sandbox", "--disable-gpu", "--remote-debugging-port=0", `--user-data-dir=${profile}`, "about:blank"], { stdio: "ignore" });
  console.log(`browser ${c.pid} ${profile}`);
}
setInterval(() => {}, 1 << 30);
