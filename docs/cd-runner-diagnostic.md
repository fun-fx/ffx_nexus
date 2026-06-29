# CD self-hosted runner 진단 + 복구 runbook

Status: **CURRENT** — `cd-prod.yml`의 `runs-on: self-hosted` 잡이 1주일 넘게 queued로 멈춤.
Last updated: 2026-06-29

**결론부터**: GitHub App `FFX Actions Runner Controller`는 **All repositories**로 정상 설정. repo-access는 문제 아님.
진짜 문제는 **`fun-fx/ffx_nexus`에 등록된 self-hosted runner가 0개**라는 것. ARC가 cluster에서 pod를 못 띄우거나, helm release가 없거나, 띄웠는데 죽었거나.

```
$ gh api repos/fun-fx/ffx_nexus/actions/runners
{"total_count":0,"runners":[]}
```

GitHub 측에서는 매칭되는 runner가 0개라 잡이 `queued`로 영원히 머무름.

---

## 0. 이 문서의 전제

- **당신**: 클러스터 admin (kubectl, helm, flux 다 됨). git repo 권한도 있어서 PR/머지 가능.
- **사장님**: org owner. 사장님께 요청해야 하는 것은 **GitHub 측 admin** (org-level runner API, App private key 회전)뿐. 5분짜리 1회 작업.
- **GitHub Actions runner 자체의 설치/관리는 org 또는 repo admin이어야 함** — 이 문서 마지막 §6 참고.

---

## 1. 빠른 진단 (5분)

prod bastion에 SSH로 들어가서 (또는 local에서 `KUBECONFIG=~/kubeconfig/prod.yaml kubectl`로) 아래를 **순서대로** 실행. 결과를 복사해서 채팅에 붙여주면 다음 단계가 명확해짐.

### 1.1 ARC namespace가 있나

```bash
kubectl get ns arc-system 2>&1
kubectl -n arc-system get all 2>&1 | head -30
```

| 결과 | 의미 | 다음 |
|---|---|---|
| `Error from server (NotFound)` | ARC가 **설치되지 않음** | §3 (재설치) |
| `arc-system` 있음, pod 있음 | controller는 떠있음 | §1.2 |
| `arc-system` 있음, pod 없음 | helm release가 uninstall됐거나, helm chart가 잘못됨 | §3 |

### 1.2 controller pod + runner pod 상태

```bash
kubectl -n arc-system get pods -o wide 2>&1
```

| Pod | 기대 상태 | 실패 시 |
|---|---|---|
| `actions-runner-controller-*` (1개) | `Running` | §4 로그 확인 |
| `*-runner-set-*` 또는 `actions-runner-*` (≥1) | `Running` 또는 `Pending` | §1.3 |

Cozystack 환경이면 (deploy/cozystack/ 사용 중):

```bash
# flux로 관리되는 helm release 확인
flux get hr -A 2>&1 | grep -iE "runner|arc"
kubectl -n arc-system get hr 2>&1
```

### 1.3 runner pod가 `Pending`이면

```bash
kubectl -n arc-system describe pod -l actions.github.com/scale-set-name 2>&1 | tail -30
```

흔한 원인:
- node가 `NotReady` 또는 `cordoned` (Talos/Cozystack maintenance 모드)
- `arc-system` ServiceAccount가 쓸 image pull secret 없음 (Harbor 자격증명 만료)
- PDB / NodeAffinity 미스매치

### 1.4 controller 로그에서 인증/연결 문제 찾기

```bash
kubectl -n arc-system logs -l app.kubernetes.io/name=actions-runner-controller --tail=200 2>&1 | tail -50
```

키워드:
- `401` / `403` → App private key 또는 installation token 만료 → §5
- `404 Not Found` → scale set 이름 오타, repo/org 매핑 오타 → §5
- `dial tcp ... i/o timeout` → egress에서 GitHub API 차단

### 1.5 GitHub 측 org runners 직접 확인

repo-level은 위 `gh api ... /actions/runners`로 끝. **org-level**은 admin:org scope 필요 → 본 계정에 없으면 사장님께 한 줄 요청:

```
사장님, 다음 명령 결과만 공유 부탁드립니다 (30초):
gh api orgs/fun-fx/actions/runners --jq '.runners[] | {name, status, busy, os}'
```

---

## 2. 시나리오별 복구

### 시나리오 A: runner pod는 떴는데 GitHub 연결 실패 (가장 흔함)

`kubectl logs`에서 `401`/`403`/`404` 보이거나, pod는 Running인데 `gh api ... /actions/runners`가 여전히 0.

1. App installation 확인 (사장님):
   - https://github.com/organizations/fun-fx/settings/installations → `FFX Actions Runner Controller` → Configure
   - "App settings" → **Private keys**: 회전 필요하면 ① Generate new key ② 다운 ③ 시크릿 갱신
