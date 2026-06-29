# 수동 배포 가이드 (Manual deploy)

Status: **BACKUP** (CD가 복구됨 — repo를 PRIVATE으로 전환하여 org self-hosted runner가 잡을 받게 됨. 자세한 경위는 `docs/cd-runner-diagnostic.md` 참고)
Last updated: 2026-06-29

CD가 1주일+ queued였던 근본 원인은 **`ffx_nexus`가 PUBLIC repo였기 때문** — GitHub org는 기본적으로 public repo가 org self-hosted runner를 쓰지 못하게 막는다 (ARC 인프라는 정상이었음). 2026-06-29에 repo를 PRIVATE으로 전환하여 해결. 이 수동 배포 가이드는 이제 CD 장애 시 **백업 절차**로 유지한다.

---

## TL;DR

```bash
# 1. prod kubeconfig 환경 변수 (또는 ~/.zshrc에 export)
export KUBECONFIG=~/kubeconfig/prod.yaml

# 2. (선택) image tag를 새 버전으로 올리고 한 번에 빌드+배포
./scripts/deploy-prod.sh --tag 0.3.6

# 3. 빌드된 이미지가 이미 Harbor에 있으면 tag bump만 다시
./scripts/deploy-prod.sh
```

---

## 사전 요구사항

### 1. 로컬 도구

```bash
brew install kubectl helm
helm version   # v3.16.x
kubectl version --client
```

### 2. Tailscale

- 로컬 Mac에 Tailscale 설치 + `<tailnet>.ts.net` 망에 로그인
- `tailscale status`로 prod node가 보이는지 확인

```bash
tailscale status | grep infrfx
# 100.x.x.x   infrfx-1   ...
```

### 3. prod kubeconfig

prod cluster 관리자로부터 kubeconfig을 받아 `~/kubeconfig/prod.yaml`에 저장.
**만약 이미 `~/talos-cozystack/kubeconfig`이 있다면 그걸 사용해도 됨** (server: `https://<node-ip>:6443`).

테스트:
```bash
KUBECONFIG=~/kubeconfig/prod.yaml kubectl get ns
# 또는
KUBECONFIG=~/talos-cozystack/kubeconfig kubectl get ns
# 둘 중 하나가 동작하면 OK
# tenant-nexus, cozy-* namespace들이 보여야 함
```

**실제 검증 결과 (2026-06-22)**:
- server: `https://<node-ip>:6443` → Tailscale 망에서 접근 가능
- <node-ip> ping: ~117ms RTT (Tailscale MagicDNS routing)
- K8s server version: v1.35.0

### 4. Harbor 자격증명 (선택)

Kaniko 빌드 로그인에 필요. 받으면 `~/.config/nexus/deploy-prod.env`에 저장:
```bash
mkdir -p ~/.config/nexus
cat > ~/.config/nexus/deploy-prod.env <<'EOF'
export HARBOR_USER=robot$ffx-nexus
export HARBOR_PASS=...
EOF
echo 'source ~/.config/nexus/deploy-prod.env' >> ~/.zshrc
```

> kaniko 자체는 in-cluster `harbor` docker-registry secret을 사용하므로, 사용자가 직접 push하지 않습니다. Harbor 자격증명은 dashboard 접근 시에만 필요.

### 5. `harbor` pull secret (one-time)

인-클러스터 Harbor에 pull할 수 있도록 secret이 `tenant-nexus`에 있어야 합니다. 없으면:

```bash
KUBECONFIG=~/kubeconfig/prod.yaml kubectl -n tenant-nexus create secret docker-registry harbor \
  --docker-server=harbor.<tailnet>.ts.net \
  --docker-username='<user>' \
  --docker-password='<pass>' \
  --dry-run=client -o yaml | kubectl apply -f -
```

---

## 사용법

### 기본: 빌드 + 배포

```bash
export KUBECONFIG=~/kubeconfig/prod.yaml

cd ~/ffx_nexus
./scripts/deploy-prod.sh --tag 0.3.6
```

흐름:
1. `values-prod.yaml`의 `image.tag`를 `0.3.6`으로 업데이트
2. 인-클러스터 Kaniko Job apply → main 브랜치에서 `0.3.6` build & push to Harbor
3. Kaniko 완료 대기 (최대 15분)
4. `helm diff upgrade` (변경 사항 미리보기)
5. `helm upgrade --install nexus -f values-prod.yaml`
6. `kubectl rollout status` 대기
7. `/healthz` 두 개 (gateway + console) 확인

