# Cloudflare Pages 셋업 가이드 (nexus.ffx.ai)

Status: **DRAFT — for the operator**
Owner: Nexus team
Last updated: 2026-06-22

이 문서는 `nexus.ffx.ai` 마케팅 사이트를 Cloudflare Pages에 배포하기 위한 **one-time 셋업** 절차를 단계별로 정리합니다. 한 번만 수행하면 이후 `main` 브랜치 push마다 자동 배포됩니다.

---

## 개요

| 항목 | 값 |
|---|---|
| Site URL | `https://nexus.ffx.ai` |
| Cloudflare Pages 프로젝트명 | `nexus-marketing` |
| Build command | `npm run build` (in `marketing/`) |
| Build output | `marketing/dist` |
| Production branch | `main` |
| DNS | `nexus.ffx.ai` CNAME → `<project>.pages.dev` (proxied) |
| CI | `.github/workflows/marketing-pages.yml` |

---

## 사전 요구사항

- Cloudflare 계정 (`ffx.ai` 도메인이 등록된 account)
- `fun-fx/ffx_nexus` GitHub repo에 **admin / maintain** 권한
- Cloudflare 계정의 `fun-fx` org 접근 권한

---

## 1단계: Cloudflare API Token 발급

Cloudflare Dashboard → My Profile → API Tokens → Create Token → Custom token.

**권한 (Permissions):**

| Type | Permission | Access |
|---|---|---|
| Account | Account Settings | Read |
| Account | Pages | Edit |
| Zone | Zone | Read |
| Zone | DNS | Edit |

**Account Resources:** `Include → ffx` (또는 해당 account)
**Zone Resources:** `Include → Specific zone → ffx.ai`

> **팁**: "Edit Cloudflare Pages" 템플릿이 거의 맞지만, DNS Edit 권한을 추가해야 custom domain 자동 설정 가능.

생성 후 토큰 값 복사 → 안전한 곳에 보관 (다시 못 봅니다).

---

## 2단계: Cloudflare Account ID 확인

Cloudflare Dashboard → Workers & Pages 우측 하단, 또는 도메인 Overview 페이지에서:
- Account ID: 우측 사이드바 `Account ID` (32자리 hex)
- 예시: `a1b2c3d4e5f6...`

이 값을 GitHub Secret에 등록.

---

## 3단계: GitHub Secrets 등록

GitHub.com → `fun-fx/ffx_nexus` → Settings → Secrets and variables → Actions → New repository secret.

| Name | Value | 비고 |
|---|---|---|
| `CLOUDFLARE_API_TOKEN` | (1단계 토큰) | `Secret` |
| `CLOUDFLARE_ACCOUNT_ID` | (2단계 Account ID) | `Secret` |

설정은 **Repository secret** (not Environment secret)으로. 워크플로우에서 `secrets.CLOUDFLARE_API_TOKEN`으로 접근.

CLI로 등록하려면:
```bash
gh secret set CLOUDFLARE_API_TOKEN -R fun-fx/ffx_nexus
# 프롬프트에 토큰 입력

gh secret set CLOUDFLARE_ACCOUNT_ID -R fun-fx/ffx_nexus
# 프롬프트에 Account ID 입력
```

확인:
```bash
gh secret list -R fun-fx/ffx_nexus
# CLOUDFLARE_API_TOKEN, CLOUDFLARE_ACCOUNT_ID 두 줄이 보여야 함
```

---

## 4단계: Cloudflare Pages 프로젝트 생성

**옵션 A — Dashboard UI (권장)**

1. Cloudflare Dashboard → Workers & Pages → Pages → Create a project
2. "Import an existing Git repository" 또는 "Connect to Git"
3. GitHub 연결 → `fun-fx` org → `ffx_nexus` repo 선택
4. **Project name**: `nexus-marketing`
5. **Production branch**: `main`
6. **Build settings** (Framework preset: `Astro` 자동 인식):
   - Build command: `npm run build`
   - Build output directory: `dist`
   - Root directory: `marketing`
