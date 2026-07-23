## 요약
PR #120 이 시크릿 브라우저 SPA 크래시(= 검은 화면)의 임박 원인을 잡았지만, 동일한 root cause(401 응답 본문을 강한 타입으로 잘못 사용)가 다른 endpoint에서 재현될 가능성이 있어 전방위로 defense-in-depth 보강 + 세션 만료 실시간 감지를 추가.

## 변경

### Hardening

* **`web/src/api.ts` — `fetchEvalConfig` sanitizer 추가**
  - 401 / 부실 body에 대해 `ZERO_EVAL` 스냅샷을 반환 (이전엔 throw → Overview/Routing 페이지에서 `evalCfg?.routing.weights.quality` 옵셔널 체인만으로 완화했으나, `fetchEvalConfig` 자체가 throw되면 `Promise.allSettled`로 흡수되긴 함 — 그래도 sanitizer로 정확히 동일한 패턴 유지)
  - `patchEvalConfig` 도 sanitizer 통과 (관리자 페이지에서 PATCH 응답이 부실할 가능성 차단)
  - Optional field 모두 강타입으로 보정: 숫자는 `NaN/null/undefined → 0`, boolean은 `→ false`, string은 `→ ""`, array는 `→ []`

* **`web/src/components/RequireAuth.tsx` — 세션 만료 sweeper**
  - `refetchInterval: 60_000`로 `/api/me` 백그라운드 재검증
  - 응답에서 `isError`가 뜨면 `qc.clear()` + 현재 경로 보존하면서 `/login?next=...` 로 replace navigate
  - 사용자가 다른 탭에서 강제 logout 됐거나 server 가 인증을 회전해도 1분 이내 자동 화면 복귀

### Tests

* **`web/src/api.sanitize.test.ts`** (신규) — fetchStats / fetchEvalConfig sanitizer 단위 테스트
  - 401 응답이 zero-shape 로 안전히 변환되는지
  - 이상한 body (`NaN`, `undefined`) 도 안전한 default 로 fall-back 되는지

* **`web/src/components/RequireAuth.test.tsx`** (신규) — guard 동작 3 케이스
  - `/api/me` resolve 중에는 loading card 가 마운트
  - 401 응답이면 `/login` 으로 bounce
  - 정상 user 응답이면 `<Outlet>` 자식(Overview)이 렌더되며 "Why FFX Nexus" 섹션까지 정상 표시 (sanitizer 와 end-to-end 호환 확인)

## 검증

* `tsc --noEmit` clean
* vitest 32/32 pass (이전 25 → 32, +7 신규)
* Docker `nexus:test-ui` (`linux/amd64`) 재빌드 후 헤드리스 Playwright 시나리오:
  - `/traces` 익명 → `/login?next=/traces`, body=Login page, pageerror=0
  - `/` 익명 → `/login?next=/`, body=Login page, pageerror=0
  - 키 구성 + auth 시 success 케이스 별도 수동 확인

## Side effect

* 기존 Auth 사용자 환경 변화 없음 (TierCard, Why-Cards 렌더링 동일)
* CSR 캐시 (`gcTime: 5min`) 가 남아 있다가 분기 시 즉시 무효화됨

✅ Closes: "추가 보강 검토 + 운영 배포" 요청.
