# P1-3 게이트 — 실패 사실 기록 (Mick 2026-09-22 정리)

이 문서는 **`nexus_ex`의 P1-3 성능 게이트가 왜 fail인지 객관화된 자료**다.
terminal에서 직접 측정한 수치만 적었으며, 추측/희망은 [Hypothesis] 섹션에
분리해 두었다.

Hex 라이브러리 결정은 후속 조사(Manus, 2026-09-22)를
[p1-3-hex-decision.md](p1-3-hex-decision.md)에 고정했다.
요지: **Mint/Bandit을 지금 교체하지 않는다.** Darwin 수치는 방향성 신호일
뿐 pass/fail 산식에 넣지 않는다. 다음 작업은 stage timestamp이지 풀 도입이
아니다.

## 1. 게이트가 무엇을 측정하는가

- 스크립트: `nexus_ex/scripts/run_linux_soak.sh` → `ffx_nexus/scripts/run_p1_gate.sh`
- 기준 (비-열악, hard gate):
  - `ttft p50`: Elixir ≤ Go × **1.10**
  - `ttft p99`: Elixir ≤ Go × **1.10**
  - `rss/stream`: Elixir ≤ Go × **1.20**
  - `errors == 0`
  - `disconnect_cleanup_ok == true`
- `run_p1_gate.sh`는 동일 mock SSE upstream을 두고, 같은 옵션으로 두 게이트웨이를 직렬로 부팅하고 `ffx_nexus/scripts/load_baseline/main.go`로 부하를 인가, server PID의 RSS를 샘플링한다.
- 인자 (2026-09-18 측정 시점):
  - `-streams 256`, `-duration 10s`, `-model gpt-4o-mini`
  - `-mock-listen 127.0.0.1:19090` (게이트가 mock upstream을 자체 띄움)
  - `-base-url http://127.0.0.1:18080` (게이트웨이: Go, 그 뒤 Elixir)
  - 환경: `NEXUS_KEY_MODE=shared`, `NEXUS_ALLOW_SHARED_KEYS=true`, `NEXUS_DEV_MODE=true`, `OPENAI_API_KEY=sk-gate`, `OPENAI_BASE_URL=http://127.0.0.1:19090/v1`. 모두 `NEXUS_POSTGRES_URL` / `NEXUS_CLICKHOUSE_URL` / `NEXUS_REDIS_URL` unset → zero-dep 비교.

## 2. 직접 측정 결과

### 2.1 darwin soak (현재 보고된 last-known)

- 파일: `priv/gate/go-sse.json` (ts 2026-09-18T11:16:57Z), `priv/gate/ex-sse.json` (ts 2026-09-18T11:17:10Z)
- `goos: darwin`, `goarch: arm64`, `cgroup: false`, host 동일.
- 핵심 메트릭 (ns → ms는 단위 변환한 표):

| 메트릭 | Go | Elixir | 비율 (Ex/Go) | 한도 | 결과 |
|---|---|---|---|---|---|
| TTFT p50 | 14.54 ms | 25.77 ms | **1.77×** | ≤ 1.10 | FAIL |
| TTFT p95 | 20.58 ms | 28.09 ms | — | — | — |
| TTFT p99 | 21.14 ms | 29.26 ms | **1.39×** | ≤ 1.10 | FAIL |
| TTFT max | 21.28 ms | 29.91 ms | — | — | — |
| RSS total | 63.39 MB | 149.18 MB | — | — | — |
| RSS/stream | 247.6 KiB | 582.8 KiB | **2.35×** | ≤ 1.20 | FAIL |
| completed | 256 | 256 | — | — | — |
| errors | 0 | 0 | — | = 0 | PASS |
| disconnect_cleanup_ok | true | true | — | = true | PASS |

### 2.2 왜 Linux 게이트가 별도 필요한가

`nexus_ex/GATE.md`:

> Darwin has no cgroup; official RSS for a kill/keep decision on Linux is `memory.current`. TTFT is comparable across OS as long as both sides ran on the same machine.

