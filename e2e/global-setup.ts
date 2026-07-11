import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import { existsSync, mkdirSync, openSync, rmSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

type ManagedProcess = {
  name: string;
  pid: number;
};

const E2E_DIR = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(E2E_DIR, "..");
const WORKSPACE_DIR = path.join(REPO_ROOT, ".e2e-workspace");
const FIXTURE_DIR = path.join(WORKSPACE_DIR, "fixtures");
const STATE_FILE = path.join(WORKSPACE_DIR, "state.json");
const API_URL = "http://127.0.0.1:18000";
const WEB_URL = "http://127.0.0.1:15173";
const TOKEN = "e2e-token";

const started: ChildProcess[] = [];

async function globalSetup(): Promise<void> {
  cleanWorkspace();
  mkdirSync(path.join(WORKSPACE_DIR, "logs"), { recursive: true });

  try {
    // 造好导入 fixture（草稿本体改由测试内「开始创作」经真实 REST 建，不再预置项目）。
    seed("init", ["--fixture-dir", FIXTURE_DIR]);

    const api = startProcess("api", "uv", [
      "run",
      "uvicorn",
      "e2e.fixtures.app:create_app_from_env",
      "--factory",
      "--host",
      "127.0.0.1",
      "--port",
      "18000"
    ], {
      RUSHES_WORKSPACE_PATH: WORKSPACE_DIR,
      RUSHES_API_TOKEN: TOKEN,
      RUSHES_API_PORT: "18000",
      RUSHES_FS_ROOTS: FIXTURE_DIR
    });
    started.push(api);
    await waitForApi();

    const worker = startProcess("worker", "uv", [
      "run",
      "python",
      "-m",
      "apps.worker.main",
      WORKSPACE_DIR,
      "--worker-id",
      "e2e-worker",
      "--poll-interval",
      "0.5"
    ]);
    started.push(worker);

    // 直接启动已锁定依赖里的 Vite，避免本机全局 pnpm 版本影响 E2E 运行。
    const web = startProcess("web", path.join(REPO_ROOT, "apps/web/node_modules/.bin/vite"), [
      "--host",
      "127.0.0.1",
      "--port",
      "15173"
    ], {
      RUSHES_WEB_PROXY_TARGET: API_URL
    }, path.join(REPO_ROOT, "apps/web"));
    started.push(web);
    await waitForHttp(WEB_URL);

    writeState(started.map((child, index) => {
      const pid = child.pid;
      if (pid === undefined) {
        throw new Error(`missing pid for process ${index}`);
      }
      return { name: processName(index), pid };
    }));
  } catch (error) {
    stopStarted();
    throw error;
  }
}

function cleanWorkspace(): void {
  if (existsSync(WORKSPACE_DIR)) {
    rmSync(WORKSPACE_DIR, { recursive: true, force: true });
  }
  mkdirSync(FIXTURE_DIR, { recursive: true });
}

function seed(command: string, args: string[]): void {
  execFileSync("uv", ["run", "python", "e2e/fixtures/seed_draft.py", command, ...args], {
    cwd: REPO_ROOT,
    env: { ...process.env },
    stdio: "inherit"
  });
}

function startProcess(
  name: string,
  command: string,
  args: string[],
  extraEnv: Record<string, string> = {},
  cwd: string = REPO_ROOT
): ChildProcess {
  const logPath = path.join(WORKSPACE_DIR, "logs", `${name}.log`);
  const log = openSync(logPath, "a");
  return spawn(command, args, {
    cwd,
    env: { ...process.env, ...extraEnv },
    detached: true,
    stdio: ["ignore", log, log]
  });
}

async function waitForApi(): Promise<void> {
  await waitForHttp(`${API_URL}/api/drafts`, {
    headers: { Authorization: `Bearer ${TOKEN}` }
  });
}

async function waitForHttp(url: string, init: RequestInit = {}): Promise<void> {
  const deadline = Date.now() + 60_000;
  let lastError: unknown = null;
  while (Date.now() < deadline) {
    try {
      const response = await fetch(url, init);
      if (response.ok) {
        return;
      }
      lastError = new Error(`${url} returned ${response.status}`);
    } catch (error) {
      lastError = error;
    }
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  throw lastError instanceof Error ? lastError : new Error(`timed out waiting for ${url}`);
}

function writeState(processes: ManagedProcess[]): void {
  writeFileSync(
    STATE_FILE,
    JSON.stringify(
      {
        apiUrl: API_URL,
        webUrl: WEB_URL,
        token: TOKEN,
        workspaceDir: WORKSPACE_DIR,
        fixtureDir: FIXTURE_DIR,
        processes
      },
      null,
      2
    )
  );
}

function stopStarted(): void {
  for (const child of started.toReversed()) {
    if (child.pid !== undefined) {
      stopProcess(child.pid);
    }
  }
}

function stopProcess(pid: number): void {
  try {
    process.kill(-pid, "SIGTERM");
  } catch {
    try {
      process.kill(pid, "SIGTERM");
    } catch {
      return;
    }
  }
}

function processName(index: number): string {
  return ["api", "worker", "web"][index] ?? `process_${index}`;
}

export default globalSetup;
