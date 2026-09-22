<!--
category: engineering
title: Elixir cutover inventory
summary: Go production surface vs nexus_ex progress for Manus review. Fact / inference / hypothesis separated.
order: 91
status: draft
-->

# Elixir cutover inventory (Manus briefing)

작성: 2026-09-22. 대상 독자: 외부 기술 검토 (Manus).
프로덕션 서빙 바이너리는 여전히 Go (`ghcr.io/fun-fx/ffx_nexus`).
Elixir 이미지 이름은 `ghcr.io/fun-fx/nexus_ex`. 두 이미지를 섞어
`helm upgrade` 하지 않는다. dual-write / serial Go→Elixir proxy 금지.

역할이 다른 문서:

| 문서 | 역할 |
| --- | --- |
| [platform.md](platform.md) | Go가 고객에게 **무엇을 파는지** |
| [p1-3-failure-context.md](p1-3-failure-context.md) | darwin P1-3 측정 숫자 |
| [p1-3-hex-decision.md](p1-3-hex-decision.md) | HTTP 스택을 지금 바꾸지 말 것 |
| **이 파일** | Go 전면 × Elixir 매트릭스 (live / stub / missing / fail) |

문체: **사실**은 코드·측정·lockfile. **추론**은 설계 판단. **가설**은
Linux gate에서 아직 인과가 없는 것. Darwin RSS는 방향성만 — pass/fail
산식에 넣지 않는다.

## 0. 한 줄

Nexus는 Go 단일 바이너리 LLM 게이트웨이(`:8080`) + React 콘솔(`:8081`)이다.
Elixir 우산 `nexus_ex`는 계약 오라클을 Go에 두고 같은 포트, 같은 Postgres
SQL ledger, 같은 Redis 키 이름을 재구현 중이다. Chat SSE SHA golden은
통과. P1-3 darwin soak는 **FAIL** (TTFT p50 1.77×, RSS/stream 2.35×).
live load-balancer canary는 Linux cgroup `memory.current` 통과 전 금지.
Go를 삭제하지 않는다.

## 1. Go가 파는 것 (사실)

출처: 이 저장소 `docs/platform.md`, `internal/gateway/server.go`,
`internal/evalplugin/types.go`, Helm `deploy/helm/nexus`.

### 1.1 프로세스와 데이터

한 프로세스, 두 리스너. `nexus serve --local`은 `~/.nexus` 아래 사설
Postgres를 띄운다. 요청 경로는 무상태다. 스토어가 없으면 기능만 줄어들고
부팅은 된다 (zero-dep).

| 스토어 | 권위 있는 데이터 | 없으면 |
| --- | --- | --- |
| Postgres | users, virtual keys, BYOK creds, eval plugins, audit | 콘솔·키·SSO 불가 |
| Redis | RPM, budget, semantic cache | 인메모리 limiter (단일 replica) |
| ClickHouse | `gateway_traces`, `eval_scores`, `mcp_tool_logs` | 트레이스 live-only, 라우팅 통계 없음 |

### 1.2 `/v1` (Go `internal/gateway/server.go`)

- `GET /healthz`, `GET /readyz`
- `POST /v1/chat/completions` — raw SSE passthrough, non-standard 필드 보존
- `POST /v1/responses`, `/v1/messages` (Anthropic), `/v1/embeddings`,
  `/v1/moderations`, `/v1/images/generations`
- `GET /v1/models`
- `GET /v1/mcp/servers`, `POST /v1/mcp/servers/{id}/tools/list|call`

에러는 OpenAI 셰이프. Cursor Agent hybrid body는 chat으로 rewrite.
Grid 307 cross-origin hop에서 `Authorization` strip. 기본 키 모드
`strict_byok`. 비용은 `internal/gateway/pricing.go` 토큰×정가 + LiteLLM
JSON 6h poll (불일치는 로그/게이지, 테이블 rewrite 아님). Grid는
`usage.estimated_cost`가 양수면 그 값. 응답/트레일러 `x-nexus-cost-usd`.
프롬프트 바디는 기본 CH 미저장 (`NEXUS_CAPTURE_TRACE_CONTENT`).

