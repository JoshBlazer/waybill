# Stage 0 walkthrough — Foundations

## What was built

| Piece | Where | Why |
|---|---|---|
| Documents | `docs/`, `CLAUDE.md`, `README.md` | The product, the invariants and the risks are written down before code, so later stages are checked against them rather than invented along the way. |
| Test-money guard | `api/internal/safety` | The one property that must hold from the first commit: Waybill cannot touch real funds. |
| OpenAPI contract and codegen | `openapi/waybill.yaml` → `api/internal/httpapi/api.gen.go`, `web/src/lib/api/schema.d.ts` | The contract is defined once. A mismatch fails `go build`, `tsc` or `make check-gen` instead of failing in production. |
| One binary, five subcommands | `api/cmd/waybill` | `api`, `worker`, `watcher` share code but run as separate processes, so one can be killed without the others ("pull the plug"). `migrate` and `healthcheck` serve Compose. |
| Migrations | `api/db/migrations` (goose, embedded) | The migration path is proven end to end (binary, Compose, testcontainers, CI) before the ledger schema depends on it. |
| sqlc | `api/db/queries`, `api/internal/store` | Proves the SQL-first query path. The health endpoint uses the generated `Ping` query. |
| Mock stablecoin | `contracts/src/MockStablecoin.sol` | Six decimals, like USDC, so decimal bugs show up locally. It refuses to deploy or mint outside the test chain ids. |
| Web scaffold | `web/` | Design tokens, the stamp and perforation motifs, and a status page that fetches `/v1/health` at request time through the generated types. |
| Local stack | `deploy/compose.yaml` | PostgreSQL, Anvil, migrate, API, web. Health-checked, with `--wait`, so `make up` returns only when everything answers. |
| CI | `.github/workflows/ci.yml` | Separate jobs (api, contracts, web, codegen drift, Compose smoke test) so a failure points at one part. |

## How the guard works

1. **Static check.** Every `WAYBILL_CHAINS` entry must be one of five network ids. A Paystack key, if set, must start with `sk_test_`. All problems are reported together.
2. **Live check.** Each EVM RPC is asked for `eth_chainId`. A mismatch, an RPC error or a timeout refuses startup. Networks without a live check (Bitcoin, until stage 5) also refuse. This is the "couldn't find out is not yes" rule applied to safety.
3. **No leaks.** Errors print the RPC host, never the URL (URLs often hold API keys), and never echo a Paystack key.

The tests drive the real `EVMProber` (go-ethereum's `ethclient`) against an `httptest` JSON-RPC server, so the code that runs in production is the code that is tested. In the real container, `WAYBILL_CHAINS=evm:84532=http://anvil:8545` exits 1 with `RPC at anvil:8545 reports chain id 31337, want 84532`.

## Decisions worth defending

- **Live chain-ID check (ADR-023).** An allowlist alone trusts the label in config. The check asks the chain itself.
- **NUMERIC(78,0) (ADR-022).** `BIGINT` cannot hold a `uint256`. Exactness matters more than the small arithmetic cost.
- **Codegen tools as `go tool` dependencies (ADR-021).** Versions are pinned in `go.mod`, with no global installs. The cost: `go mod download` pulls the tools' dependency trees, so the Dockerfile builds without it.
- **Request-time fetch on the status page.** Next.js 16 with Cache Components prerenders a static shell. `connection()` inside a `<Suspense>` boundary keeps the API call out of the build and streams it per request.

## Interview questions

1. Why does the guard ask the RPC for its chain id when the config already names the network? What attack or mistake does that catch?
2. The guard fails closed when an RPC is unreachable. What is the operational cost of that choice, and why is it still right for a payments system?
3. Why are RPC URLs redacted to the host in errors? Name a provider whose URLs carry credentials.
4. What happens, step by step, when someone edits `openapi/waybill.yaml` and forgets to run `make gen`? Which job fails, and why?
5. Why one binary with subcommands instead of three services? What does it make easier, and what does it not solve?
6. Why `NUMERIC(78,0)` and not `BIGINT` or `DECIMAL(38,18)` for amounts? How will Go code represent these values?
7. The first migration is empty. What does it prove, and why bother before the ledger schema exists?
8. Why does the mock stablecoin use six decimals, and what class of bug does that surface early?
9. How does `make up` know the stack is ready? Trace the dependency and health-check chain from PostgreSQL to the web container.
10. The home page's status block is rendered at request time. What would go wrong if it were prerendered at build time, and which Next.js API prevents that here?
