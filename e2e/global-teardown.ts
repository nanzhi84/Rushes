import { existsSync, readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

type StateFile = {
  processes?: Array<{ name: string; pid: number }>;
};

const E2E_DIR = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(E2E_DIR, "..");
const STATE_FILE = path.join(REPO_ROOT, ".e2e-workspace", "state.json");

async function globalTeardown(): Promise<void> {
  if (!existsSync(STATE_FILE)) {
    return;
  }
  const state = JSON.parse(readFileSync(STATE_FILE, "utf8")) as StateFile;
  for (const item of (state.processes ?? []).toReversed()) {
    stopProcess(item.pid, "SIGTERM");
  }
  await new Promise((resolve) => setTimeout(resolve, 1_000));
  for (const item of (state.processes ?? []).toReversed()) {
    stopProcess(item.pid, "SIGKILL");
  }
}

function stopProcess(pid: number, signal: NodeJS.Signals): void {
  try {
    process.kill(-pid, signal);
  } catch {
    try {
      process.kill(pid, signal);
    } catch {
      return;
    }
  }
}

export default globalTeardown;
