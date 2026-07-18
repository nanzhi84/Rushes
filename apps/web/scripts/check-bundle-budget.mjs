#!/usr/bin/env node
// 主入口 chunk 的 gzip 体积预算闸门（#95 F6）。在 vite build 之后运行：解析 dist/index.html
// 里的 module 入口脚本，gzip 后与预算对比，超预算即非零退出让 CI 变红。
// 单位与 vite 输出一致（1 kB = 1000 B，gzip 默认级别），便于与构建日志直接对照。

import { readFileSync } from "node:fs";
import { gzipSync } from "node:zlib";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const BUDGET_KB = 350; // 主 chunk gzip 上限

const webDir = join(dirname(fileURLToPath(import.meta.url)), "..");
const distDir = join(webDir, "dist");

let indexHtml;
try {
  indexHtml = readFileSync(join(distDir, "index.html"), "utf8");
} catch {
  console.error("check-bundle-budget: 未找到 dist/index.html，请先执行 `vite build`。");
  process.exit(1);
}

const match = indexHtml.match(/<script[^>]*type="module"[^>]*src="([^"]+)"/);
if (!match) {
  console.error("check-bundle-budget: dist/index.html 中未找到 module 入口脚本。");
  process.exit(1);
}

const entryRel = match[1].replace(/^\//, "");
const raw = readFileSync(join(distDir, entryRel));
const gzipKb = gzipSync(raw).length / 1000;
const rawKb = raw.length / 1000;

console.log(`主入口 chunk: ${entryRel.split("/").pop()}`);
console.log(`  原始 ${rawKb.toFixed(2)} kB / gzip ${gzipKb.toFixed(2)} kB（预算 ${BUDGET_KB} kB gzip）`);

if (gzipKb > BUDGET_KB) {
  console.error(
    `✗ 主 chunk 超出 bundle 预算 ${(gzipKb - BUDGET_KB).toFixed(2)} kB。` +
      "请把重依赖拆成懒加载/独立 chunk（如路由级 code-split 或 manualChunks）后再合入。"
  );
  process.exit(1);
}

console.log(`✓ 在预算内，余量 ${(BUDGET_KB - gzipKb).toFixed(2)} kB`);
