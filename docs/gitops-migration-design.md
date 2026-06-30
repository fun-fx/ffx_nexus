# Nexus CD → GitOps 전환 설계

> 목표: `git push origin main` 한 번으로 프로덕션에 자동 배포되는 파이프라인을,
> 클러스터 네이티브 방식(FluxCD GitOps)으로 단순·견고하게 만든다.

## 1. 배경 — 지금 구조와 통증

현재 `main` push CD는 `.github/workflows/cd-prod.yml` 한 파일이 전부를 한다:

1. ARC self-hosted 러너가 checkout
2. kaniko Job을 **클러스터 안**에 던져 이미지 빌드 → Harbor push (`main-latest` floating tag)
3. 러너가 `helm upgrade` + `rollout restart` + smoke

겪은 문제(시간순):

| 단계 | 증상 | 원인 |
|------|------|------|
| 빌드 | `Repository not found` | kaniko pod에 git 자격 미전달 |
| 빌드 | 10분 멈춤 | `kubectl wait Complete`가 Failed 미감지 |
| 빌드 | `Invalid username or token` | `github.actor`+app-token 거부 (→ `x-access-token`으로 해결) |
| push | `UNAUTHORIZED` | harbor secret이 LAN 호스트(nip.io)로 키잉, push는 ts.net |
| 빌드 | **`Invalid username or token` (CI에서만)** | **`secrets.GITHUB_TOKEN`이 외부(kaniko) clone에 거부됨** ← 현재 막힌 곳 |

**핵심 통찰**: 막힌 지점들은 대부분 "kaniko가 GitHub에서 소스를 **다시 clone**한다"는 설계 선택에서 파생됐다.
GitOps 전환은 이 중 **배포(롤아웃) 부분**을 클러스터 네이티브로 깔끔하게 만든다.
단, **이미지 빌드 시 private repo 접근**은 GitOps와 별개 문제로 남는다(아래 6장).

## 2. 환경 사실 (조사 결과)

- 클러스터 = **Cozystack**, 플랫폼 전체가 이미 **FluxCD HelmRelease로 관리**됨
  (`cozy-fluxcd` 네임스페이스, 전 컴포넌트가 `helmrelease` CR).
- Flux 컨트롤러(현재 가동 중): `source-controller`, `kustomize-controller`,
  `helm-controller`, `notification-controller`, `source-watcher`.
- **미설치**: `image-reflector-controller`, `image-automation-controller`
  → "레지스트리 새 태그 자동 감지·커밋"은 추가 설치가 필요.
- 관련 CRD는 이미 존재: `gitrepositories`, `helmreleases`, `helmrepositories`,
  `imagepolicies`, `imagerepositories` (Flux가 설치돼 있어 CRD는 깔림).
- nexus만 GitOps 밖에서 `helm upgrade`(cd-prod.yml)로 별도 관리 중.
- Harbor가 두 주소로 노출: LAN `harbor.<node-ip>.nip.io` / Tailscale
  `harbor.<tailnet>.ts.net`. 클러스터는 ts.net으로 pull.
- helm chart: `deploy/helm/nexus/` (Chart `0.3.3`, values.yaml 존재).
- prod values: `deploy/cozystack/values-prod.yaml`
  (`image.repository=harbor.<tailnet>.ts.net/ffx/nexus`, `tag=main-latest`, `pullPolicy=Always`).

## 3. 목표 상태 (공통)

- **배포 선언은 Git에**: nexus도 다른 컴포넌트처럼 Flux가 관리.
- **`git push origin main` = 자동 배포** 유지.
- CI는 "빌드 + 레지스트리 push"만, "롤아웃"은 Flux가.
- kubeconfig를 CI에 노출하는 의존을 줄이거나 제거.

## 4. 옵션 비교

### 옵션 A — Flux HelmRelease + Actions가 빌드/태그커밋 (권장)

```
push main ─▶ GH Actions ─▶ build+push 이미지(tag=git SHA) ─▶ Git에 HelmRelease values 태그 커밋
                                                                      │
                                                          Flux helm-controller가 감지
                                                                      ▼
                                                            자동 helm 롤아웃
```

- nexus를 `HelmRelease` CR로 만들어 `cozy-fluxcd`(또는 tenant-nexus)에 둠. 기존 chart 재사용.
- 이미지 태그를 **불변(git SHA / 시맨버)** 으로. floating `main-latest` 폐기 → 캐시/롤아웃 모호함 제거.
- Actions가 빌드 후 그 태그를 Git의 values(또는 HelmRelease)에 커밋 → Flux가 자동 적용.
- **장점**: 플랫폼과 동일 방식, 배포 이력이 Git에 남음, kubeconfig CI 노출 불필요(롤아웃은 Flux),
  추가 컨트롤러 설치 없음.
- **단점**: Actions가 Git에 되커밋하는 스텝 1개 추가(봇 토큰 또는 deploy key 필요).