### 1.3 라우터와 Eval

`internal/router`: quality × cost × latency 가중, failover, 그룹
`fast=a,b;smart=c,d`. 롤링 입력은 CH `eval_scores`와 `benchmark_runs`.

Eval은 두 모드만:

1. **heuristic** — 프로세스 안, egress 0. 닫힌 이름: `contains`, `pii`,
   `exact_match`, `rouge_l`, `hf_evaluate`, `lighteval`, `ragas`
   (뒤 셋은 Python sidecar 가능).
2. **external** — YAML, OTLP send, webhook/poll collect. 닫힌 타입:
   langsmith, langfuse, datadog, braintrust, arize, confident_ai,
   arize_phoenix, otel / webhook / otel_collector.

`NEXUS_EVAL_PLUGIN_ONLY`는 내장 PII/completeness seed를 건너뛴다.
`NEXUS_EVAL_PURGE_LEGACY_PROFILES_ON_BOOT`는 파괴적 companion — 외부
PII 커버 확인 후에만.

벤치마크는 `internal/benchmark` — PrimeIntellect hosted eval. Nexus는
harness를 돌리지 않고 행을 들고 poll한다. `via_gateway`면 추론을 이
게이트로 되돌린다.

### 1.4 콘솔, 보안, 배포

React SPA (`web/`)는 `web/embed.go`로 바이너리에 들어간다. `/api`는
auth, keys, credentials, eval plugins/profiles/config, benchmarks,
MCP registry/settings, observability (traces/spend/dashboard),
gateway policy (guardrails, cache, alerting), users, audit, docs.

보안: 호출자는 `nxs_live_…`. provider secret은 AES-256-GCM
(`NEXUS_MASTER_KEY`). vkey는 SHA-256. 세션 쿠키, CSRF, CSP
`connect-src`는 `NEXUS_PUBLIC_WEB_ORIGINS`. `X-Forwarded-For`는
`trustedProxyCIDRs`일 때만. 감사는 append-only. enterprise
NetworkPolicy는 default-deny, 피어 명단 없으면 install refuse.

배포: Helm `deploy/helm/nexus` (DB는 설치하지 않음), Docker,
goreleaser 바이너리, npx `@ffxnexus/nexus`. Python `eval-service/`는
옵션 sidecar. 메인터너 프로드는 별도 private ops repo.

Go 패키지 지도는 [platform.md § Package map](platform.md)과 같다.
이 재고에서 특히 Elixir와 대비할 패키지: `internal/cron`,
`internal/evalbatch`, `internal/semcache`, `internal/limiter` (IP
limiter 포함), `internal/observability` (Prometheus, OTLP, Metabase
bootstrap, live WS hub), `internal/localdb`, `internal/egress`,
`internal/console` (invite, evidence, pricing drift, quality router).

## 2. Elixir가 있는 곳 (사실)

저장소: `../nexus_ex` (이 워크스페이스와 형제). umbrella 앱:
`nexus_contracts`, `nexus_core`, `nexus_data`, `nexus_gateway`,
`nexus_workers`, `nexus_console`, `nexus_release`.

계약 픽스처는 `apps/nexus_contracts/priv/fixtures`에 Go SHA로
vendor. `FFX_NEXUS=… ./scripts/sync_fixtures.sh`.

PHASES 0–8은 `nexus_ex/PHASES.md`. P8 **산출물**(이미지·Helm overlay·
boot refuse)은 트리에 있다. **라이브 LB 가중치는 없다.**

로컬 마지막 `mix test`: 앱별 합 약 219 tests, 0 failures
(2026-09-22). 이는 SHA·단위·LiveView 렌더 보장이지 Linux soak 보장이
아니다.

### 2.1 매트릭스

