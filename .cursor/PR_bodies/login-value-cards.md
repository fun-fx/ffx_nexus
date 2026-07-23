## 요약
`/login` (anon 사용자가 처음 보는 랜딩 페이지) 의 hero 카드 row 를 Overview 페이지와 동일한 **"Why FFX Nexus" 가치 제안** 카드로 교체합니다.

## 배경
PR #119 가 Overview 페이지의 기존 `Best pick / Lowest p95 / Eval window` (사실상 운영 metric 표시) 카드 row 를 "Sense · Govern · Defend" 가치 제안 카드로 바꿨는데, **`/login` 의 카드는 그대로 두었음**. 결과적으로 익명 사용자가 처음 보는 페이지에서는 여전히 `Code · agent / Reasoning / Burst friendly` (모델 메트릭) 가 노출되어, **사용자가 처음 느끼는 첫인상이 '모델 스펙' 이지 '왜 이 게이트웨이가 다른지' 가 되지 않았음**.

## 변경
**`web/src/pages/Login.tsx`** — `auth-tier-row` 의 3개 TierCard 교체:

| 변경 전 | 변경 후 |
|---|---|
| `Code · agent / code-prime / minimax-m3` (workhorse 코드/agent 모델) | **`Sense / Quality-aware auto / auto alias`** — quality-aware auto 라우팅 강조 |
| `Reasoning / text-max / frontier` (심층 추론 모델) | **`Govern / Strict BYOK + audit / 100% your keys`** — BYOK + audit 강조 |
| `Burst friendly / text-standard / <0.04/IU` (price-optimized 모델) | **`Defend / Eval-aware failover / PII + SLM judge`** — 페일오버 강조 |

각 카드 CTA 는 인증된 라우트로 향하므로 익명 사용자가 클릭시 **RequireAuth 가드가 자연스럽게 `/login?next=...` 로 redirect** 한 뒤 로그인하면 원래 라우트로 이동합니다.

## 왜 카드만 바꾸고 전체 레이아웃은 유지하는지
- 디자인 토큰/그리드/CTA 컴포넌트는 Overview 와 동일한 `TierCard` 라서 **시각 일관성 확보**.
- Login 의 hero 카드는 5개 섹션 중 가장 시선이 가는 자리라 안 바뀌면 marketing message 가 깨짐.
- Login 자체의 카피 ( "One gateway. Every model. Unified quality." ) 는 그대로 두고 카드의 **설명** 만 update.

## 검증
- `tsc --noEmit` clean
- vitest 32/32 pass
- 로컬 docker (`nexus:test-ui`) 재빌드 후 헤드리스 Playwright 검증:
  - `/login` 익명 접속 → 페이지에 "Sense", "Quality-aware auto", "Strict BYOK + audit", "Eval-aware failover" 모두 노출
  - 옛 카드 ("Code · agent", "Reasoning", "Burst friendly", "text-max") 모두 제거됨
  - `pageerror` 0건

## Side effect
- 인증된 사용자가 보는 Overview 의 카드와 메시지가 일관됨.
- 익명 → 인증 후 CTA 누르면 자연스럽게 라우팅 페이지로 이동.

✅ Closes: "처음 게정을 로그인하는 페이지에서 카드를 agent 들이 아니라 nexus 의 장점들을 카드로 만들어서 띄우는걸로 바꿔달라" 요청.