### 옵션 B — Flux Image Automation까지 (풀 자동화)

```
push main ─▶ GH Actions ─▶ build+push(tag=SHA) ─▶ Harbor
                                                     │ (감시)
                              image-reflector-controller
                                                     ▼
                              image-automation-controller ─▶ Git 자동 커밋 ─▶ helm 롤아웃
```

- 옵션 A + `image-reflector` / `image-automation` 컨트롤러 설치.
- Flux가 Harbor를 직접 polling → 새 태그 자동으로 Git 반영.
- **장점**: Actions가 Git을 건드릴 필요 없음. 가장 "무인" 자동화.
- **단점**: 컨트롤러 2개 추가 설치(Cozystack 관리 영역). Harbor 인증을 Flux에 설정. 복잡도↑.

## 5. 이미지 빌드 방식 (옵션 A/B 공통 결정 항목)

빌드는 GitOps와 무관하게 여전히 필요. 세 갈래:

1. **클라우드 러너 + buildx** (가장 단순): `runs-on: ubuntu-latest`, `docker buildx`로
   빌드 → Harbor push. 소스는 러너가 이미 checkout했으므로 **git clone 인증 문제 없음**.
   단 Harbor가 외부에서 접근 가능해야 함(현재 Tailscale 내부 → Tailscale Action 필요할 수 있음).
2. **self-hosted 러너 + buildx**: 러너가 클러스터 안이라 Harbor 접근 OK. 러너 이미지에
   buildx 필요(현재 없음 → DinD/buildkit 셋업 필요).
3. **kaniko 유지**(현 방식): git clone 인증(PAT/deploy key) 필요 → 지금 통증의 근원.

→ GitOps로 가면 **#1 또는 #2가 자연스럽다**(빌드 컨텍스트를 러너 로컬 소스로 주면 clone 불필요).

## 6. private repo 접근 정리 (중요)

- GitOps는 "롤아웃"을 풀(pull) 방식으로 바꿔 **kubeconfig CI 노출**을 없앤다.
- 하지만 "빌드 시 소스 접근"은 별개:
  - kaniko 유지 시 → PAT/deploy key 필요 (현재 막힌 문제 그대로).
  - 러너 로컬 빌드(#1/#2) 시 → `actions/checkout`이 이미 받은 소스 사용 → **추가 인증 불필요**.
- Flux가 Git repo(설정)를 읽을 때도 private면 자격 필요하나, 이는 Harbor가 아니라
  소스 repo 한정이며 deploy key 1회 설정으로 끝.

## 7. 권장안

**단계적 접근**:

1. **(지금) PAT로 현 파이프라인을 일단 green 처리** — 동작하는 기준선 확보.
   (`GIT_CLONE_TOKEN` fine-grained PAT, contents:read, ffx_nexus 한정)
2. **(다음) 옵션 A로 전환** — 빌드를 러너 로컬(buildx)로, 배포를 Flux HelmRelease로.
   이때 kaniko git clone과 PAT 의존이 사라지고, kubeconfig CI 노출도 제거.
3. **(선택) 옵션 B** — 완전 무인 자동화가 필요해지면 image-automation 추가.

## 8. 작업 항목 (옵션 A 기준, 2단계)

- [ ] nexus `HelmRelease` CR 작성 (`deploy/flux/nexus-helmrelease.yaml` 등), 기존 chart 참조
- [ ] Flux가 chart를 가져올 소스 정의 (`GitRepository` 또는 `HelmRepository`)
- [ ] 이미지 태그 전략 변경: `main-latest` → git SHA (불변 태그)
- [ ] `cd-prod.yml` 재작성:
  - 빌드: buildx로 Harbor push (clone 인증 제거)
  - 배포: helm upgrade 스텝 제거, 대신 Git에 태그 커밋(또는 image-automation에 위임)
- [ ] Flux용 Git 접근(deploy key) + Harbor pull secret 정리
- [ ] smoke는 유지(배포 후 검증) 또는 Flux health check로 대체
- [ ] 문서화: `deploy/manual-deploy.md`, `cd-runner-diagnostic.md` 갱신

## 9. 리스크 / 주의

- Cozystack 관리 영역(`cozy-fluxcd`)에 손대면 플랫폼 업그레이드와 충돌 가능 →
  nexus 전용 리소스는 `tenant-nexus`에 격리 권장.
- 옵션 B의 image-automation은 Cozystack이 관리하지 않는 추가 컴포넌트 → 유지보수 부담.
- 빌드를 클라우드 러너로 옮기면 Tailscale 내부 Harbor 접근 경로 확인 필요.
- 태그를 SHA로 바꾸면 values 되커밋 루프(Actions→Git→Flux)에서 무한 트리거 방지 주의
  (`[skip ci]` 또는 paths-ignore).
```