7. "Save and Deploy" 클릭 → **첫 빌드 시도**

**옵션 B — Wrangler CLI**

```bash
# 로컬에 wrangler 설치 (Node 20+)
npm install -g wrangler

# Cloudflare 로그인
wrangler login

# 프로젝트 생성 (GitHub 연결 안 함, dashboard에서 나중에 connect)
wrangler pages project create nexus-marketing --production-branch main
```

**첫 빌드가 성공하면** 임시 URL을 받습니다:
`https://nexus-marketing.pages.dev`

이 URL을 먼저 확인해서 사이트가 잘 뜨는지 검증한 후 custom domain 연결.

---

## 5단계: DNS 설정 (nexus.ffx.ai)

Cloudflare Dashboard → `ffx.ai` 도메인 → DNS → Records → Add record.

| Type | Name | Target | Proxy |
|---|---|---|---|
| CNAME | `nexus` | `nexus-marketing.pages.dev` | Proxied (orange cloud) |

> **Proxied** 권장: Cloudflare CDN + WAF + 무료 TLS 자동 발급.

TTL은 `Auto`. 저장 후 DNS 전파 1~5분.

---

## 6단계: Custom Domain 연결

Cloudflare Dashboard → Workers & Pages → `nexus-marketing` → Custom domains → Set up a custom domain.

- 도메인 입력: `nexus.ffx.ai`
- Cloudflare가 자동으로 CNAME 검증 + TLS 인증서 발급 (1~2분)
- Active 상태가 되면 `https://nexus.ffx.ai`로 접속 가능

---

## 7단계: GitHub Action 워크플로우 트리거

`.github/workflows/marketing-pages.yml`은 이미 main에 머지되어 있음. 다음 조건 중 하나로 트리거:

1. **`main`에 push** (marketing/ 하위 파일 변경 시)
2. **Manual dispatch** (GitHub UI → Actions → marketing-pages → Run workflow)

`main`에 marketing/ 변경사항 push (또는 이미 main에 있다면 workflow_dispatch):

```bash
git checkout main
# 변경 없을 경우 touch로 강제 트리거
touch marketing/.gitkeep
git add marketing/.gitkeep
git commit -m "chore: trigger marketing-pages workflow"
git push origin main
```

또는 GitHub UI에서:
- https://github.com/fun-fx/ffx_nexus/actions/workflows/marketing-pages.yml
- "Run workflow" 버튼 → branch: main → Run

---

## 8단계: 검증

### 8.1 빌드 검증
- GitHub Actions에서 `marketing-pages` 워크플로우가 **success**로 끝나는지 확인
- 실패 시: Actions 탭 → 해당 run 클릭 → 로그 확인

### 8.2 사이트 접속
- `https://nexus-marketing.pages.dev` (임시) → 200 OK
- `https://nexus.ffx.ai` (custom domain) → 200 OK
- 4가지 페이지 모두 확인:
  - `/` (home)
  - `/enterprise`
  - `/pricing`
  - `/docs`

### 8.3 TLS 인증서
- 브라우저 주소창 자물쇠 표시
- 인증서 발급자: `Google Trust Services` 또는 `Let's Encrypt` (Cloudflare Pages 기본)

### 8.4 CDN 캐시 확인
- 첫 요청: TTFB ~200ms
- 후속 요청: Cloudflare edge cache로 ~50ms (HIT)

---

## 트러블슈팅

### 문제 1: `Error: Authentication error [code: 10000]`
- **원인**: `CLOUDFLARE_API_TOKEN`이 잘못되었거나 만료
- **해결**: 1단계에서 토큰 재발급 후 3단계에서 secret 업데이트

### 문제 2: `Error: Cloudflare account ID invalid`
- **원인**: `CLOUDFLARE_ACCOUNT_ID` 오타 또는 잘못된 account
- **해결**: 2단계에서 다시 확인 (32자리 hex)