### 빠른 재배포 (이미지 그대로)

```bash
./scripts/deploy-prod.sh
# tag 변경 없이 values만 다시 apply → pod 재시작
```

### 강제 재시작 (config 변경 없지만 pod 새로 띄우기)

```bash
./scripts/deploy-prod.sh --restart
# rollout restart
```

### 빌드 건너뛰기 (이미지 Harbor에 이미 push됨)

```bash
./scripts/deploy-prod.sh --skip-build
# helm upgrade + rollout만
```

### Dry run (실제 변경 X)

```bash
./scripts/deploy-prod.sh --tag 0.3.6 --dry-run
# 어떤 명령이 실행될지 출력만
```

### 다른 namespace / 다른 values

```bash
./scripts/deploy-prod.sh --ns tenant-nexus-staging -f deploy/cozystack/values-staging.yaml
```

---

## 사용 시나리오

### 시나리오 1: 일반 코드 변경 (커밋 → main 머지 → 배포)

```bash
git checkout main
git pull --rebase
# 코드 변경, commit
git push origin main

# 새 tag 결정 (보통 appVersion +1)
export KUBECONFIG=~/kubeconfig/prod.yaml
./scripts/deploy-prod.sh --tag 0.3.6
```

### 시나리오 2: values-prod.yaml만 변경 (config-only)

```bash
# values-prod.yaml 수정 후 commit + push
git checkout main
# ... edit deploy/cozystack/values-prod.yaml
git add deploy/cozystack/values-prod.yaml
git commit -m "chore: bump judge sampleRate to 0.2"
git push origin main

# tag 그대로, deploy만
export KUBECONFIG=~/kubeconfig/prod.yaml
./scripts/deploy-prod.sh
```

### 시나리오 3: Chart 변경 (deploy/helm/nexus/*)

```bash
git checkout main
# ... edit deploy/helm/nexus/templates/...
git commit -m "feat(helm): add HPA"
git push origin main

# Chart version bump
# deploy/helm/nexus/Chart.yaml 의 version: 0.3.3 -> 0.3.4

export KUBECONFIG=~/kubeconfig/prod.yaml
./scripts/deploy-prod.sh --tag 0.3.6   # app version (선택)
```

### 시나리오 4: 핫픽스 (긴급)

```bash
git checkout main
git pull --rebase
# 핫픽스 commit
git commit -am "fix: OIDC redirect URI mismatch"
git push origin main

# tag는 그대로 (코드만 바뀌면 보통 같은 tag로 다시 빌드)
export KUBECONFIG=~/kubeconfig/prod.yaml
./scripts/deploy-prod.sh --tag 0.3.5-hotfix1
# 또는 같은 tag로 강제 재빌드:
./scripts/deploy-prod.sh --tag 0.3.5
```

> Tip: 같은 tag로 재빌드하려면 Kaniko는 layer cache로 빨리 끝남. 그래도 새 digest가 생기므로 pod는 재시작됨.

---

## 환경 변수

| 변수 | 기본값 | 설명 |
|---|---|---|
| `KUBECONFIG` | (필수) | prod kubeconfig 경로 |
| `NS` | `tenant-nexus` | target namespace |
| `CHART` | `deploy/helm/nexus` | helm chart 경로 |
| `VALUES` | `deploy/cozystack/values-prod.yaml` | values 파일 |
| `KANIKO_JOB` | `deploy/cozystack/kaniko-build.yaml` | kaniko 매니페스트 |
| `TIMEOUT` | `15m` | helm upgrade timeout (이전 timeout 5m은 부족했음, 15m 권장) |
| `ROLL_TIMEOUT` | `300s` | rollout status 대기 시간 |
| `GW_URL` | `https://nexus.<tailnet>.ts.net` | gateway health URL |
| `CON_URL` | `https://console.<tailnet>.ts.net` | console health URL |

---

## 트러블슈팅

### Q: `Error: cluster unreachable` / `connection refused`

**원인**: Tailscale이 off이거나 kubeconfig이 잘못됨.

**해결**:
```bash
tailscale status
# prod node가 보여야 함
# 안 보이면: tailscale up

# kubeconfig server URL 확인
grep server: ~/kubeconfig/prod.yaml
# https://<node-ip>:6443 또는 Tailscale MagicDNS 이름
```

### Q: `Error: could not find tiller` (helm v2 스타일 메시지)

**원인**: helm v2가 깔려 있음.

**해결**:
```bash
brew uninstall helm
brew install helm
helm version  # v3.16.x 확인
```