등급: **live** = 코드+테스트로 끝-끝 또는 단위 경로가 있음.
**stub** = UI/API가 있으나 벤더·CH·매니저가 고의로 비어 있음.
**partial** = 라우트는 있고 Go 전체 어댑터는 아님.
**missing** = Go에 있고 Elixir에 없음.
**fail** = 측정이 한도를 넘음.

| 영역 | Go | Elixir | 등급 |
| --- | --- | --- | --- |
| 리스너 | 한 바이너리 :8080/:8081 | Bandit gateway + Phoenix console | live |
| Chat SSE raw bytes | `internal/gateway` | `Nexus.Gateway.RawSSE` + Mint HTTP/1 `:passive` | SHA **live**; soak **fail** |
| responses / messages | 풀 rewrite | `Responses` / `Messages` shim | partial |
| embeddings / moderations / images | 풀 어댑터 | `Nexus.Gateway.OpenAI.*` unary proxy | partial |
| `GET /v1/models` | catalog | `Models` | live (단위) |
| MCP `/v1/mcp/*` | 실제 manager | `Application.get_env(:mcp_manager)` nil → 503 `mcp_disabled` | 골격 |
| MCP 콘솔 registry/library/settings | `internal/console` + `internal/mcp` | LiveView + Memory store + YAML `decode_spec` | UI live; 게이트 실행과 분리 |
| PG users / vkeys / sessions / AES-GCM | core | Ecto **read**, custom `Migrator` (Ecto.Migrator 아님) | live (단위) |
| Redis RPM `nexus:rpm:` | limiter | `Nexus.Data.Limiter` Redix + memory fallback | live (단위) |
| CH insert | native `NEXUS_CLICKHOUSE_URL` | HTTP `NEXUS_CLICKHOUSE_HTTP_URL`, RowBinary | live 코드; URL 없으면 `:disabled` |
| CH select traces/spend/dashboard/mcp logs | observability reader | `Nexus.Console.Observability` + JSONEachRow | live 코드; 빈 배너 when disabled |
| Eval 2모드 dispatcher | evalplugin + worker | `Nexus.Eval.Dispatcher` + Oban send/poll | 단위 live |
| Heuristic 메트릭 | 7 이름 (3개는 Python) | `contains`, `pii`, `exact_match`, `rouge_l`만. `hf_evaluate`/`lighteval`/`ragas`는 거절하고 external로 안내 (P6: Python sidecar 제거) | partial (의도된 축소) |
| Guardrails / alert webhook / cache **콘솔** | gateway + console API | LiveView + `Application.put_env` | UI live |
| Semantic cache **요청 경로** | `internal/semcache` Redis+embed | 콘솔 스냅샷만, gateway hot path에 모듈 없음 | missing (경로) |
| Rank / quality-aware pick | `internal/router` + CH | `/api/routing` synthetic seed; `/eval/routing` LiveView | **stub** |
| Prime Hub benchmarks | `internal/benchmark` | Memory rows + `vendor_note` Phase 4 unbound | **stub** |
| Connectors 페이지 | Observability.tsx 정적 카드 | LiveView + env on/off; eval live_count=0 | stub/UI |
| Cron / evalbatch / invite / evidence | 해당 internal 패키지 | 대응 앱 없음 | **missing** |
| IP limiter | `limiter/iplimiter.go` | 미확인/없음 | **missing** |
| Prometheus / OTLP recorder / Metabase bootstrap / live WS | `internal/observability` | Connectors 스니펫만 | **missing** / 부분 |
| `--local` embedded PG, npx installer | `internal/localdb`, `npx/` | 없음 | **missing** |
| Grid 307 / 임의 OpenAI-compat base URL | `providers/` | Mint unary 일부 | **partial / missing** |
| NetworkPolicy D-2b tests | `internal/contracttest` | Elixir 차트만 | **missing** |
| Helm | `deploy/helm/nexus` | `deploy/helm/nexus-ex` 두 번째 릴리스 | 산출물; 라이브 스플릿 아님 |
| Boot refuse serial proxy / dual-write | (정책 문서) | `Nexus.Release.Canary.assert_boot!/1` | live |
| P1-3 Linux cgroup | `scripts/load_baseline` | darwin FAIL만 `GATE.md` / `priv/gate/*.json` | **fail**; Linux 미실행 |

