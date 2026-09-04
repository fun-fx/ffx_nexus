# 배포 경화 작업 기록 (D-2b 종료 → D-2c 캡처 → CI 집행 → 설치 신뢰성)

**기간** 2026-09-03 ~ 09-04 · **범위** PR #292 ~ #297 · **최종 커밋** `d4611ce`
· **규모** 44개 파일, +3366 / −743

이 문서는 무엇을 바꿨는지보다 **무엇이 고장 나 있었는지**를 남기기 위한 기록입니다.
이번 작업에서 고친 것들은 대부분 에러를 내지 않고 조용히 실패하고 있었고, 그래서
"동작한다"는 신호가 있는데도 동작하지 않는 상태가 오래 유지됐습니다. 같은 종류의
공백을 다시 만들지 않는 것이 이 기록의 목적입니다.

## TL;DR

가장 중요한 발견 하나만 남긴다면: **프로덕션 예시 values 로는 소프트웨어가 동작하지
않았습니다.** 게이트웨이·워커·마이그레이션 Job 이 각각 이그레스 규칙을 딱 하나 —
DNS — 만 받았습니다. Postgres 도, ClickHouse 도, 클러스터 밖으로 나가는 경로도
없었습니다. 마이그레이션 훅이 DB 이름은 해석하고 연결은 못 열어서 데드라인까지
매달린 뒤 릴리스를 같이 끌어내리는 상태였습니다. `helm install` 은 성공을 반환했습니다.

그 외에 이번에 닫은 공백:

| 발견 | 상태 |
|---|---|
| 프로덕션/스테이징 예시가 이그레스 없는 릴리스를 만듦 | 렌더 단계 거부로 전환 |
| ClickHouse 이그레스 규칙이 템플릿에 아예 없음 | 3개 정책에 추가 |
| `dependencies.redis` 가 중복 선언돼 필드가 소멸 | 병합 |
| 프록시 규칙이 게이트웨이에만 있어 외부 eval 이 벤더 타임아웃 | 워커에도 추가 |
| 오프라인 하네스 8개가 커밋만 되고 실행되지 않음 | 11개를 필수 검사로 승격 |
| heavy gate 가 수동 dispatch 전용 | 야간 cron 추가 |
| 트레이스 본문(프롬프트·완성)이 기본으로 durable 저장 | 기본 off 옵트인으로 전환 |
| Dependabot 미해결 6건 | 34건 전부 fixed |
| 존재하지 않는 문서를 49곳에서 참조 | 30곳으로 감소, 설치 경로 26곳 중 19곳 해소 |

---

## 1. 병합된 변경

