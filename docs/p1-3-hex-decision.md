<!--
category: engineering
title: P1-3 Hex decision
summary: Do not replace Mint/Bandit. Instrument first. Pooling does not fix a cold 256-stream HTTP/1.1 SSE wave.
order: 90
status: draft
-->

# P1-3 Hex 의사결정 (2026-09-22)

Manus 조사 보고서를 프로젝트 결정으로 고정한 문서다.
측정 사실(darwin soak)은 [p1-3-failure-context.md](p1-3-failure-context.md)에
있고, 이 문서는 **라이브러리를 지금 바꾸지 않는 이유**와 **다음 실험 순서**만
남긴다.

Darwin 수치는 방향성 신호다. pass/fail 산식에는 넣지 않는다.
권위 있는 메모리 게이트는 Linux cgroup v2 `memory.current`다.

## 직접 결론

**지금은 HTTP 클라이언트나 서버를 교체하지 않는다.**
`Bandit → Plug → RawSSE → Mint` 투명 바이너리 relay를 유지한다.

| 패키지 | 현재 lock (`nexus_ex/mix.lock`, 2026-09-22) | 결정 |
| --- | --- | --- |
| Mint | **1.10.0** | **1.10.1 이상으로 pin** (보안 수정선). 성능 보장이 아님. |
| Bandit | **1.12.5** | 이미 하한. 유지. |
| Telemetry | **1.4.2** | 유지. stage marker에 사용. |
| Finch / Req / ReverseIt / ReqFuse | 미사용 | hot path에 넣지 않음. |
| Gun | 미사용 | upstream이 HTTP/2 SSE를 **실제로** 줄 때만 별도 연구. |
| NimblePool / Plug.Cowboy / jiffy / Hammer | 미사용 | 가설이 입증된 뒤에만 분기(branch) POC. |

Mint의 요청별 `connect`는 유력한 TTFT 후보이지만, **현재 실패의 주원인이라는
결론은 아직 가설**이다.

## 왜 풀이 cold wave를 고치지 않는가 (사실)

HTTP/1.1 persistent connection은 응답 순서를 보장하고, 재사용 전에 이전 응답을
끝까지 읽어야 한다. 끝나지 않는 SSE는 한 연결 슬롯을 점유한다.

| 상태 | idle socket | 풀이 제거하는 비용 |
| --- | --- | --- |
| Cold first wave (게이트 조건) | 0 | **없음.** 256 stream이면 최대 256 dial. |
| Prewarmed 256 idle | 256 | connect를 TTFT 밖으로 옮길 수 있음. idle RSS·FD 비용. |
| Completed-wave recycle | ≤256 | 첫 wave와 섞어 cold 성능이라고 주장하면 안 됨. |
| Pool capacity &lt; 256 | 부족 | long SSE가 slot을 점유 → checkout queue. |

따라서 “Finch/keep-alive 풀을 넣으면 게이트가 통과한다”는 명제는
**이 workload의 cold first wave에서는 성립하지 않는다.**

## 사실 / 추론 / 가설

**사실**

- Mint는 caller가 socket ownership을 갖는 low-level client이고, 내장 generic
  pool을 제공하지 않는다.
- Finch는 Mint 위에 HTTP/1 pool + checkout queue다. cold 256 + idle 0이면
  결국 256 dial이거나 queue 대기다.
- Req 기본 transport는 Finch다. HTTP/1 `into: :self` streaming은 별도
  consumer process를 만든다.
- GATE.md에 이미 시도된 보강(Mint `:passive`, nodelay, acceptor 증설, Mix
  prod release)은 TTFT를 회복하지 못했다.

**추론**

- 현재 RawSSE가 upstream body를 `Plug.Conn.chunk/2`로 즉시 넘기고 JSON
  재인코드·SSE parse·별도 proxy worker가 없다면, 이것이 가장 짧은 의미
  보존 경로다. 그 위에 pool/parser/queue를 올리는 것은 Linux gate에서
  효과가 입증될 때만 정당하다.
- Thousand Island `num_acceptors`는 **inbound accept**에만 직접 작용한다.
  이미 열린 256 request의 upstream first byte를 해결한다고 가정하면 안 된다.

**가설** (Linux stage map으로 반증/채택)

