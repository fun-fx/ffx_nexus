# 데모 영상 스크립트 — Nexus 첫 사용 90초 가이드

> 데모 **녹화용** 워크스루. 처음 사용하는 사용자 시점 — 가입 → 키 발급 → 첫 호출 → 대시보드에서 실시간으로 결과 확인. 데모 환경은 깨끗하게 reset 된 상태에서 시작한다고 가정 (`scripts/demo_reset.sh` 참고).
>
> 스크립트는 실제 provider key 를 화면에 노출하지 않습니다 — `AQ.Ab8R...xxxx` 처럼 마스킹된 자리표시자를 사용하거나, 화면 캡처 시 오버레이로 가려주세요. 또는 테스트용 throwaway Gemini / OpenAI key 를 새로 발급해서 사용해도 됩니다.
>
> **English version**: [`docs/demo-script.md`](demo-script.md)

---

## 0. 녹화 시작 전 준비

1. 다른 탭과 알림 모두 닫기 (시스템 설정 → 알림 → "방해금지 모드 1시간" 추천).
2. Dock 을 자동으로 숨기도록 설정 → 녹화 중 갑자기 튀어나오지 않게.
3. **Chrome 시크릿 창** 새로 열기 (Cmd+Shift+N) — 쿠키·캐시가 깨끗하여 데모에 영향 안 줌.
4. Chrome 창 크기를 **1440 × 900** 으로 조정.
5. 화면 녹화 시작: **Cmd+Shift+5 → 선택 영역 녹화** (권장) — 다른 앱 알림이 들어와도 영역 밖이라 안전.
6. Chrome 확대 100%, 밝은 테마 설정 (설정 → 모양 → 라이트).

---

## 1. 인트로 (≈ 0:00–0:20)

> **내레이션:**
> "안녕하세요. 이번엔 Nexus 를 처음 사용하는 모습을 시연해 보겠습니다.
> 가입부터, provider key 등록, virtual key 발급, 첫 chat completion 호출,
> 그리고 대시보드에서 실시간으로 결과가 뜨는 것까지 — 전체 약 90초 정도면 됩니다."

커서: 비어 있는 `localhost:5173` 페이지 위에 idle 상태.

---

## 2. 대시보드 열기 (≈ 0:20–0:30)

액션:

1. Chrome 을 연다.
2. 주소창에 `localhost:5173` 입력.
3. <kbd>Return</kbd> 키.

> **내레이션:**
> "이게 Nexus 대시보드입니다. 모두 비어 있죠 — user 도 없고 trace 도 없습니다.
> 방금 dev 환경을 reset 했기 때문이에요. 가장 먼저 할 일은 계정 만들기입니다."

페이지가 완전히 렌더링될 때까지 대기 (상단에 **Overview / Sign in** 탭,
우상단에 **OFFLINE** 표시 — 아직 로그인 안 했기 때문).

---

## 3. 로그인 → 계정 만들기 (≈ 0:30–1:10)

액션:

1. 우상단의 **Sign in** 버튼 클릭.
2. 열린 패널에서 **Create account** 탭 클릭 (오른쪽).
3. 양식 채우기:
   * email: `demo@nexus.local`
   * password: `hunter2hunter` (시청자가 따라칠 수 있게 천천히 입력)
   * provider dropdown: **gemini**
   * label: 비워두기
   * your LLM API key: (예시) `AQ.Ab8R…vA4` — 앞 8자만 보이고 뒤는 마스킹됩니다. **절대 실제 키를 붙여넣지 마세요.**
4. **Create account** 버튼 클릭.

> **내레이션:**
> "BYOK, Bring Your Own Key 방식입니다. 각 사용자가 자기 provider key 를 들고 와서 —
> LLM 비용은 여전히 자기 provider 에서 청구됩니다. Nexus 는 그 key 를 우리 DB 에
> AES-GCM 으로 암호화해서 저장하고, 평문으로는 절대 로그하지 않아요. 그래서 우리도
> 사용자의 키를 볼 수 없습니다."

---

## 4. Virtual key 복사 (≈ 1:10–1:30)

액션:

1. 양식 제출 후, **Account created** 패널이 나오면서 `<code>` 블록 안에 virtual key 가 표시됨 — `nxs_live_` 로 시작하는 긴 문자열.
2. 가상 키를 triple-click 으로 선택 → <kbd>Cmd+C</kbd> → **Continue to dashboard** 클릭.

> **내레이션:**
> "Nexus 가 방금 이 계정에 대한 virtual key 를 발급했습니다. 이게 앱이 Nexus 와
> 통신할 때 쓰는 유일한 credential 입니다. 앱은 gateway 요청의 `Authorization`
> Bearer 헤더에 이걸 넣습니다. **단 한 번만** 보여주기 때문에 잃어버리면 새로
> 발급해야 해요. 브라우저 밖으로 한 번도 나가지 않고 모든 게 끝났습니다."

---

## 5. 첫 chat completion 호출 (≈ 1:30–2:10)

액션:

1. 터미널 창으로 전환 (사전 open 해두기).
2. 아래 curl 실행 — 4단계에서 복사한 virtual key 를 `Authorization` 헤더에 paste.
   시청자는 전체 요청/응답을 보게 됩니다.

```bash
curl http://localhost:8090/v1/chat/completions \
  -H "Authorization: Bearer nxs_live_..." \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-2.5-flash",
    "messages": [{"role": "user", "content": "Say hi in five words"}]
  }'
```

