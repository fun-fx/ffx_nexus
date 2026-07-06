# 데모 영상 스크립트 — Nexus 첫 사용 90초 가이드

> 데모 **녹화용** 워크스루. 처음 사용하는 사용자 시점 — 가입 → 키 발급 → 첫 호출 → 대시보드에서 실시간으로 결과 확인.
>
> **반드시** `bash scripts/demo_reset.sh` 로 환경을 시작하세요. 이 스크립트가 semantic cache·guardrail·signup·quality-aware routing(`auto`) 을 한 번에 켭니다. Nexus 를 수동으로 띄우면 7·8·9단계(cache / blocked / auto routing)가 동작하지 않습니다.
>
> 스크립트는 실제 provider key 를 화면에 노출하지 않습니다 — `AQ.Ab8R...xxxx` 처럼 마스킹된 자리표시자를 사용하거나, 화면 캡처 시 오버레이로 가려주세요. 또는 테스트용 throwaway Gemini / OpenAI key 를 새로 발급해서 사용해도 됩니다.
>
> `demo_reset.sh` 는 Postgres 의 모든 user·session 을 **삭제**합니다. reset 직후에 브라우저는 stale 세션 쿠키를 들고 있어서 `My usage` 가 `login required` 응답을 받게 되고, React 가 빈 body 를 JSON.parse 하려고 하면 콘솔에 `Unexpected end of JSON input` 가 찍힙니다. **반드시 시크릿 창 (Cmd+Shift+N) 으로 시작**해서 이 상황을 피하세요 — 데모 스크립트의 다른 단계도 같은 시크릿 창에서 진행합니다.
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
7. **(선택, 9단계 auto routing)** 라우팅 비교를 풍부하게 보여주려면 녹화 전에 provider key 를 두 개 이상 export 한 뒤 reset 하세요. 예: `export GEMINI_API_KEY=...` 와 `export OPENAI_API_KEY=...` → `bash scripts/demo_reset.sh`. key 가 하나만 있어도 `auto` 는 동작하지만, **Model routing** 테이블에 모델이 하나만 보입니다.

---

## 3b. 자신의 OpenAI-호환 upstream 연결하기 (선택, ≈ 0:15 추가)

> **데모 시간 여유가 있을 때만 사용.** Built-in Gemini/OpenAI/Grid 데모만으로도 핵심은 다 보여주므로, 별도 시간이 없는 릴리스 데모라면 이 단계는 건너뛰고 §4 부터 진행하세요. 청중이 “OpenRouter 같은 외부 gateway 도 붙을 수 있나요?”를 물어볼 때만 꺼내면 됩니다.

세 번째 (§3) **Create account** 형식의 provider 드롭다운을 실행한 후 dropdown 끝에서 선택하세요:

**Custom (OpenAI-compatible)…**.

선택하면 세 가지 추가 입력 필드가 나타납니다:

1. **Provider name** — `openrouter`, `together`, `fireworks`, `mycorp-llm` 같은 쉼은 식별자. 소문자+숫자만 허용.
2. **Base URL** — OpenAI-호환 루트. 예: `https://openrouter.ai/api/v1` 또는 `https://llm.example.com/v1`.
3. **Chat models** (선택) — 콤마로 구분. 예: `openai/gpt-4o, anthropic/claude-3.5-sonnet`.
4. **Embed models** (선택) — 콤마로 구분. 예: `text-embedding-3-large`.

**Create account** 버튼을 누르면 Nexus 가 다음 boot 시 자동으로 UserCompat 어댑터를 자가 등록합니다. 채팅 모델은 Playground model picker 자동완성에 `user/<provider>/<model>` 형태로 나타납고 (예: `user/openrouter/openai/gpt-4o`), outbound 요청 시에는 prefix 가 자동으로 떼어 upstream 에 원래 model id 만 전달됩니다. Go 리빌드, config 수정, provider-side adapter 없이 연결 가능합니다.

---

## 1. 인트로 (≈ 0:00–0:20)

> **내레이션:**
> "안녕하세요. 이번엔 Nexus 를 처음 사용하는 모습을 시연해 보겠습니다.
> 가입부터, provider key 등록, virtual key 발급, 첫 chat completion 호출,
> cache·guardrail, 그리고 eval 기반 **auto** 라우팅까지 — 대시보드에서
> 실시간으로 확인합니다."

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
2. 훨씬 빠른 응답 (수십 ms) 받기.
3. Chrome 으로 다시 전환.

> **참고:** 첫 호출은 upstream(Gemini)으로 가서 응답을 **저장**합니다. 두 번째 **완전히 동일한** 호출부터 cache 배지가 뜹니다. `demo_reset.sh` 없이 Nexus 를 띄웠다면 cache 가 꺼져 있어서 배지가 안 나옵니다.

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