- 따라서 **TTFT는 darwin과 Linux가 의미 비교 가능**, RSS는 cgroup RSS로 Linux에서 다시 측정하는 게 정답.
- 그러나 같은 코드 그대로 Linux에서 돌릴 경우 TTFT 비율이 (OS 차이 외에) **darwin과 거의 같은 수준**일 가능성이 매우 크다. 코드를 손대지 않는 한 darwin fail이 그대로 재현될 가능성이 큰 이유:
  - 우리 README의 hot-path bans: 글로벌 GenServer 없음, Finch 금지, JSON re-encode 금지, dual-write 금지, serial proxy 금지.
  - P1-3에서 이미 한 차례 시도한 보강: Mint `:passive`, transport nodelay, Bandit acceptor 늘림, Mix prod release 사용 — **모두 효과 없음** (`GATE.md` 발췌).
  - 그 결과의 폭(1.77×, 2.35×)이 OS·드라이버보다 **per-request hot path의 비용**에서 나오는 신호로 해석됨 (상세 가설은 §5).

### 2.3 기록된 소스 위치

- `nexus_ex/GATE.md` — 정의 + status FAIL + 시도 기록 + 한도 표.
- `nexus_ex/priv/gate/go-sse.json`, `nexus_ex/priv/gate/ex-sse.json` — 실제 보고서. 위 표는 이 두 파일에서 추출.
- `ffx_nexus/scripts/run_p1_gate.sh` — 게이트 스크립트.
- `nexus_ex/scripts/run_linux_soak.sh` — wrapper, Darwin 거부.
- `ffx_nexus/scripts/load_baseline/main.go` — 부하 도구. `-pid`로 server RSS sample.

## 3. 우리가 게이트 무관으로 OK인 부분 (자료조사 AI에게는 "fail은 어디인가" 질문과 함께 던져 주세요)

전체 작업은 게이트와 무관하게 여러 PR이 머지되어 있다. **fail은 hot path**에 제한되어 있고, 다른 영역(control plane / 콘솔 / audit / Eval / MCP 라이브러리)은 별개일 수 있다.

- 콘솔 / LiveView 셸: main에 5 PR 누적 (`#1` UI port + MCP, `#2` Benchmarks, `#3` Eval routing, `#4` Connectors, `#5` Placeholder 정리). 76 console unit tests + LiveView 렌더 green (`mix test 219 tests, 0 failures`).
- 부트 가드 (`Nexus.Release.Canary.assert_boot!/1`): serial proxy / dual-write 거부. 13 tests green.
- Redis limiter, ClickHouse RowBinary/JSONEachRow, AES-GCM, OIDC, audit fail-stop 등: 단위 테스트 green.
- ClickHouse-backed observability surfaces (Traces / Spend / Dashboard / MCP Logs): `disabled` 모드에선 `"ClickHouse not wired"` 배너 노출. CH 없으면 정상 fail-soft.
- Eval plugin dispatcher: heuristic + external 두 모드 모두 해석. 51 unit tests green.
- 데이터 평면 백엔드 일부는 stub 또는 missing: `vendor_note = "Prime Hub adapter is not bound in this build (Phase 4)"` 같은 명시. **hot path와 독립**.

자료조사 요청 시 위 영역들은 fail 게이트와 결부하지 말 것.

## 4. 어디를 손대야 하는 후보 (실제 코드 위치)

핫 패스 (`POST /v1/chat/completions` stream, mock upstream으로 SSE 인) 진입 경로:

1. `apps/nexus_release/lib/nexus/release/application.ex`
   ```elixir
   {Bandit,
    plug: Nexus.Gateway.Router,
    scheme: :http,
    port: gateway_port,
    thousand_island_options: [num_acceptors: 64]}
   ```
   - 수용 acceptor는 이미 64 (기본 8 vs 64 비교는 GATE.md에 시간·메모리 데이터 없음).

2. `apps/nexus_gateway/lib/nexus/gateway/router.ex`
   - `RequestID`, `ConsoleCORS`, `match`, `dispatch` 4-plug chain.
   - `POST /v1/chat/completions` → `Nexus.Gateway.Auth.call/2` → `Nexus.Gateway.Chat.call/1`.

3. `apps/nexus_gateway/lib/nexus/gateway/chat.ex`
   - `read_body(length: 1_048_576)` → `Jason.decode(body)` → `Guardrails.prompt_text/1` → `Pipeline.prepare/3` → `RawSSE.proxy/2` 또는 `unary_with_cost`.

