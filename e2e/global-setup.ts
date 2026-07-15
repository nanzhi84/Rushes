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
const WORKSPACE_DIR = process.env.RUSHES_E2E_WORKSPACE ?? path.join(REPO_ROOT, ".playwright-workspace");
const FIXTURE_DIR = path.join(WORKSPACE_DIR, "fixtures");
const BIN_DIR = path.join(WORKSPACE_DIR, "bin");
const STATE_FILE = path.join(WORKSPACE_DIR, "state.json");
const API_PORT = process.env.RUSHES_E2E_API_PORT ?? "18001";
const WEB_PORT = process.env.RUSHES_E2E_WEB_PORT ?? "15174";
const API_URL = `http://127.0.0.1:${API_PORT}`;
const WEB_URL = `http://127.0.0.1:${WEB_PORT}`;
const TOKEN = "e2e-token";

const started: ChildProcess[] = [];

async function globalSetup(): Promise<void> {
  cleanWorkspace();
  mkdirSync(path.join(WORKSPACE_DIR, "logs"), { recursive: true });

  try {
    makeFixtures();
    buildGoBinaries();

    const api = startProcess("api", path.join(BIN_DIR, "rushes-api"), [
      "-workspace",
      WORKSPACE_DIR,
      "-port",
      API_PORT,
      "-token",
      TOKEN
    ], {
      RUSHES_WORKSPACE_PATH: WORKSPACE_DIR,
      RUSHES_API_TOKEN: TOKEN,
      RUSHES_API_PORT: API_PORT,
      RUSHES_FS_ROOTS: FIXTURE_DIR,
      RUSHES_DASHSCOPE_API_KEY: ""
    });
    started.push(api);
    await waitForApi();

    const worker = startProcess("worker", path.join(BIN_DIR, "rushes-worker"), [
      "-workspace",
      WORKSPACE_DIR,
      "-concurrency",
      "2"
    ], {
      RUSHES_DASHSCOPE_API_KEY: ""
    });
    started.push(worker);

    // 直接启动已锁定依赖里的 Vite，避免本机全局 pnpm 版本影响 E2E 运行。
    const web = startProcess("web", path.join(REPO_ROOT, "apps/web/node_modules/.bin/vite"), [
      "--host",
      "127.0.0.1",
      "--port",
      WEB_PORT
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

function buildGoBinaries(): void {
  mkdirSync(BIN_DIR, { recursive: true });
  execFileSync("go", ["build", "-tags=e2e_scaffold", "-o", path.join(BIN_DIR, "rushes-api"), "./cmd/api"], {
    cwd: path.join(REPO_ROOT, "go"),
    stdio: "inherit"
  });
  execFileSync("go", ["build", "-tags=e2e_scaffold", "-o", path.join(BIN_DIR, "rushes-worker"), "./cmd/worker"], {
    cwd: path.join(REPO_ROOT, "go"),
    stdio: "inherit"
  });
}

function makeFixtures(): void {
  for (const [name, source] of [
    ["path3-fixture.mp4", "testsrc2=size=320x568:rate=30:duration=2"],
    ["path3-fixture-2.mp4", "color=c=blue:size=320x568:rate=30:duration=2"],
    ["understanding-cancel-a.mp4", "color=c=red:size=320x568:rate=30:duration=2"],
    ["understanding-cancel-b.mp4", "color=c=green:size=320x568:rate=30:duration=2"]
  ] as const) {
    execFileSync(
      "ffmpeg",
      ["-y", "-loglevel", "error", "-f", "lavfi", "-i", source, "-c:v", "libx264", "-pix_fmt", "yuv420p", path.join(FIXTURE_DIR, name)],
      { stdio: "inherit" }
    );
  }
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