### Q: Kaniko Job이 ImagePullBackOff

**원인**: `harbor` docker-registry secret이 없거나 만료.

**해결**:
```bash
KUBECONFIG=~/kubeconfig/prod.yaml kubectl -n tenant-nexus get secret harbor -o yaml
# 없으면 recreate (5단계 pull secret 참조)
```

### Q: Kaniko 빌드는 성공했는데 Helm upgrade에서 `ImagePullBackOff`

**원인 1**: image.tag가 Harbor에 push된 tag와 다름.

**해결**:
```bash
KUBECONFIG=~/kubeconfig/prod.yaml kubectl -n tenant-nexus get pods
# Pod log에서 실제 pull 시도한 image 확인
# values-prod.yaml의 image.tag와 Harbor에 push된 tag가 일치하는지 확인
```

**원인 2**: Talos containerd가 Harbor 자체 서명 cert를 trust 안 함.

**해결**: `values-prod.yaml`의 `image.repository`가 `harbor.<tailnet>.ts.net/ffx/nexus`인지 확인. Tailscale MagicDNS 사용. `<node-ip>.nip.io` 호스트는 kubelet이 못 풂.

### Q: Helm upgrade 성공했는데 rollout이 Pending

**원인**: 새 pod가 scheduling 안 됨 (resource 부족, node affinity 등).

**해결**:
```bash
KUBECONFIG=~/kubeconfig/prod.yaml kubectl -n tenant-nexus describe pod -l app.kubernetes.io/name=nexus
# Events 섹션에서 FailedScheduling 원인 확인
```

### Q: helm upgrade에서 `context deadline exceeded`

**원인**: 기본 5m timeout이 부족. prod 환경에서 revision 19, 20이 이 에러로 failed 상태.

**해결**: 기본 timeout이 15m으로 설정되어 있음. 그래도 부족하면:
```bash
./scripts/deploy-prod.sh --tag 0.3.6 -- --timeout 30m
# 또는
TIMEOUT=30m ./scripts/deploy-prod.sh --tag 0.3.6
```

**만약 helm upgrade가 timeout으로 fail**:
- 새 pod는 떴지만 helm이 old pod 종료 확인 못 한 것
- `kubectl get deploy nexus`로 새 Replicaset 확인
- 새 Replicaset이 정상 동작하면 무시 가능 (helm release만 failed 표시)
- 다음 deploy에서 자동으로 fix됨

**history 정리** (failed revision들):
```bash
KUBECONFIG=~/kubeconfig/prod.yaml helm -n tenant-nexus history nexus
# revision 19, 20이 failed로 보임
# 다음 helm upgrade 시 자동으로 새 revision 생성 (failed는 남음)
```

### Q: Health check 502 / 503

**원인**: Pod는 떴지만 app이 OIDC discovery나 DB 연결에서 멈춤.

**해결**:
```bash
KUBECONFIG=~/kubeconfig/prod.yaml kubectl -n tenant-nexus logs -l app.kubernetes.io/name=nexus --tail=100
# TLS trust, DSN, Keycloak URL 등 확인
```

### Q: helm diff가 비어있는데 pod가 안 뜸

**원인**: Kaniko가 예전 digest를 push했거나 Harbor에 image가 없음.

**해결**:
```bash
# Harbor dashboard에서 image 존재 + tag 확인
# 또는 인-클러스터에서 pull 시도
KUBECONFIG=~/kubeconfig/prod.yaml kubectl -n tenant-nexus run test-pull --rm -it --image=harbor.<tailnet>.ts.net/ffx/nexus:$TAG --restart=Never --command -- sh
```

---

## CI/CD 복구 시 (참고)

ARC runner / GitHub App 권한 문제가 해결되면:

1. `cd-prod.yml`의 `runs-on: self-hosted` 유지
2. PR로 workflow_dispatch가 정상 동작하는지 테스트
3. 정상 동작 확인 후, 이 manual script는 **백업 수단**으로 유지
4. main push → 자동 배포, local deploy는 incident / hotfix 용

### 완전 자동화 복귀 조건

runner pod가 다시 뜨고 ffx_nexus에 등록되면 (자세한 진단은 `docs/cd-runner-diagnostic.md`):
- [ ] `kubectl -n arc-system get pods`로 controller/runner 둘 다 Running
- [ ] `gh api repos/fun-fx/ffx_nexus/actions/runners`로 `total_count > 0`
- [ ] `cd-prod.yml`이 최소 3회 main push에서 자동 success
- [ ] on-call 사용자가 `kubectl --kubeconfig=~/prod.yaml` 권한을 잃어도 됨 (full automation)