4. `apps/nexus_gateway/lib/nexus/gateway/raw_sse.ex`
   ```elixir
   opts = [
     mode: :passive,
     protocols: [:http1],
     transport_opts: [timeout: 5_000, nodelay: true]
   ]
   Mint.HTTP.connect(scheme, uri.host, port, opts)
   ```
   - **매 요청마다 `Mint.HTTP.connect` 호출** — 동일 host로의 keep-alive 풀이 없음. 256 동시 요청이면 256번 connect.
   - `Mint.HTTP.request` → `Mint.HTTP.recv(mint, 0, @idle_timeout)`. `@idle_timeout = 30_000`.
   - 응답 후 `pump_copy(conn, mint, ref)` → `Mint.HTTP.recv` → `handle_copy` → `chunk(conn, data)` → 응답 시 `send_chunked(200)` + chunk 송신.

5. `apps/nexus_gateway/lib/nexus/gateway/pipeline.ex`
   - `Application.get_env(:nexus_gateway, :route_groups, %{})`. 매 요청 lookup이라 256 동시에서 누적. ETS 캐시나 컴파일 타임 모듈 attr로 옮길 여지.

6. `apps/nexus_gateway/lib/nexus/gateway/auth.ex` — 가벼움.

## 5. Hypothesis (자료조사에 던질 때 명확히 "가설"이라고 분류)

- [H-1] **Mint connect 비용**: 매 요청 새 connect → syscall 폭증 → TTFT 폭의 큰 부분. 풀이 있으면 바로 0.4 ms 단위로 줄어들 추정.
- [H-2] **Bandit chunk path**: `chunk(conn, data)` 호출마다 NIF/encoding 비용. SSE가 짧은 chunk이면 hot path 분포 늘어남. Bandit 내부 옵션이나 chunk-packaging 변화에 의존.
- [H-3] **mock upstream 첫 chunk 지연**: mock 자체 첫 chunk 지연이 1 ms라도 있으면 그대로 우리 측 TTFT에 반영. 측정한 적 없음. 좋은 검증: `mockSSE` 첫 응답이 첫 chunk를 곧바로 보내는지.
- [H-4] **Mint `:passive` 모드 적용 후 recv path**: 0-byte recv → 즉시 응답 반환 패턴. 첫 chunk 도착 후 chunk send 사이 경합. 효과는 작을 수 있음.
- [H-5] **plug pipeline 추가 비용**: `match/dispatch` 외에 4-plug (RequestID, ConsoleCORS, ...) → RequestId UUID 생성, header parse.
- [H-6] **ETS/모듈 attr 캐시 누락**: 라우팅 그룹 lookup 등이 매 요청.

## 6. 자료조사에 던질 때 권장 질문

- darwin 측정에서 TTFT p50 1.77× 폭은 **Mint HTTP connect를 풀링하지 않는 데서** 오는 게 가장 큰 설명력이 있는지.
- Go의 net/http keep-alive 풀이 정확히 동등한 효과를 주는지, 우리 측에서 `Mint.HTTP` + `:keepalive` (per private slot) 풀 구현의 비용은 얼마나 되는지.
- `:keepalive`를 `:passive` 모드와 결합한 경험 / `Mint.HTTP.put_private_pid` 등 캐싱 패턴이 Mint에 있는지.
- Bandit chunk path에서 매 `chunk(conn, data)` 호출 비용을 batch로 줄일 수 있는 옵션이 있는지 (`sendfile`, `chunked_ref` 등).
- 256 동시 stream 시 mock upstream의 응답 log를 확인해 첫 chunk까지 mock이 몇 ms 걸렸는지. mock 응답이 게이트에 어떤 영향을 줬는지.
- 같은 코드를 Linux + cgroup v2 호스트에서 다시 돌릴 때 TTFT 변화 폭 예상 (OS 의존이 아님을 검증).
- 보조로: 우리 측의 `Pipeline.prepare` lookup을 `:persistent_term` 또는 모듈 attribute로 옮길 때의 코드 변경 사이즈와 동시성 안전성.

## 7. 결론 (현재 알고 있는 사실만)

- 게이트는 **darwin에서 fail** 상태가 명시적으로 기록되어 있음.
- 1.77× / 2.35× 폭은 darwin 측정값으로 **이미 비-열악 게이트 한도를 초과**.
- 같은 코드로 Linux에서 재실행해도 TTFT는 거의 같은 폭일 가능성이 매우 큼 (download 비율 측정값이 OS 차이보다 코드 차이에서 옴).
- 이미 시도한 보강책(Mint `:passive`, nodelay, acceptor 증설, prod release)은 한계.
- 후보 조치는 §4 (코드 위치) + §5 (가설).