2. 클러스터에 저장된 GitHub App 시크릿 갱신:
   ```bash
   kubectl -n arc-system get secret 2>&1
   # 보통 controller-manager secret, github-app-secret 같은 이름
   kubectl -n arc-system create secret generic github-app-secret \
     --from-file=github-app-private-key.pem=/path/to/new-key.pem \
     --dry-run=client -o yaml | kubectl apply -f -
   ```
3. controller rollout restart:
   ```bash
   kubectl -n arc-system rollout restart deploy/actions-runner-controller
   ```

### 시나리오 B: runner pod가 죽어있음

`kubectl get pods`에서 `CrashLoopBackOff` / `Error` / `OOMKilled` 보이면:

```bash
kubectl -n arc-system logs <pod> --tail=100
kubectl -n arc-system describe pod <pod> | tail -30
```

흔한 원인:
- **OOMKilled**: pod 메모리 limit 너무 작음 → `RunnerScaleSet`의 `template.spec.containers[].resources` 조정
- **ImagePullBackOff**: Harbor 자격증명 만료 → `imagePullSecrets` 갱신
- **CrashLoopBackOff**: App 시크릿 형식 오류 (PEM vs base64) → §5

### 시나리오 C: ARC가 통째로 없음 (가장 큼, §3으로)

`kubectl get ns arc-system` → NotFound. **재설치**가 필요.

---

## 3. ARC 재설치 (시나리오 C)

helm chart: `oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set` (v0.10+)

이건 **PR**로 만들고 main에 머지하면 helm chart/values가 cluster에 적용됨 (Cozystack이면 flux가 reconcile). 흐름:

1. **GitHub 측에서 runner 등록 token 받기 (사장님)**
   ```
   사장님, ARC 재설치합니다. 다음 명령을 한 번만 실행해주시면 됩니다:
   gh api -X POST orgs/fun-fx/actions/runners/registration-token --jq '.token,.expires_at'
   ```
2. **values 작성**: `deploy/cozystack/07-arc.yaml` (또는 kustomize overlay)
3. **PR**: `feat/arc-restore` → main 머지 → flux가 reconcile

원하는 경우 제가 PR을 통째로 작성해드릴 수 있습니다. 어떤 시나리오에 해당하는지 §1 결과를 채팅에 붙여주시면 그에 맞춰 진행.

---

## 4. 사장님께 보내야 할 1줄 (GitHub 측 admin)

본인 권한 밖. 짧고 굵게:

```
사장님, CD 잡이 1주일째 queued로 멈춤. 진단 결과:
- ffx_nexus repo에 등록된 runner 0개
- ARC는 cluster에 설치 안 되어 있거나, 있어도 runner pod 0개
GitHub 측에서 (1) App 'FFX Actions Runner Controller' private key 유효한지
(2) org-level runner가 0개면 gh cli로 한 줄:
  gh api -X POST orgs/fun-fx/actions/runners/registration-token --jq .token
결과 토큰을 저한테 주시면 cluster에 secret으로 등록하고 helm chart PR 올리겠습니다.
```

---

## 5. 일반적인 cluster 측 작업 (당신이 직접)

- `kubectl -n arc-system edit hr <name>` — helm release values 수정
- `kubectl -n arc-system get secret,configmap` — App credentials 확인
- `kubectl logs -f ...` — controller / runner 로그
- `helm -n arc-system list` — 설치된 차트 버전

---

## 6. runner 자체 (GitHub 측) admin 작업 — 사장님 영역

`org runners` API는 org owner만:
- `POST /orgs/{org}/actions/runners/registration-token` — 새 runner 등록 토큰
- `POST /orgs/{org}/actions/runners/remove-token` — 제거 토큰
- `GET /orgs/{org}/actions/runners` — org-level runner 목록

`gh auth refresh -h github.com -s admin:org` 한 번 하면 본인도 가능하지만, prod 환경 권한 격리를 위해 사장님께 요청하는 게 안전.

---

## 7. 복구 확인

```bash
# cluster 측
kubectl -n arc-system get pods  # controller + runner 모두 Running

# GitHub 측
gh api repos/fun-fx/ffx_nexus/actions/runners --jq '.total_count'  # > 0

# E2E
gh workflow run cd-prod.yml --ref main
sleep 30
gh run list --workflow="CD (prod)" --limit 1  # conclusion: success
```

세 조건 모두 OK면 `docs/manual-deploy.md`의 "manual deploy가 primary" 표기를 "auto deploy 복귀"로 되돌리고, 이 문서는 archive.

---

## 변경 이력

- 2026-06-29: 첫 작성. App 권한 가설 폐기, runner 0개 가설로 전환.