콘솔 LiveView 라우트는 Go SPA와 맞추려 했다. `nexus_ex` PR #1–#5
(MCP, Benchmarks stub, Eval routing stub, Connectors, Placeholder 제거 +
`/gateway/routing` → `/eval/routing` redirect). PHASES.md의
“Remaining MCP shells stay placeholders”는 **stale** — 이 작업에서 고친다.

### 2.2 Hot-path bans (Elixir README, 사실)

- 요청마다 global GenServer 없음
- long stream에 Finch 없음 (Mint가 conn 소유)
- raw SSE JSON 재인코드 없음
- audit fail-stop에 Oban 없음
- RPM 권위에 PubSub 없음
- Go와 같은 커맨드에서 Redis/PG dual-write 없음
- serial Go→Elixir proxy 없음 (TTFT 두 배); 분할은 LB만

### 2.3 Lockfile (2026-09-22, 사실)

- Mint **1.10.0** (Hex 결정: **≥1.10.1 pin**, 아직 미적용)
- Bandit **1.12.5** (하한 충족)
- Telemetry **1.4.2**

## 3. 핫패스와 P1-3

**사실.** Chat stream 경로:

Bandit (`num_acceptors: 64`) → Plug.Router (`RequestID`, `ConsoleCORS`)
→ Auth → `Chat.call` (`read_body` + `Jason.decode` + Guardrails +
`Pipeline.prepare`) → `RawSSE.proxy` → **요청마다**
`Mint.HTTP.connect` HTTP/1 `:passive` + `nodelay` → `send_chunked` →
`Mint.HTTP.recv` → `Plug.Conn.chunk/2`.

darwin/arm64, mock SSE, 256 streams × 10s (`priv/gate/go-sse.json`,
`ex-sse.json`):

| 메트릭 | Go | Elixir | 비율 | 한도 |
| --- | --- | --- | --- | --- |
| TTFT p50 | 14.5 ms | 25.8 ms | 1.77× | ≤1.10 |
| TTFT p99 | 21.1 ms | 29.3 ms | 1.39× | ≤1.10 |
| RSS/stream | 248 KiB | 583 KiB | 2.35× | ≤1.20 |
| errors / disconnect_cleanup | 0 / true | 0 / true | — | pass |

이미 시도(효과 없음): Mint `:passive`, `nodelay`, acceptor 증설, Mix
prod release.

**추론.** HTTP/1.1에서 끝나지 않는 SSE는 연결 슬롯 하나를 점유한다.
idle socket이 0인 cold 256-wave에서 generic keep-alive/Finch pool은
256 dial을 제거하지 못한다. 상세: [p1-3-hex-decision.md](p1-3-hex-decision.md).

**가설 (Linux stage map 전 채택 금지).**

- H-1: 매 요청 `Mint.connect`가 TTFT의 큰 비중
- H-2: Jason reference string / request body sub-binary가 10초 state에
  남아 RSS/stream을 키움
- H-3: Bandit/Plug chunk 또는 mock first-data timing

권고된 다음 코드 단계(아직 **미구현**, Manus 회신 대기): Mint 1.10.1
pin + 요청당 1회 Telemetry stage marker. 풀/Finch/Cowboy/Gun은 분기
POC만, H-1이 입증된 뒤.

## 4. 컷오버 제약 (사실)

- 이미지 이름 분리. Go chart를 Elixir 이미지로 upgrade 금지.
- 같은 프로세스 커맨드에서 Go+Elixir가 공유 PG/Redis에 쓰지 않음.
  LB canary **이후**에는 요청당 writer가 하나면 같은 DSN 허용
  (`CANARY.md`).
- Mix는 CH를 `NEXUS_CLICKHOUSE_HTTP_URL`로 읽음. Go native
  `NEXUS_CLICKHOUSE_URL`과 키가 다름.