이 조건 충족 전까지 **manual deploy가 primary**.

---

## Marketing 사이트 (nexus.ffx.ai) 수동 deploy

marketing 사이트는 Cloudflare Pages에 정적 사이트로 deploy됩니다.
GitHub Actions (`.github/workflows/marketing-pages.yml`)가 main에 push되면
자동 deploy하도록 설정되어 있으나, **wrangler Pages API 401 이슈가 있어**
수동 deploy를 병행합니다.

### 사전 요구사항

- `node` 20+ (Astro 4 빌드)
- `wrangler` 3.114+ (`npm install -g wrangler@^3.114.0`)
- Cloudflare API token (Pages:Edit 권한) — `.env`에 저장
- Cloudflare account ID — `.env`에 저장

### 한 줄 deploy

```bash
cd marketing
set -a && source ../.env && set +a
npm run build
CLOUDFLARE_API_TOKEN="$CLOUDFLARE_API_TOKEN" \
CLOUDFLARE_ACCOUNT_ID="$CLOUDFLARE_ACCOUNT_ID" \
wrangler pages deploy dist \
  --project-name=nexus-marketing \
  --branch=main \
  --commit-dirty=true \
  --commit-message="Marketing deploy from $(hostname)"
```

### 단계별 deploy

```bash
# 1. (선택) marketing/ 변경사항 pull
cd /Users/munsojin/ffx_nexus
git checkout main
git pull --rebase

# 2. .env 로드
set -a; source .env; set +a

# 3. 빌드
cd marketing
npm ci
npm run build

# 4. Deploy
CLOUDFLARE_API_TOKEN="$CLOUDFLARE_API_TOKEN" \
CLOUDFLARE_ACCOUNT_ID="$CLOUDFLARE_ACCOUNT_ID" \
wrangler pages deploy dist \
  --project-name=nexus-marketing \
  --branch=main
```

### 검증

```bash
# Production custom domain
curl -sS -k -o /dev/null -w "HTTP %{http_code} | TTFB %{time_starttransfer}s\n" \
  --max-time 30 https://nexus.ffx.ai/

# Pages deploy URL (이전 deploy URL을 wrangler 출력에서 확인)
curl -sS -k -o /dev/null -w "HTTP %{http_code}\n" \
  --max-time 30 https://<deploy-hash>.nexus-marketing.pages.dev/
```

### GitHub Actions 자동 deploy

`.github/workflows/marketing-pages.yml`이 main push에 트리거되도록 설정됨.
GitHub Actions에서 wrangler 401 에러 발생 시:

1. **로컬 deploy로 우회** (위 절차)
2. **GitHub Actions 디버깅**:
   - workflow에 `echo "${#CLOUDFLARE_API_TOKEN}"` 추가 → env가 실제로 들어가는지 확인
   - token이 Pages:Edit 외에 **Account: Cloudflare Pages: Edit** (새 명명) 권한 필요한지 확인
   - 새 template "Edit Cloudflare Pages"로 token 재발급 후 GH secret 업데이트

### Cloudflare Pages 프로젝트

- Name: `nexus-marketing`
- Production branch: `main`
- Build command: `npm run build` (in `marketing/`)
- Build output: `dist`
- Root directory: `marketing`
- Custom domain: `nexus.ffx.ai`
- DNS: `nexus.ffx.ai` CNAME → `nexus-marketing.pages.dev` (Proxied)

---

## Quick reference

```bash
# One-liner: build + deploy
export KUBECONFIG=~/kubeconfig/prod.yaml && ./scripts/deploy-prod.sh --tag 0.3.6

# 재시작만
./scripts/deploy-prod.sh --restart

# 상태 확인
KUBECONFIG=~/kubeconfig/prod.yaml kubectl -n tenant-nexus get pods,svc,ingress

# 로그
KUBECONFIG=~/kubeconfig/prod.yaml kubectl -n tenant-nexus logs -l app.kubernetes.io/name=nexus -f --tail=100

# 스모크 테스트
./scripts/test_prod_smoke.sh

# 롤백 (이전 release로)
KUBECONFIG=~/kubeconfig/prod.yaml helm -n tenant-nexus history nexus
KUBECONFIG=~/kubeconfig/prod.yaml helm -n tenant-nexus rollback nexus 5  # revision 번호
```