- **H-1** 매 요청 `Mint.connect`가 cold-wave TTFT의 큰 비중.
- **H-2** Jason `:reference` string 또는 request body sub-binary가 10초
  stream state에 남아 RSS/stream을 키움.
- **H-3** Bandit/Plug outbound chunk 또는 mock first-data timing이 병목.

## 지금 할 일 (패키지 교체 금지)

1. **Mint를 `~> 1.10.1`로 pin.** Bandit은 이미 1.12.5.
2. **얇은 Telemetry stage marker** (요청당 각 1회, handler는 숫자만):
   `handler_enter` → `before_connect` → `connect_ok` → `request_written` →
   `first_upstream_data` → `first_downstream_chunk_return` → `cleanup_done`.
   handler에서 네트워크 I/O, Logger, JSON, body 보관 금지 (Telemetry는
   발생 process에서 동기 실행).
3. **Linux cgroup gate를 권위 있는 score로 고정.** `memory.current`가
   공식 메모리. `smaps_rollup` / `VmRSS` / `:erlang.memory/0`은 원인 분석용.
4. marker-off vs marker-on을 각각 ≥10회 cold 256×10초로 돌려 marker가
   p99/RSS를 오염시키지 않는지 확인한 뒤, 구간별 p50/p99로 H-1/H-2/H-3을
   가르기.

## 가설이 입증된 뒤에만 분기에서 시험

한 번에 한 변수. production lockfile에 넣지 않는다.

| 조건 | 후보 | 통과해도 |
| --- | --- | --- |
| H-1 확인 + pre-connect 운영 허용 | test-only 256 exclusive preconnect, 그다음 scheduler-sharded NimblePool | idle RSS 포함 pass 필수. 전역 GenServer 금지. |
| H-1이 지배적이지 않음 | pool 작업 중지 | — |
| accept latency가 stage map에 보임 | Plug.Cowboy default / `active_n` 대조군 | Mint connect는 그대로. 서버 교체가 아님. |
| `t_decode` 또는 body lifetime이 보임 | Jason `strings: :copy` / 필요한 field만 `:binary.copy/1` | jiffy는 그 다음 POC. |
| 위와 독립 reference | bare Finch HTTP/1 `size: 256, count: 1` | 통과해도 ban을 즉시 풀지 않음. |
| upstream이 H2 SSE를 공식 지원 | Mint H2 / Gun H2 | Finch H2 SSE는 no-back-pressure 경고로 제외. |

Hammer Atomic은 **허용된 stream의 TTFT/RSS를 개선하지 않는다.**
overload admission이 제품 요구일 때만 connect 전 1회, 초과 시 즉시
429/503 (queue 만들지 않음).

## main merge 필수 조건

동일 cold regime, 별도 fresh cgroup/release process:

1. TTFT p50 ≤ Go × 1.10, p99 ≤ Go × 1.10
2. peak `memory.current` delta / active streams ≤ Go × 1.20
3. ≥20회 교차 반복, errors 0, first SSE payload 누락 0
4. cancel 후 30초 내 socket/slot/FD/mailbox가 baseline 회복, 120초에도
   상승 추세 없음
5. status / header / SSE byte order / cancellation / timeout 계약이
   기준 RawSSE와 동등

rollback은 **RawSSE + Mint passive request-per-connect** 기준선으로
즉시 되돌아갈 수 있어야 한다. 이 변경에 DB migration, dual-write,
wire-format 변경을 묶지 않는다.

## 거절 (지금)

Req, ReverseIt, ReverseProxyPlug, ReqFuse, Poolboy, `:httpc`, Hackney를
즉시 대체재로 채택하지 않는다. GenStage/Broadway를 relay에 넣지 않는다.
Fuse/ExRated를 normal stream hot path에 넣지 않는다. Gun을 HTTP/1 기본
대체재로 쓰지 않는다.

## 조사 한계 (이 결정이 보수적인 이유)

- Mint/Finch/Req/Gun/Bandit/Cowboy 어느 쪽에도 **이 조건과 동일한**
  256 concurrent × 10s long-lived HTTP/1 SSE 공개 벤치마크가 없다.
- upstream HTTP/2 SSE 지원은 확인되지 않았다.
- Darwin RSS ≠ Linux `memory.current`.
- merge 직전 `mix hex.info mint` / Bandit advisory를 다시 확인한다.