### 문제 3: 빌드 실패 — `npm ci` 단계에서 lock 파일 mismatch
- **원인**: `marketing/package-lock.json`이 main과 PR 사이에 어긋남
- **해결**: `cd marketing && npm install && git add package-lock.json && git commit`

### 문제 4: 빌드 실패 — `astro check` / TypeScript 오류
- **원인**: marketing/src에 타입 오류
- **해결**: 로컬에서 `cd marketing && npm run check` 후 수정 push

### 문제 5: 빌드는 성공했는데 사이트가 빈 화면
- **원인**: Build output directory가 잘못 설정됨
- **해결**: Pages 프로젝트 설정 → Build output directory = `dist` (Cloudflare UI에서 `marketing/dist` 아님. Root directory가 `marketing`이므로 그 기준)

### 문제 6: Custom domain이 "Pending" 상태로 멈춤
- **원인**: DNS 전파 지연 또는 CNAME target 오타
- **해결**: 5분 대기 후 Cloudflare에서 "Retry" 클릭. 그래도 안 되면 `dig nexus.ffx.ai CNAME`으로 DNS 확인

### 문제 7: `Error 1014: CNAME cross-user banned`
- **원인**: 다른 Cloudflare account의 도메인을 가리키는 CNAME
- **해결**: Cloudflare account에서 `ffx.ai`가 등록된 account의 Pages 프로젝트인지 확인

### 문제 8: GitHub Action에서 `runs-on: ubuntu-latest` 사용 시 네트워크 지연
- **원인**: GitHub-hosted runner → Cloudflare Pages 외부 통신
- **해결**: 정상. Cloudflare Pages 자체 빌드는 Cloudflare 인프라에서 실행. 30초~1분 예상

---

## 운영 노트

### 향후: Preview Deployments (PR별)
- 현재 워크플로우는 `main` push만 production 배포
- PR별 preview URL이 필요하면 `.github/workflows/marketing-pages.yml`에 `wrangler pages deploy --branch=<branch>` 추가
- 단, Cloudflare의 무료 plan은 preview deploy 횟수 제한 있음

### 향후: Analytics
- v1은 analytics 없음 (privacy-first)
- Plausible / Fathom / Cloudflare Web Analytics 추가 고려
- Cookie banner 추가 시 EU 사용자 대응

### 향후: 도메인 추가 (예: `www.nexus.ffx.ai`)
- 같은 Pages 프로젝트의 Custom domains에 추가만 하면 됨
- Cloudflare가 자동 redirect 규칙 생성 옵션 제공

### Secret rotation
- `CLOUDFLARE_API_TOKEN` 90일 rotation 권장
- Cloudflare API Token 페이지에서 만료일 설정 가능

---

## 체크리스트 (사용자 액션)

| # | Task | Status |
|---|---|---|
| 1 | Cloudflare API Token 발급 | ☐ |
| 2 | Cloudflare Account ID 확인 | ☐ |
| 3 | GitHub Secrets에 `CLOUDFLARE_API_TOKEN` 등록 | ☐ |
| 4 | GitHub Secrets에 `CLOUDFLARE_ACCOUNT_ID` 등록 | ☐ |
| 5 | Cloudflare Pages 프로젝트 `nexus-marketing` 생성 | ☐ |
| 6 | 첫 빌드 성공 확인 (`*.pages.dev`) | ☐ |
| 7 | DNS CNAME `nexus.ffx.ai` 추가 | ☐ |
| 8 | Custom domain `nexus.ffx.ai` 연결 | ☐ |
| 9 | GitHub Action 트리거 (push 또는 manual) | ☐ |
| 10 | `https://nexus.ffx.ai`로 4개 페이지 모두 접속 확인 | ☐ |

모든 항목 완료 후 → v1.1 작업 (audit log / onboarding / app.nexus.ffx.ai ingress) 진행.