| PR | 제목 | 규모 | 커밋 |
|---|---|---|---|
| [#292](https://github.com/fun-fx/ffx_nexus/pull/292) | D-2c.1 — 캡처 표면 인벤토리 + 누출 재현 고정 | +622/−24 | `079194e` |
| [#293](https://github.com/fun-fx/ffx_nexus/pull/293) | 트레이스 본문 저장을 옵트인으로 (기본 off) | +839/−183 | `4e4ac9a` |
| [#294](https://github.com/fun-fx/ffx_nexus/pull/294) | Dependabot 6건 해소 + 콘솔 테스트 스위트 복구 | +323/−96 | `e0926bc` |
| [#295](https://github.com/fun-fx/ffx_nexus/pull/295) | 오프라인 정책 계약을 CI 에서 실행 + 차트 fail-closed 공백 | +279/−418 | `bbd8186` |
| [#296](https://github.com/fun-fx/ffx_nexus/pull/296) | 집행 게이트 야간 실행 + 취소 유발 concurrency 수정 | +359/−166 | `1100e2f` |
| [#297](https://github.com/fun-fx/ffx_nexus/pull/297) | enforce 모드 이그레스 도달성 + 설치 문서 3종 | +1117/−29 | `d4611ce` |

---

## 2. D-2b 종료

집행 게이트를 손으로 핀한 SHA `69c75bf` 에 대해 dispatch 로 5회 연속 성공시켜
D-2b 를 닫았습니다 (push 1회 포함 총 6회, 전부 success). 게이트 job 자체가 내부적으로
핀된 SHA 에 3회 실행하는 구조이므로, 증거는 "한 커밋에 대한 반복 성공"입니다.

재배선 후에도 유효한지 확인하기 위해 병합 직후 `1100e2f` 에서 dispatch 로 재검증했고,
이번 이그레스 변경(`ac03198`)에서도 실제 enforcing Cilium 클러스터에서 다시 통과시켰습니다.
이그레스를 건드리는 변경이라 오프라인 계약만으로는 부족하다고 판단했습니다.

---

## 3. D-2c — 캡처 게이트

### 무엇이 문제였나

프롬프트·완성·검색 컨텍스트가 별도 설정 없이 ClickHouse 에 그대로 적재되고
있었습니다. 운영자가 켠 적 없고, 끌 수 있는 스위치도 없었습니다.

### 어떻게 고쳤나

`internal/observability/capture.go` 의 `CaptureGate` 가 `Recorder` 를 감싸고, 감싼
쪽으로 넘어가기 전에 Trace 에서 본문 필드를 제거합니다.

게이트웨이 안의 조건문이 아니라 **래퍼**인 이유가 설계의 핵심입니다. Trace 의 두
소비자가 서로 반대되는 것을 원하기 때문입니다. 인프로세스 평가기는 프롬프트와 완성이
있어야 하고(`internal/evals/judge.go`, `remote.go` 는 둘 중 하나라도 비면 점수 없이
반환합니다), durable 저장소는 운영자가 요청했을 때만 보관해야 합니다. `MultiRecorder`
가 각 recorder 에 값 복사본을 주므로 두 소비자는 체인이 아니라 형제이고, 한쪽 가지만
감싸면 다른 쪽은 그대로 둘 수 있습니다. 트레이스 생성 시점에 게이팅했다면 저장을
지키려고 평가를 망가뜨리는 — 보안 이름표를 단 기능 회귀가 됐을 겁니다.

**보관하는 recorder 를 감싸고, eval 워커는 감싸지 마십시오.**

Fail-closed 로 설계했습니다. `NewCaptureGate` 는 캡처가 **명시적으로 켜졌을 때만**
통과시키므로, 설정을 스레딩하는 걸 잊은 호출자는 누출되는 기본값이 아니라 비공개
기본값을 받습니다. 새 본문 필드가 Trace 에 추가됐는데 게이트를 갱신하지 않으면
테스트가 실패하도록 `contentFieldCount` 를 두었습니다 — 고객의 ClickHouse 테이블에서
발견하는 대신에.

### 노출 경로

`config.captureTraceContent` (기본 `false`) → `NEXUS_CAPTURE_TRACE_CONTENT`.

---

## 4. CI 집행 — 있는 줄 알았던 것들

### 오프라인 하네스 8개가 실행된 적이 없음

D-2b 가 하네스 8개를 남겼는데 실행되지 않고 있었습니다. `cni-nightly.yml` 의 path
필터와 주석 처리된 한 줄에만 등장했고, 그래서 거기 인코딩된 계약들은 한 번도
집행되지 않았습니다. **그중 둘은 이미 차트와 어긋나 있었고 아무도 알아채지 못했습니다.**

`Policy contracts (offline)` 잡을 만들어 11개 hermetic 하네스(클러스터·Docker·네트워크
없이 약 3분)를 실행합니다:

- 차트 fail-closed 스위트 — `d2b-failure-reproduction.py`, `mutation_test.py`
- 차트 픽스처·이미지 스위트 5종 — `cni_readiness_gate_test.py`,
  `fixture_namespace_document_boundary_test.py`, `fixture_semantic_admission_test.py`,
  `fixture_yaml_structure_test.py`, `image_pipeline_mutation_test.py`
- heavy 워크플로 아티팩트·트리거 계약 — `test_clean_tree_artifact_routing.sh`,
  `test_cni_workflow_trigger_reindex.sh`
- 업그레이드 리허설 fail-closed 계약 — `test_upgrade_rehearsal_failclosed_contract.sh`
- 픽스처 readiness 관측성 — `test_fixture_readiness_observability.sh` (74/74)

**의도적으로 path 필터를 걸지 않았습니다.** 일부 PR 에서 보고하지 않는 필수 검사는
그 PR 들을 영원히 막습니다.

### heavy gate 가 수동 전용

집행 게이트가 `workflow_dispatch` 에서만 돌아서, 한 번의 수동 캠페인과 다음 캠페인
사이에는 enforcing-CNI 경로를 아무것도 검증하지 않았습니다. 정책 집행을 깨뜨리는 차트
변경이 누가 dispatch 를 떠올릴 때까지 main 에 남아 있는 구조였습니다. 기존 02:00 cron
에 얹어 그 창을 하루로 제한했습니다.

**병합 게이트로 만들지는 않았습니다.** 이 게이트의 증거는 "손으로 핀한 SHA 에 대한
3회 실행"이고, 움직이는 ref 에 대한 1회 실행은 그 증거가 아닙니다. path 필터가 걸린
잡을 필수로 만들면 무관한 PR 을 매달리게 하는 문제도 있습니다.

야간 실행에는 SHA 를 핀하거나 회차를 매길 운영자가 없으므로 `PINNED_SHA` 와
`RUN_INDEX` 를 잡 env 에서 `github.sha` / `nightly-<run_number>` 로 해석합니다.
concurrency 를 워크플로 수준에서 잡 수준으로 내려 heavy gate 가 취소되지 않게 했습니다.

### 필수 검사 10개

`Go`, `Helm chart`, `Schema migrations (real Postgres)`, `Eval regression gate`,
`Eval service (Python)`, `Secret and image scanning`, `Web dashboard`,
`E2E (full suite)`, `Bench live contracts (PG/CH)`, `Policy contracts (offline)`.

CNI 잡들은 path 필터 때문에 모든 PR 에서 보고하지 않으므로 제외했습니다 — 이유는
`.github/branch-protection.md` 에 기록돼 있습니다.

---

## 5. 설치 신뢰성 — 가장 큰 발견

문서를 쓰려고 프로덕션 예시를 **실제로 렌더해 본 것**이 계기였습니다. 원인이 셋이었고
셋 다 에러가 아니라 침묵을 냈습니다.

### (a) 이그레스 피어가 예시가 채우지 않는 필드에서 나옴

모든 규칙이 `dependencies.<dep>.host` 로 게이트됩니다. 피어는 렌더 중에 읽을 수 있는
값으로 만들어야 하는데, 연결 URL 은 Secret 안에 있어서 Helm 이 읽을 수 없기
때문입니다. 기존 게이트는 정책 쪽 절반(selector/cidr 모드가 켜졌는지)만 검사했고,
그래서 **"URL 있고 host 없음"은 모든 검사를 통과하고 규칙을 하나도 내지 않았습니다.**
예시 파일은 셋 다 켜고 셋 다 비워 뒀습니다.

`values.schema.json` 의 `dependency` 설명이 이 사고를 문장으로 예언해 뒀습니다 —
"URL 을 채운 운영자가 NetworkPolicy 피어 필드를 실수로 비워두지 않도록". 강제하는
코드는 없었습니다.

이제 템플릿이 의존성별로, 그리고 그 의존성이 **켜져 있을 때만** 거부합니다. 아무것도
켜지 않는 프로필(개발 프로필, 픽스처 설치)은 영향받지 않습니다.

### (b) ClickHouse 규칙이 아예 없었음

설정 실수가 아니라 템플릿에 그런 규칙이 처음부터 없었습니다. enforce 에서 모든
트레이스·eval 점수 쓰기가, 허용해 줄 규칙 없이 default-deny 에 막혔습니다.
게이트웨이·워커·마이그레이션 정책에 추가했고, 모드는 스키마가 이미 정의해 둔 두 가지
— 인클러스터 Service 용 namespace, 관리형 엔드포인트용 CIDR — 를 그대로 씁니다.

네임스페이스 스코프이고 파드 라벨 스코프가 아닙니다. 차트에 ClickHouse 파드 라벨
입력이 없고, 라벨을 추측하면 아무것도 매칭하지 않는 규칙이 렌더됩니다 — 더 긴 경로를
거친 같은 침묵입니다.

### (c) `dependencies.redis` 가 두 번 선언됨

YAML 은 뒤의 선언이 이기므로 `host`·`port`·`namespace` 가 조용히 사라졌고, Redis
이그레스 규칙은 **어떤 입력으로도** 렌더될 수 없었습니다.

### (d) 프록시 규칙이 게이트웨이에만 있었음

외부 eval 플러그인은 워커의 일입니다 — Langfuse·LangSmith·Datadog 등으로 트레이스를
전달하고 전부 클러스터 밖입니다. enforce 에서 이들이 벤더 타임아웃으로 실패하고
있었습니다. ClickHouse 규칙과 함께 헬퍼로 추출해 둘 다에 넣었고, 데이터스토어하고만
대화하는 마이그레이션 정책에는 의도적으로 넣지 않았습니다.

### 결과

프로덕션 예시 렌더 결과 — 게이트웨이 `53, 5432, 9000, 3128`, 워커 동일,
마이그레이션 `53, 5432, 9000`. 스테이징도 동일한 형태입니다(리허설이 의미를 가지려면
정책 집합이 같아야 합니다).

README 의 helm 명령 **세 개가 전부 실패**하고 있었습니다. Nexus 를 처음 써 보는
방법으로 제시된 첫 번째조차 enterprise 프로필의 acknowledgement 거부에 걸렸고,
README 는 NetworkPolicy 를 언급조차 하지 않았습니다. 셋 다 렌더되도록 고쳤습니다.

---

## 6. 문서

존재하지 않는 문서를 49곳에서 참조하고 있었고(문서 15개), 그중 26곳이 고객이 설치할
때 따라가는 경로 위에 있었습니다. `values.yaml`, 예시 values 2개,
`migrations/README.md`, 마이그레이션 Job 템플릿이 각각 설치 가이드와
업그레이드/롤백 가이드를 가리켰는데 **둘 다 쓰인 적이 없었습니다.** 마이그레이션
실패를 만난 사람이 파일 이름으로 안내받고 있었습니다.

3개를 썼고 26곳 중 19곳이 해소됐습니다 (전체 49곳 → 30곳, 문서 15개 → 11개).

- [`network-policy-prerequisites.md`](network-policy-prerequisites.md)
- [`customer-self-hosted-upgrade-rollback.md`](customer-self-hosted-upgrade-rollback.md)
- [`customer-self-hosted-install.md`](customer-self-hosted-install.md)

내용은 전부 코드에서 읽어냈고 주장마다 출처와 대조했습니다 — advisory lock 키
(`4242042001`), 원장 테이블·컬럼, 훅 애너테이션과 weight(`-5`), 실패한 Job 을 일부러
남기는 삭제 정책, readiness 검사 이름과 required 여부, master key 길이, 포트, CLI
종료 코드. 잘못된 설치 문서는 없는 것보다 나쁩니다.

읽는 중에 발견한 함정도 같이 적었습니다: `deploymentMode: split`,
`gateway.*`/`worker.*`/`connections.*` 블록, `serviceTargets` 6개 중 4개는
`values.yaml` 에 설명돼 있지만 **어떤 템플릿도 읽지 않아서** 설정해도 아무 일이
없습니다. 실제로 읽히는 것은 `serviceTargets.sso.namespace` 와
`serviceTargets.resend.namespace` 둘뿐입니다.

---

## 7. 보안 위생

Dependabot 미해결 6건(공급망 critical, XSS, open-redirect 포함)을 해소해 추적 중인
34건이 전부 `fixed` 상태입니다. `react-router-dom` 업그레이드가 포함됩니다.

콘솔 테스트 스위트가 죽어 있던 것도 살렸습니다 — jsdom 에 `localStorage` /
`sessionStorage` 심을 넣어 `npm test` 가 다시 의미를 갖습니다.

---

## 8. 이번에 고친 CI 이식성 버그

로컬(macOS)에서는 통과하고 Linux 러너에서만 실패하던 것들입니다. 하네스를 CI 에
연결하기 전에는 드러날 수 없었던 종류입니다.

| 증상 | 원인 |
|---|---|
| `test_upgrade_rehearsal_failclosed_contract.sh` rc=127 | `python3` 경로가 `/opt/homebrew/bin/python3` 로 하드코딩 |
| `test_fixture_readiness_observability.sh` c12e 실패 | `grep -E` 에 `'\t'` 를 넘겨 BSD/GNU 해석이 갈림 (ANSI-C 인용 `$'\t'` 로 수정) |
| `TestProdOTLPRecorderLiveBody` 간헐 패닉 | `http.Server.Shutdown(nil)` — 타이밍에 따라 nil 컨텍스트로 패닉 |
| `policy-contracts` 잡 실패 | shallow clone 에서 `origin/main` 미해석 (`fetch-depth: 0`) |

---

## 9. 운영자 영향 — 업그레이드 시 렌더가 거부될 수 있음

**이번 변경 중 유일한 파괴적 변경입니다.** `dependencies.<dep>.enabled=true` 를
`host` 없이 쓰고 있었다면, 다음 `helm upgrade` 에서 렌더가 거부됩니다:

```
networkPolicy: mode=enforce renders a default-deny policy, and these
dependencies are enabled with no egress peer to reach them, so the release
would install and then fail at runtime: dependencies.postgres.enabled=true
but dependencies.postgres.host is empty (...)
```

이것은 **이미 고장 나 있던 설정을 드러내는 것**이지 새로 깨뜨리는 것이 아닙니다. 그
설정으로 돌아가던 릴리스는 해당 데이터스토어에 도달하지 못하고 있었습니다. 조치는
메시지가 지목하는 필드를 채우는 것이고, 정책 집행을 원하지 않으면
`networkPolicy.mode=disabled` 입니다.

CI 에서도 같은 일이 한 번 일어났습니다. `Helm chart` 잡의 마이그레이션 훅 픽스처 두
개가 host 없이 Postgres 를 켜고 있어서 렌더가 거부됐고, 무관한 실패 5개로
보고됐습니다. 그중 하나는 **Job 부재를 단정하는 테스트**여서 렌더가 통째로 실패하면
만족됩니다 — 아무것도 증명하지 못한 채 통과하고 있었고, `migrations.enabled` 가 완전히
고장 나도 계속 통과할 상태였습니다. 릴리스가 여전히 렌더된다는 양성 단정을 추가했습니다.

---

## 10. 현재 검증 상태

| 항목 | 상태 |
|---|---|
| 필수 검사 | 10개, `main` 보호 활성 |
| 오프라인 하네스 | 11개, PR 필수, 약 3분 |
| readiness 관측성 | 74/74 |
| helm 렌더 회귀 스위트 | 59 passed / 0 failed |
| Go 전체 | 통과 |
| heavy gate | 야간 cron + 수동 dispatch, 최근 실행 전부 success |
| Dependabot | 34건 전부 fixed |

---

## 11. 남은 것 / 알려진 공백

의도적으로 이번 범위에서 뺀 것들입니다.

**프로바이더 이그레스(443) 부재는 여전히 거부되지 않습니다.** 데이터스토어와 정확히
같은 부류의 실패입니다 — 게이트웨이가 뜨고, readiness 를 통과하고, healthy 를
보고하고, `api.openai.com` 에 닿아야 하는 모든 요청을 실패시킵니다. 막으려면 "인클러스터
모델만 쓴다"는 새 opt-out 플래그가 필요해서 넣지 않았고, 세 문서에 명시했습니다.
`/readyz` 에 이 경로에 대한 검사가 없다는 점도 함께 적었습니다.

**죽은 설정 표면.** `serviceTargets.postgres`/`.redis`/`.clickhouse`/`.egressProxy`,
`deploymentMode: split`, `gateway.*`/`worker.*`/`connections.*` — 문서화는 했지만
제거하거나 구현하지는 않았습니다.

**끊긴 참조 11개 문서 / 30곳.** 설치에 인접한 것은 `network-allowlist.md`(6곳)와
`customer-self-hosted-backup-recovery.md`(1곳)이고, 나머지는 eval-plugins·packaging
같은 내부 설계 문서입니다.

**README 버전 표기 표류.** `Chart.yaml` 이 `0.6.12` 인데 README 본문은 v0.5.1 을
"현재 릴리스"로 적고 있습니다. GitHub 릴리스는 `v0.4.0` 하나뿐이라 임의로 고치지
않고 남겨 뒀습니다.

**D-2c.4 이후.** 별도 트랙으로 분리했습니다.

---

## 관련 문서

- [`d2c-design-inventory.md`](d2c-design-inventory.md) — 캡처 표면 인벤토리
- [`d2c-implementation-spec.md`](d2c-implementation-spec.md)
- [`d2b-final-report.md`](d2b-final-report.md) — D-2b 집행 검증
- [`network-policy-prerequisites.md`](network-policy-prerequisites.md)
- [`customer-self-hosted-install.md`](customer-self-hosted-install.md)
- [`customer-self-hosted-upgrade-rollback.md`](customer-self-hosted-upgrade-rollback.md)
- `.github/branch-protection.md` — 필수 검사와 CNI 잡 제외 사유