> **내레이션:**
> "한 번 chat completion 을 보냅니다. 표준 OpenAI 프로토콜 그대로에요 — base URL
> 만 바뀌었을 뿐. 우리 앱 입장에서는 다른 건 아무것도 바뀐 게 없습니다."

3. 응답 JSON 에 `"content": "Hi. Hello there, friend."` 같은 결과가 나옵니다.

---

## 6. 대시보드 복귀 — trace 가 나타남 (≈ 2:10–2:45)

액션:

1. Chrome 으로 다시 전환.
2. 5초 이내에 **Recent traces** 테이블로 스크롤.

> **내레이션:**
> "대시보드가 5초마다 ClickHouse 를 폴링합니다. 잠깐만 — 방금 호출한 trace 가
> 떴어요. 모델, latency, 토큰 수, 비용, 그리고 flags 가 보입니다 —
> 'cache' 가 있으면 캐시 적중, 'blocked' 가 가드레일 발동, 'byok' 은
> 암호화된 저장소에서 가져온 키라는 뜻입니다."

커서로 가리키기:

* status 셀 (200, 초록색)
* provider 태그 (`gemini`)
* tokens (입력/출력)
* latency 컬럼
* 우상단 LIVE indicator (이제 초록색)

---

## 7. Cache trigger (≈ 2:45–3:15)

액션:

1. 터미널에서 <kbd>↑</kbd> 키를 눌러 **동일한** curl 한 번 더 실행.
2. 훨씬 빠른 응답 (1초 미만) 받기.
3. Chrome 으로 다시 전환.

> **내레이션:**
> "이제 정확히 같은 요청을 다시 실행합니다. 이번엔 몇 밀리초 만에 결과를 받았고,
> flags 컬럼에 'cache' 배지가 떴습니다. 이게 semantic cache 입니다 — 반복
> 호출은 무료이고, latency 컬럼이 그만큼 작아진 게 보입니다."

(선택) 새 row 에서 `cache` 배지와 작은 latency 숫자를 잠시 강조.

---

## 8. Guardrail trigger (≈ 3:15–4:05)

액션:

1. prompt 를 본인 것이 아닌 타인의 email 주소가 포함된 것으로 교체. 예:

   ```bash
   curl http://localhost:8090/v1/chat/completions \
     -H "Authorization: Bearer nxs_live_..." \
     -H "Content-Type: application/json" \
     -d '{
       "model": "gemini-2.5-flash",
       "messages": [{"role": "user", "content": "Email my old colleague at jane.doe@example.com"}]
     }'
   ```

2. 터미널에서 `403` + `input_blocked:pii_input` 응답 확인.

> **내레이션:**
> "누군가 Nexus 로 다른 사람의 개인정보를 빼내려 시도하면, 가드레일 레이어가
> 토큰 비용 발생 전에 요청을 막아버립니다. trace row 에서 — status 가 4xx,
> 'blocked' flag 가 보입니다. 그리고 대시보드의 Guardrail events 카운터도
> 하나 올라갔어요."

Chrome 으로 돌아와서 가리키기:

* trace row 의 `blocked` 배지
* **Guardrail events** 카드 카운트 +1

---

## 9. 마무리 (≈ 4:05–4:30)

Overview 페이지 상단으로 스크롤, 카드들 보여주며:

> **내레이션:**
> "이게 Nexus 입니다 — 한 명령으로 설치, 5분이면 deploy, 실시간으로 동작
> 확인까지. 소스는 Apache 2.0, 대시보드는 MIT 라이선스입니다. 추가로 연결할
> 것도 없습니다. 시청해 주셔서 감사합니다."

녹화 종료.

---

## 데모 후 정리

```bash
bash scripts/demo_reset.sh        # Postgres + ClickHouse reset
kill "$(cat $HOME/.nexus/nexus.pid)"
pkill -f vite                    # dashboard 종료
```

다음 데모는 같은 빈 상태에서 다시 시작.

---

## (선택) 시간이 부족할 때 컷

* **curl 단계 생략.** OpenAI Python SDK 에 `base_url="http://localhost:8090/v1"` 만 설정해서 동일한 효과.
* **Guardrail 섹션 생략.** ≈ 50초 절약; cache 섹션만으로도 충분히 인상적.
* **큰 화면 사용.** `localhost:5173` 는 4K 까지 반응형이지만, 1440×900 에서 좌우 8개 카드가 가장 깔끔.

---

## 한국어 스크립트 노트

* 위 대본을 그대로 읽으면 자연스러운 한국어 나레이션이 됩니다.
* 영어 발음 발화 (예: "OpenAI", "ClickHouse", "BYOK") 는 한 번 원어 그대로 발음하고 한국어 설명을 곁들이면 좋습니다.
* "ClickHouse" 는 발음 그대로 "클릭하우스" 와 "ClickHouse" 둘 다 자연스러움.
* "BYOK" 는 "Bring Your Own Key" 풀어 설명 후 "BYOK" 를 언급하는 게 시청자 입장에서 명료.

## 다음 단계

이 문서 (`docs/demo-script.md`) 와 영어판은 같은 line-by-line 단계로 동기화되어 있으므로, 같은 데모에서 영문/한글 내레이션을 혼용하던, 바꿔서 쓸 수 있습니다.
