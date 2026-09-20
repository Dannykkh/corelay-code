# S09 이미지 전달

상태: PASS
연결: F24
선행: S02,S03

## 수정 시작점과 소유 범위

web/src/pages/Chat.tsx; internal/types message blocks; internal/protocol/providers adapter; internal/agent/tools_advanced.go(ImageRead); internal/agent/sessions.go

경로는 시작점이다. 실제 production caller와 기존 테스트를 rg로 추적한다. 공통 파일은 plan.md 순차 소유 규칙을 따른다.

## 하위 단계

- A. [PASS] 기존 canonical image block을 엄격한 입력 계약으로 확장하고 provider/model별 effective capability와 MIME·byte·pixel 제한을 정의한다.

  - 기존 `ContentBlockParam.Source`를 canonical image owner로 유지한다. 각 image는 strict base64, 실제 포맷과 선언 MIME 일치, 양수 dimensions 검사를 통과해야 한다.
  - 허용 MIME: PNG, JPEG, GIF, WebP. 이미지당 4 MiB, 요청당 누적 5 MiB와 최대 8 image blocks, 각 변 8,192 px 이하와 24,000,000 pixels 이하. HTTP body 상한 8 MiB는 유지하며, canonical message content 상한을 body 상한으로 맞춰 유효한 4 MiB 이미지가 종전 4 MiB content 검사에 걸리지 않게 했다.
  - `types.ModelInfo.ImageInput`은 Corelay의 현재 전체 adapter 경로를 뜻하는 `supported` / `unsupported` / `unknown`(빈 값·생략) 계약이다. 기본 HTTPS OpenAI API endpoint의 확인된 모델(`gpt-5.5`, `gpt-4.1`, `gpt-4o`, `gpt-4o-mini`, `o4-mini`, `o3`)과 Anthropic 모델들을 지원으로 표시한다. 내장 OpenAI provider도 공식 API 호스트가 아닌 명시적 `BaseURL`에서는 known model capability를 unknown으로 낮춘다. Gemini 모델은 외부 API가 이미지 입력을 지원해도 현재 Corelay Part 변환이 이미지 블록을 거부하므로 이번 단계에서는 미지원이다. 나머지와 사용자 지정·미등록 모델은 unknown으로 닫는다. `openai-*` prefix로 OpenAI 호환 provider를 등록해도 임의 endpoint의 모델 capability를 보장할 수 없으므로 unknown으로 강등한다. `/api/providers`와 `/api/runtime`에 필드가 전달된다.
  - WebP `DecodeConfig`에 의존하는 `golang.org/x/image`는 Go 1.25 요구와 보안 수정이 반영된 v0.45.0으로 고정했다. CI 및 Docker build는 이미 Go 1.25를 사용하지만 `go.mod`와 README 요구 버전은 1.22였으므로 1.25로 맞췄다. 취약한 구버전에서 WebP를 처리하지 않도록 했다. 참고: [Go 취약점 GO-2026-4961](https://pkg.go.dev/vuln/GO-2026-4961), [GO-2026-5061](https://pkg.go.dev/vuln/GO-2026-5061), [x/image v0.45.0 go.mod](https://go.googlesource.com/image/+/refs/tags/v0.45.0/go.mod).
  - 회귀 재현: `go test ./internal/protocol -run 'TestAnthropicImageCanonical' -count=1` — 수정 전 FAIL (잘못된/빈 base64, MIME 불일치, image count·dimensions를 허용하고 4 MiB 초과 encoded message content를 가진 유효한 이미지를 거부).
  - 검증: PowerShell `$env:GOTOOLCHAIN = 'go1.25.0'; go test ./internal/protocol ./internal/translate ./internal/providers ./internal/server -count=1` — PASS, exit 0 (Windows/amd64). 같은 Go 1.25 환경에서 `go vet ./internal/protocol ./internal/translate ./internal/providers ./internal/server` — PASS, exit 0. `go mod tidy -diff`, `gofmt -d`, `git diff --check` — PASS, exit 0 및 차이/출력 없음.
  - 독립 리뷰 보완: custom `openai-*` provider와 내장 OpenAI의 비공식 `BaseURL`이 OpenAI 기본 모델 목록의 supported capability를 노출하던 경로를 unknown으로 낮추고 prefix·endpoint matrix 회귀 테스트를 추가했다. `$env:GOTOOLCHAIN = 'go1.25.0'; go test ./internal/providers ./internal/server -count=1` 및 `go vet ./internal/providers ./internal/server` — PASS, exit 0.
  - NOT RUN: 실제 외부 provider API 검증. 세션 blob 소유권/resume/fork와 미지원 모델 fallback은 C~D에서 검증한다.

- B. [PASS] upload/paste/ImageRead를 동일 검증/실제 image block 전달 경로로 연결하고 Base64 placeholder를 제거한다.

  - Chat upload/paste는 MIME을 보존하고 PNG/JPEG/GIF/WebP 및 이미지당 4 MiB를 사전 제한한다. UI preview는 실제 MIME을 쓴다. 요청에는 텍스트와 canonical base64 image block을 보내며 화면·durable session에는 표시용 첨부 라벨만 보관한다. 서버의 strict canonical 검증이 실제 형식/MIME, dimension, pixel, count, aggregate byte 한도를 다시 적용한다.
  - `/api/agent`는 bounded JSON decode로 8 MiB body를 제한하고 정확히 한 JSON 값만 허용한 뒤 canonical history/image를 provider 및 durable session 처리 전에 검증한다. 기존 `executionPolicy.runtimeCapabilities` 호환을 유지하기 위해 strict unknown-field decoder 대신 handler 수준 `http.MaxBytesReader`와 trailing-value 검사를 사용한다.
  - `ImageRead`는 regular file만 4 MiB 상한으로 읽고 실제 bytes를 공통 image validator에 통과시킨다. run-owned thread-safe side channel이 call ID로 canonical image를 전달하며, provider history에서는 `tool_result`와 별도 top-level `image` block으로 연결한다. bytes는 tool text/event/evidence/receipt/durable transcript에 넣지 않는다. 최초 요청 이미지와 ImageRead 이미지가 같은 8장/5 MiB run budget을 공유한다.
  - 회귀 재현: `go test ./internal/agent -run TestImageReadPassesValidatedImageBlockToNextProviderRequest -count=1` — 수정 전 FAIL (도구가 Base64 길이 placeholder만 내고 다음 provider request에 image block이 없음).
  - 검증 (Go 1.26.1, Windows/amd64): `go test ./internal/protocol -count=1` 및 `go test ./internal/server -count=1` — PASS, exit 0. 이미지 budget·ImageRead·전체 권한 파일 정책 회귀 테스트 — PASS. `go vet ./internal/protocol ./internal/agent ./internal/server` — PASS, exit 0.
  - 검증 (Web): `npm run lint` 및 `npm run build` — PASS, exit 0. 번들 경고는 기존 단일 JS chunk가 500 kB를 넘는다는 안내다. 빌드 결과를 embed 경로에 반영하고 `index.html`이 생성된 JS/CSS를 참조함을 확인했다. `git diff --check` — PASS, exit 0.
  - 전체 `go test ./internal/agent -count=1` — FAIL, exit 1: `TestRunChronosForwardsFullExecutionPolicyToKernel` (Chronos가 full-mode approval/write를 수행하지 않음). 동일 실패를 단독 실행으로 확인했다. 이미지 전달 경로 밖의 실행 모드 테스트라 이번 하위 단계 범위에서 수정하지 않았다. S09.B ImageRead 및 파일 정책 테스트는 통과한다.
  - 독립 코드 리뷰: S09.B 변경에서 수정 필수 결함 없음. Gemini provider는 현재 canonical image block 변환을 지원하지 않아 ImageRead 다음 provider 요청에서 실패할 수 있다. 명시적 사용자 오류/fallback은 D의 책임이다.
  - NOT RUN: 실제 외부 provider API, 실제 브라우저 upload/paste interaction, Q11 전체 acceptance. 세션 이미지 blob ownership/resume/fork는 C에서 검증한다.

- C. [PASS] durable session blob ownership/digest/reference로 보관하고 resume/fork에서 보존한다. base64를 receipt/log에 복사하지 않는다.

  - 기준 revision: 82d033bf4490033d782c97566877fed02536799e (이번 작업 시작 시 HEAD).
  - 세션 스키마를 v4로 올리고 user message에 digest, MIME, byte size 메타데이터만 저장한다. 원본 bytes는 세션별 해시 namespace의 검증된 blob으로 원자 저장하며 이미지당 4 MiB, 세션당 256 MiB, 세션당 512개 한도를 적용한다. v1~v3 세션은 읽을 수 있고 과거 세션에는 image ref를 소급 생성하지 않는다.
  - /api/agent는 마지막 사용자 턴의 canonical image를 세션 blob으로 저장하고 참조를 결합한다. 재개 시 이전 턴의 모든 참조를 소유 세션 namespace에서 다시 검증하고 bytes를 provider 입력으로 복원한다. transcript, SSE, receipt/log에는 base64를 넣지 않는다. 같은 image block 중복은 provider 요청에서 보존하되 blob은 digest 단위로 공유한다.
  - 전체 누적 요청 image budget과 canonical 입력 검증을 먼저 끝낸 뒤 revision을 CAS 저장한다. 초과 요청은 revision을 바꾸지 않고 이번 요청에서 생긴 미참조 blob을 정리하므로 기존 이력을 텍스트 재시도로 계속 쓸 수 있다. 저장된 blob 누락·손상·다른 세션 참조는 provider 호출 전 거절한다.
  - fork는 검증된 blob을 자식 세션의 독립 namespace로 복사하고, 세션 삭제는 소유 image/result blob을 함께 지우며 제거 통계를 반환한다.
  - 주요 변경 파일: internal/protocol/image_validation.go, internal/agent/sessions.go, internal/agent/session_memory.go, internal/agent/session_images_test.go, internal/server/agent_session.go, internal/server/workstream_plan_execution.go, internal/server/server.go, internal/server/agent_image_test.go, internal/server/session_lifecycle_api_test.go, web/src/lib/sessions.ts, web/src/pages/Chat.tsx, internal/server/webdist/index.html, 새 JS asset internal/server/webdist/assets/index-DscuSBUN.js.
  - 회귀 재현: production /api/agent 경로에서 첫 image 전송 뒤 새 SessionStore로 재개하면 과거 image bytes가 provider 입력에서 빠지는 것을 확인했다. 수정 후 이어가기에서 동일 bytes 복원, transcript/SSE base64 비노출, 손상 blob fail-closed, 중복 image 전달, fork namespace 독립성, 삭제 정리, v3→v4 migration 및 8장 이력 이후 9번째 image 거절·재시도 동작을 검사했다. 독립 리뷰에서 plan-bound 실행의 사전 stage 거절 시 staged blob 정리 누락과 image revision 저장 실패 뒤 stage가 running으로 남는 경로를 추가 확인하고, cleanup 및 failed/not-run stage 기록으로 닫았다.
  - 집중 검증: go test ./internal/agent ./internal/protocol ./internal/server -run 'SessionImage|AgentLoop.*Image|AgentLoopTransportsImageAndKeepsDurableTranscriptDisplaySafe|AgentLoopBindsAndCommitsDurableSessionBeforeDone|TestSessionLifecycle|TestSessionStoreFork|TestSessionAPI|MigratesVersionThreeBeforeImageSchema' -count=1 — PASS, exit 0. go vet ./internal/agent ./internal/protocol ./internal/server — PASS, exit 0. npm run lint, npm run build, git diff --check — PASS, exit 0. Build chunk >500 kB 경고는 유지된다.
  - plan-bound failure-path 검증: go test ./internal/server -run 'TestPlanImage(PreflightFailureCleansPreparedBlob|CommitFailureClosesStartedStageAsNotRun)$' -count=1 — PASS, exit 0. 실패 주입 시 image blob과 session revision이 정리되고 이미 시작된 Plan attempt는 not-run/incomplete로 닫힌다.
  - 당시 영향 패키지 검증: `go test ./internal/agent ./internal/protocol ./internal/server -count=1`은 Chronos full-mode fixture의 16K context overflow로 `internal/agent`가 실패했으며 이미지 경로 밖이었다. 이 historical failure는 S13에서 fixture harness를 32K로 조정한 뒤 전체 `go test ./... -count=1`과 roadmap QA entrypoint가 통과해 해소됐다.
  - NOT RUN: 실제 외부 provider API와 실제 브라우저에서의 upload/paste interaction. Q11 전체 acceptance는 미지원 모델 오류/fallback을 다루는 D까지 연결한 뒤 실행한다.

- D. [PASS] 이미지 입력을 지원하지 않거나 capability를 확인할 수 없는 모델에는 명시 오류를 내고, 자동 텍스트 대체 성공을 막는다.

  - model capability는 선택된 provider의 정확한 model ID에서 확인한다. missing/invalid metadata는 unknown으로 닫고, text-only 요청은 계속 허용한다. image가 포함된 `/api/agent` 요청은 durable image 준비와 plan stage 시작 전에 검사한다. 직접 RunLoop 호출도 context planning/compaction 전에 최초 image를 검사하고, provider 재시도·model fallback 직전 매 요청을 검사한다.
  - unsupported/unknown 모델에서 `ImageRead`를 실행하려는 경우 파일을 읽기 전에 중단한다. OpenAI-compatible protocol 경로도 첫 provider 호출, 내부 model retry, router의 cross-provider fallback 각각 직전에 검사한다. API는 422와 안정 코드 `image_input_unsupported` / `image_input_capability_unknown`, 조치 가능한 메시지를 반환한다.
  - 수정 파일: internal/agent/image_capability.go, internal/agent/loop.go, internal/agent/image_read_test.go, internal/server/server.go, internal/server/agent_image_test.go.
  - 회귀 검증 (Go 1.26.1, Windows/amd64): `$env:GOTOOLCHAIN = 'go1.26.1'; go test ./internal/agent ./internal/protocol ./internal/providers ./internal/server -count=1` — PASS, exit 0. agent 135.430s, protocol 1.257s, providers 5.098s, server 18.320s. unsupported/unknown/custom model, historical image, fallback, ImageRead 사전 거절, text-only 요청을 포함한다.
  - `go vet ./internal/agent ./internal/protocol ./internal/providers ./internal/server` — PASS, exit 0. `git diff --check` — PASS, exit 0.
  - 실제 브라우저 검증: 내장 Web UI를 Chrome 152.0.7977.77 headless + Rod로 열고 PNG upload→전송을 수행했다. 로컬 fake provider가 `image/png`, 77 bytes, image block 1개를 받았고 SHA-256 `302d727e5d93aba85c43facbef4446a23d9588db8cc47ef52bee3dfa332f092d`가 fixture와 일치했다. 임시 harness 실행 exit 0이며 테스트 후 임시 파일을 제거했다. ImageRead의 실제 PNG bytes 전달은 `TestImageReadPassesValidatedImageBlockToNextProviderRequest`와 전체 agent package suite에서 PASS다.
  - NOT RUN: 외부 유료/실제 provider API 호출, 브라우저 clipboard paste 상호작용. paste 구현은 UI 코드 경로에 연결돼 있으며 브라우저 상호작용 검증은 S10.E에 남긴다.

## 완료 조건

Q11. fake provider가 bytes를 검증; supported adapter의 실제 request shape 확인. 과대 이미지/잘못된 MIME/다른 세션 blob 거절. 이미지 포함 재개가 동일 입력.

## 회귀·제약

이미지 내용을 봤다고 주장하는 텍스트만 반환하는 구현 금지. 모든 provider가 vision 지원한다고 가정 금지.

## 검증 기록

하위 단계별 재현·수정·검사 결과와 exit code는 해당 단계 기록에 둔다. A~D와 Q11은 PASS다. 외부 provider API 및 browser clipboard paste는 NOT RUN이며 S09 완료를 막지 않는다.
