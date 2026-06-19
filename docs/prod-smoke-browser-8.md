# Prod smoke — 브라우저 8개 항목 (수동)

대상: `https://console.<tailnet>.ts.net` (Tailscale tailscale 통해 접근)
SSO IdP: `https://keycloak.<tailnet>.ts.net` (Keycloak)

자동 검증(`scripts/test_prod_smoke.sh`)은 health, login, virtual key, guardrails, routing, semantic cache, evals, stats, routing API를 커버합니다. 아래는 **UI 클릭 한 번씩** 필요한 수동 체크리스트입니다.

---

## 1. Console 진입 + SSO 버튼 노출

1. 시크릿 창으로 `https://console.<tailnet>.ts.net` 접속
2. "Sign in with your organization" 패널이 보임
3. 버튼 라벨이 "Sign in with **Keycloak**" 인지 확인

**기대**: Keycloak 버튼 표시. `fetch /api/auth/config` → `sso_enabled: true, sso_label: "Keycloak"` (DevTools network).

## 2. SSO 로그인 흐름 (Keycloak redirect)

1. "Sign in with Keycloak" 클릭
2. Keycloak 로그인 페이지로 302 redirect (URL: `https://keycloak.<tailnet>.ts.net/realms/.../protocol/openid-connect/auth?...`)
3. Keycloak에서 admin@nexus.local 과 동일한 email 로 로그인 (또는 새 email)
4. Nexus 콘솔로 다시 redirect

**기대**: 콘솔이 `/` 또는 `/account` 로 돌아오고 우상단에 user email 표시. 첫 로그인 시 자동 회원가입(JIT) + BYOK 페이지로 진입.

## 3. JIT 신규 사용자 자동 생성

1. Keycloak에 **새 email 계정** 생성 (예: `sso-test-1@nexus.local`)
2. 해당 계정으로 SSO 로그인
3. 콘솔에서 "Signed in as `sso-test-1@nexus.local`" 확인
4. 우상단 role tag가 `member` 인지 확인

**기대**: Nexus DB에 새 user row 자동 생성. admin console의 Users 목록에 새 email 등장 (`/api/users`).

## 4. Email linking (같은 email로 두 가지 방법 동시 사용)

1. 같은 email 로 SSO 로그인
2. Sign out
3. 일반 로그인 form 에 같은 email + 설정한 password 로 로그인

**기대**: 같은 user row 로 인식. SSO로 만들었던 user 와 password login user 가 통합 (별도 row가 생기지 않음). DB `users.email` 인덱스 확인.

## 5. SSO 사용자 logout → SSO 로그인 흐름

1. SSO 로그인한 상태에서 우상단 "Sign out" 클릭
2. `/api/auth/logout` 호출 → Keycloak RP-Initiated Logout URL 로 redirect
3. Keycloak 세션 종료 페이지 → "Sign in" 링크 노출
4. 다시 Keycloak 로그인 시, Keycloak SSO 세션이 살아있으면 1-step 로그인 (아이디/비번 입력 skip)

**기대**: Keycloak SSO 세션 쿠키(`KEYCLOAK_*`)가 정상 발급·삭제. 브라우저 devtools → Application → Cookies 에서 `nexus_sso_state`, `KEYCLOAK_SESSION`, `KEYCLOAK_REMEMBER_ME` 흐름 관찰.

## 6. SSO 사용자도 virtual key 발급 가능

1. SSO 로그인한 user (예: admin@nexus.local) 로 콘솔 진입
2. Account 패널 → "Create virtual key" 로 새 key 발급
3. 발급된 `nxs_live_...` 키 복사

**기대**: virtual key 생성 정상. SSO 사용자도 동일하게 virtual key 발급 가능 (소스 코드 `internal/console/auth.go` 권한 검사 우회 아님).

## 7. SSO 사용자의 virtual key 로 chat 호출 (BYOK 등록 후)

1. SSO 사용자 (예: admin@nexus.local) Account → Credentials → Gemini API key 등록
2. 새 virtual key 발급 (또는 기존 키 사용)
3. `https://nexus.<tailnet>.ts.net/v1/chat/completions` 에 bearer token 으로 호출

**기대**: 200 + 실제 LLM 응답. SSO user 도 password user 와 동일하게 BYOK 기반 chat 동작.

## 8. SSO 비활성 환경 변수 토글 (재배포 없이 검증)

1. `kubectl -n tenant-nexus set env deploy/nexus NEXUS_SSO_ENABLED=false` (또는 env name 에 따라 unset)
2. `kubectl rollout restart deploy/nexus`
3. Pod 재시작 후 `curl /api/auth/config` → `sso_enabled: false`
4. 브라우저 새로고침 → "Sign in with Keycloak" 패널 사라짐

**기대**: env 토글로 SSO 기능 활성/비활성 즉시 반영. Keycloak client / DB 변경 없이 env 한 줄로 동작 (rollback 안전).

---

## 통과 기준

- [ ] 1. SSO 버튼 노출
- [ ] 2. SSO redirect → callback → 콘솔 진입
- [ ] 3. JIT 신규 user 자동 생성
- [ ] 4. 같은 email 의 SSO/password 통합
- [ ] 5. SSO logout → Keycloak 세션 정리 → 재로그인
- [ ] 6. SSO user 도 virtual key 발급
- [ ] 7. SSO user 도 BYOK + chat 정상
- [ ] 8. env 토글로 SSO on/off

각 항목에서 "기대"와 다른 결과가 나오면 Step 1 SSO 등록 (env + client) 으로 rollback 검토.