- Python sidecar 제거가 P6 목표. heuristic ID는 `nexus:*:v2` 유지,
  로컬 메트릭 이름은 `heuristic_pii` 등 Go와 맞춤 (`Nexus.Eval.ID`).

## 5. Manus에게 묻는 것

1. **빠진 Go 표면.** 매트릭스에 cron, evalbatch, invite, evidence,
   pricing drift, Metabase bootstrap, IP limiter, semantic cache **요청
   경로**, live WS hub, Cursor rewrite, Grid 307이 얼마나 컷오버
   차단인지. 추가할 행이 있으면 지적해 달라.
2. **다음 작업 순서.** P1-3 Day 1 계측(Mint pin + stage map)이 맞는지,
   아니면 Grid / MCP manager 연결 / Rank CH / semcache hot path가
   프로덕션 컷오버를 더 막는지.
3. **MCP.** Elixir `mcp_manager` nil → 503이 Go “MCP not configured”와
   동등한지. 콘솔 Memory 레지스트리를 gateway에 붙여야 하는지.
4. **Heuristic 계약.** `contains`/`pii`/`exact_match`/`rouge_l`과
   `nexus:*:v2` ID를 Go와 바이트 단위로 감사하는 방법. Python 3
   메트릭 거절이 오라클과 의도적으로 다른 점인지 확인.
5. **Installer.** `--local` / npx를 Elixir에서 재현할지, 컷오버 후에도
   Go installer가 남는지.
6. **P1-3.** Darwin 숫자를 방향성으로만 쓰는 Hex 결정에 동의하는지.
   stage marker 동기 handler가 p99를 오염시키지 않게 할 때 기본
   attach를 off로 두는 설계가 맞는지.

## 6. 의도적으로 이 문서에 없는 것

Runbook, Helm values 전체, 고객 보안 리뷰 문장 — 각각
[development.md](development.md), [kubernetes.md](kubernetes.md),
[customer-self-hosted-security.md](customer-self-hosted-security.md).
Day 1 코드 패치는 이 브리핑 회신 전에는 넣지 않는다.

## 7. Manus 회신 (2026-09-22) — 실행 판정

전면 컷오버는 **승인되지 않았다.** 게이트가 끝날 때까지 Go
(`ghcr.io/fun-fx/ffx_nexus`)가 프로덕션 서빙·계약 오라클·롤백이다.
Elixir는 `ghcr.io/fun-fx/nexus_ex` + 별도 Helm. dual-write / serial
proxy 금지.

첫 스프린트는 기능을 넓히지 않는다: S1-01–S1-12 (Mint ≥1.10.1, route
manifest, differential, RawSSE markers/finalizer, limiter 모드, boot
refuse, Linux 측정 러너, CI). 작업 토폴로지는 Z0 soak.

열린 제품 질문(프로필 S1/E2, canary 라우트, BYOK fallback, Redis 장애
코드, Python 3종, MCP/Prime, invite, SSO, installer, CNI, audit 오류,
trailer, canary error budget)은 코딩으로 닫지 않았다.

## 8. S2-01 — redirect policy (PR #328)

Manus 비목표에 있던 redirect leak을 차단.
`stripAuthorizationOnCrossOriginRedirect`는 이제 cross-origin hop에서

- `Authorization`, `x-api-key`, **`Cookie`, `Proxy-Authorization`** 삭제
- `Host = ""` (이전 오리진으로의 SNI/Host 헤더 binding 차단)
- 10 hop 초과시 `http.ErrUseLastResponse`로 종료 (loop 차단)

[docs/upstream-redirect-policy.md](upstream-redirect-policy.md) 이유.
뉴트로런은 자격증명/allowed origin inventory를 그대로 두고 redirect
자체를 강화하는 쪽으로 갔다. outbound SSRF 자체는 다음 라운드
(egress.Tenant grpc/raw TCP dialer)로 보류.