## 9. Auto routing — eval 기반 모델 선택 (≈ 4:05–4:50)

액션:

1. 터미널에서 `model` 을 **`auto`** 로 바꾼 curl 실행 (virtual key 는 그대로):

   ```bash
   curl http://localhost:8090/v1/chat/completions \
     -H "Authorization: Bearer nxs_live_..." \
     -H "Content-Type: application/json" \
     -d '{
       "model": "auto",
       "messages": [{"role": "user", "content": "List three benefits of an AI gateway in one sentence each."}]
     }'
   ```

2. 응답 JSON 확인:
   * 요청의 `"model": "auto"` — 클라이언트가 보낸 **alias**
   * 응답의 `"model": "gemini-2.5-flash"` (또는 다른 concrete id) — Nexus 가 **실제로 고른** upstream 모델

3. **같은 curl 을 2~3번 더** 실행 (prompt 를 조금 바꿔도 됨). eval worker 가 trace 를 비동기로 채점하고, routing stats 가 쌓입니다.

4. Chrome 으로 전환 → Overview 상단으로 스크롤.

> **내레이션:**
> "이제 model 이름 대신 **`auto`** 를 씁니다. 지금 등록된 provider 는 **Gemini** 와 **The Grid** (spot market) 두 개예요. Nexus 가 ClickHouse trace 와 eval score 로 품질·비용·latency 를 집계해서, 이 순간에 더 나은 쪽으로 자동 라우팅합니다. 앱 코드는 `auto` 만 고정하면 되고, 실제로 어떤 모델이 선택됐는지는 응답 JSON 의 `model` 필드와 trace 에서 확인할 수 있습니다."

커서로 가리키기:

* **Model routing** 테이블 — `eff_quality` 막대, `avg_latency_ms`, `avg_cost_usd`, `samples`
* **Eval scores (24h)** — 앞선 호출들에서 쌓인 `completeness`, `pii_leak` 등 (heuristics)
* **Recent traces** — `request_model` 이 `auto` 인 row 와, 실제 provider/model

> **참고:**
> * `demo_reset.sh` 는 환경변수 `GRID_API_KEY`, `GEMINI_API_KEY` 를 자동 등록합니다. 두 개 이상 있으면 Model routing 테이블에 각 provider 의 모델별 통계가 뜹니다.
> * concrete model (`gemini-2.5-flash`) 을 지정하면 라우터를 **거치지 않습니다**. `auto` 또는 custom alias (`fast` 등) 만 라우팅 대상입니다.
> * provider key 가 하나뿐이면 `auto` 도 그 모델만 선택합니다 — 라우팅 **로직**은 동일하게 동작하지만 볼게 한 줄로 줄어듭니다.
> * LLM-as-judge eval 은 기본값에서 꺼져 있을 수 있습니다. heuristics 만으로도 routing signal 이 쌓입니다.

---

## 10. 마무리 (≈ 4:50–5:15)

Overview 페이지 상단으로 스크롤, 카드들 보여주며:

> **내레이션:**
> "이게 Nexus 입니다 — 한 명령으로 설치, 5분이면 deploy, trace·cache·guardrail,
> eval 기반 **auto** 라우팅까지 실시간으로 확인할 수 있습니다. 소스는 Apache 2.0,
> 대시보드는 MIT 라이선스입니다. 시청해 주셔서 감사합니다."

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
* **Guardrail 섹션 생략.** ≈ 50초 절약; cache + auto routing 만으로도 충분히 인상적.
* **Auto routing 섹션 생략.** provider key 하나·시간 부족할 때. cache + guardrail 만 녹화해도 됨.
* **큰 화면 사용.** `localhost:5173` 는 4K 까지 반응형이지만, 1440×900 에서 좌우 8개 카드가 가장 깔끔.

---

## 한국어 스크립트 노트

* 위 대본을 그대로 읽으면 자연스러운 한국어 나레이션이 됩니다.
* 영어 발음 발화 (예: "OpenAI", "ClickHouse", "BYOK") 는 한 번 원어 그대로 발음하고 한국어 설명을 곁들이면 좋습니다.
* "ClickHouse" 는 발음 그대로 "클릭하우스" 와 "ClickHouse" 둘 다 자연스러움.
* "BYOK" 는 "Bring Your Own Key" 풀어 설명 후 "BYOK" 를 언급하는 게 시청자 입장에서 명료.

## 다음 단계

이 문서 (`docs/demo-script.md`) 와 영어판은 같은 line-by-line 단계로 동기화되어 있으므로, 같은 데모에서 영문/한글 내레이션을 혼용하던, 바꿔서 쓸 수 있습니다.
