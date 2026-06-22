# Cloudflare Pages 셋업 - 빠른 체크리스트

`nexus.ffx.ai` 마케팅 사이트 배포. 자세한 내용은 `cloudflare-pages-setup.md` 참조.

---

## 1. Cloudflare API Token 만들기

**Dashboard → My Profile → API Tokens → Create Token → Custom Token**

권한 (Permissions):

| Type | Permission | Access |
|---|---|---|
| Account | Account Settings | Read |
| Account | Pages | Edit |
| Zone | Zone | Read |
| Zone | DNS | Edit |

**Account Resources:** Include → 본인의 account
**Zone Resources:** Include → Specific zone → `ffx.ai`

생성 → 토큰 값 **복사** (다시 못 봅니다).

---

## 2. Cloudflare Account ID 확인

**Dashboard → Workers & Pages** 우측 하단, 또는 도메인 Overview → Account ID

32자리 hex 값 복사.

---

## 3. GitHub Secrets 등록

**https://github.com/fun-fx/ffx_nexus/settings/secrets/actions → New repository secret**

| Name | Value |
|---|---|
| `CLOUDFLARE_API_TOKEN` | 1단계 토큰 |
| `CLOUDFLARE_ACCOUNT_ID` | 2단계 ID |

CLI:
```bash
gh secret set CLOUDFLARE_API_TOKEN -R fun-fx/ffx_nexus
gh secret set CLOUDFLARE_ACCOUNT_ID -R fun-fx/ffx_nexus
```

확인:
```bash
gh secret list -R fun-fx/ffx_nexus
```

---

## 4. Cloudflare Pages 프로젝트 생성

**Dashboard → Workers & Pages → Pages → Create a project → Connect to Git**

- GitHub → `fun-fx/ffx_nexus` → `main` 브랜치
- **Project name:** `nexus-marketing`
- **Build command:** `npm run build`
- **Build output directory:** `dist`
- **Root directory:** `marketing`

Save and Deploy → 첫 빌드 → 임시 URL: `https://nexus-marketing.pages.dev`

---

## 5. DNS CNAME 추가

**Dashboard → ffx.ai → DNS → Add record**

| Type | Name | Target | Proxy |
|---|---|---|---|
| CNAME | `nexus` | `nexus-marketing.pages.dev` | Proxied |

---

## 6. Custom Domain 연결

**Dashboard → Pages → nexus-marketing → Custom domains → Set up a custom domain**

- 입력: `nexus.ffx.ai`
- 1~2분 대기 → Active 상태

---

## 7. GitHub Action 트리거

워크플로우는 이미 main에 머지됨. 다음 중 하나:
- `main`에 marketing/ 변경사항 push
- 또는 GitHub UI → Actions → marketing-pages → Run workflow

```bash
# 강제 트리거
git checkout main
touch marketing/.gitkeep
git add marketing/.gitkeep
git commit -m "chore: trigger marketing-pages workflow"
git push origin main
```

---

## 8. 확인

- [ ] `https://nexus-marketing.pages.dev` 접속 OK
- [ ] `https://nexus.ffx.ai` 접속 OK
- [ ] TLS 자물쇠 표시
- [ ] `/`, `/enterprise`, `/pricing`, `/docs` 4개 페이지 모두 정상
- [ ] GitHub Actions `marketing-pages` 워크플로우 success

---

## 자주 발생하는 문제

| 증상 | 해결 |
|---|---|
| `Error: Authentication error [code: 10000]` | Token 재발급 → Secret 업데이트 |
| `Error: Cloudflare account ID invalid` | Account ID 다시 확인 (32자리 hex) |
| 빌드 실패: `npm ci` lock mismatch | `cd marketing && npm install` 후 `package-lock.json` commit |
| 사이트가 빈 화면 | Build output directory = `dist` (Root = `marketing`이므로) |
| Custom domain "Pending" | DNS 전파 대기 5분 → Cloudflare에서 "Retry" |

---

전체 8단계 완료 → 1~2시간 소요 예상. 완료되면 `https://nexus.ffx.ai` 라이브.
