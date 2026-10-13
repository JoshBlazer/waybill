// Measures the JavaScript a page makes the browser download up front
// (gzipped) and fails if it exceeds the budget in docs/DESIGN.md §6.
// Uses only Node built-ins. Run against a live stack:
//
//   node scripts/js-budget.mjs http://localhost:3000 /t/WB-… 185000
//
// It also fails if wallet code (wagmi/viem) appears in the initial load,
// which must only load after the payer chooses a wallet.
import { gzipSync } from "node:zlib";

const [base, path, budgetArg] = process.argv.slice(2);
if (!base || !path || !budgetArg) {
  console.error("usage: js-budget.mjs <base-url> <path> <budget-bytes-gzip>");
  process.exit(2);
}
const budget = Number(budgetArg);

const html = await (await fetch(new URL(path, base))).text();
const scripts = [...new Set(html.match(/\/_next\/static\/[^"']+\.js/g) ?? [])];
let total = 0;
const walletChunks = [];
for (const src of scripts) {
  const body = await (await fetch(new URL(src, base))).text();
  const size = gzipSync(body).length;
  total += size;
  if (/wagmi|UserRejectedRequestError|eth_requestAccounts/.test(body))
    walletChunks.push(src);
}

const kb = (n) => `${(n / 1000).toFixed(1)} KB`;
console.log(
  `${path}: ${scripts.length} scripts, ${kb(total)} gzip (budget ${kb(budget)})`,
);
let failed = false;
if (total > budget) {
  console.error(`FAIL: over budget by ${kb(total - budget)}`);
  failed = true;
}
if (walletChunks.length > 0) {
  console.error(
    `FAIL: wallet code in the initial load: ${walletChunks.join(", ")}`,
  );
  failed = true;
}
process.exit(failed ? 1 : 0);
