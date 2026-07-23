## 요약
Dependabot이 보고한 **GHSA-5xrq-8626-4rwp / CVE-2026-47429 (CVSS 9.8, Critical)** 를 해결합니다. 우리 lockfile은 `vitest 2.1.9` 로 vuln 범위(`< 3.2.6`)에 들어가 있었습니다.

## 변경
* `web/package.json` — `vitest ^2.1.2` → `^3.2.6`, `vite ^5.4.11` → `^6.0.0`  (`@vitest/mocker` / `vite-node` transitive 호환)
* `web/package-lock.json` — 새 lockfile. 16 패키지 갱신.
  - 직접 의존성 vitest: `2.1.9` → **`3.2.7`** (CVE 패치됨)
  - build toolchain: Vite 5 → 6, React-qurey 같은 의존성 호환 확인
  - `esbuild` transitive도 `npm audit` 결과 **0 vulnerability**

## 왜 major upgrade 인지
- vitest 2.x 에 공식 security fix 없음. CVE patched range는 `>= 3.2.6` 또는 `>= 4.1.0`. 3.x 가 Node 호환성 / TypeScript 호환성이 가장 무난.
- Vite 5 → 6: vite 6 가 vitest 3 와 peer fit. 우리 코드는 Vite API 거의 표준만 사용.
- App Production code 영향 없음 (vitest 는 devDependency). 빌드 결과물 변경도 미미 (JS 280KB → 287KB).

## 위험 평가
- CVE PoC 가 **Windows 전용** 이고 우리는 macOS/Linux 만 사용 → 실제 노출 0.
- 그러나 vendor advisory 따라 lockfile 자체가 vuln 으로 남아 있으면 audit gate, GitHub security 탭에서 alert 가 떠나지 않음.
- 운영 Pod 자체에는 vitest 가 들어가지 않음(Go 바이너리가 frontend 정적 자산만 임베드).

## 검증
- `npm audit` → `found 0 vulnerabilities` (prod-only, dev 포함 둘 다)
- `tsc --noEmit` clean
- `vitest run` → **32/32 pass** (3.2.7 실행)
- `npm run build` → 정상 (vite 6.4.3)
- 변경 후 bundle: `index-Zps2aSWe.js` 287.56 kB / `index-DvJZ2-xD.css` 34.88 kB

## 호환성 노트
- Test side-effects 없음: `import { ... } from "vitest"` 만 사용, `projects` config 사용 안 함.
- Vite 6 의 `--build.rollupOptions.output.manualChunks` 는 그대로 작동.
- `vitest.config.ts` 의 `defineConfig({ plugins: [react()], test: {...} })` 그대로 호환.

✅ Closes: Dependabot vitest CVE-2026-47429 (alert #25)
