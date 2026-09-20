# S01 설정·인증·비밀값

상태: PASS
연결: F01,F02,F03,F04
선행: 없음

## 수정 시작점과 소유 범위

internal/config/config.go; internal/server/server.go(authMiddleware,corsMiddleware,provider/workspace/skill handlers); internal/agent/context.go; web/src/lib/api.ts; cmd/proxy HTTP client

경로는 시작점이다. 실제 production caller와 기존 테스트를 rg로 추적한다. 공통 파일은 plan.md 순차 소유 규칙을 따른다.

## 하위 단계

- A. LoadChecked/원자적 Update를 설계하고 손상 JSON, missing file, write failure를 구분한다. 기존 Load caller 전체를 목록화한다.

- B. cross-process 갱신 직렬화와 Windows 교체 실패 원본 보존을 구현한다. 모든 Save 오류 무시 caller를 교체한다.

- C. 최초 credential bootstrap, Origin allowlist, 인증 실패 시 차단, CLI/Web credential 전달을 함께 연결한다. 건강 확인은 비민감 응답만 허용한다.

- D. prompt용 프로젝트 설정은 allowlist로 추출한다. config 및 로그에 fake secret이 유출되지 않는지 검사한다.

## 완료 조건

Q01/Q02. 저장 실패 시 runtime과 파일이 모두 이전 값이며 성공 응답이 아니다. 손상 설정으로 인증이 없어지지 않는다. 정상 최초 설치 CLI/Web은 연결 가능.

## 회귀·제약

인증 API만 바꾸고 UI/CLI를 연결하지 않은 채 완료 금지. token URL 전파/실제 secret 출력 금지.

## 검증 기록

- A/B: go test ./internal/config — PASS. missing/corrupt 구분, callback 실패 원본 보존, 파일 권한, 별도 프로세스 10개 동시 갱신.
- C: go test ./internal/server ./cmd/proxy ./cmd/corelaycode-acp — PASS. missing/malformed config 차단, header/cookie 허용, query token 거절, cross-origin POST 거절, loopback browser bootstrap, DNS rebinding host 거절, CLI loopback 자격증명 전달.
- D: go test ./internal/agent -run TestSafeProjectSettings -v — PASS. 설정 allowlist 및 provider/env/hook/MCP secret 제외.
- 전체 제품: go test ./... — PASS. 최초 Git for Windows Bash 경로가 우선되지 않아 Windows Bash/Job Object 기존 테스트가 실패했다. Git 경로를 해당 테스트 프로세스 PATH에 우선한 재실행은 PASS.
- Web: npm run lint, npm run build — PASS. 기존 bundle size 경고만 출력.
- Windows: GOOS=windows GOARCH=amd64 go build ./internal/config ./internal/agent ./internal/server ./cmd/proxy ./cmd/corelaycode-acp — PASS.
- LoadChecked는 missing/malformed/I/O error를 구분. update callback은 OS별 file lock 하에 최신 설정을 읽고 temp+flush+atomic replace 수행. production config.Save 호출 제거.
- 초기 구현 시 bootstrap 브라우저 E2E는 미실행이었다. 아래 Argos 보완에서 설치된 Chromium E2E를 통과했다. 외부 TLS reverse proxy, Windows runtime ACL은 NOT RUN. 외부 Origin은 CORELAY_ALLOWED_ORIGINS 설정 필요.
- G1 독립 운영자 보안 리뷰는 구현 직후 수행할 것.

### Argos 후속 수정 — 2026-09-20

- A07: `POST /api/bootstrap/challenge` → JSON challenge 교환을 구현했다. 60초 만료, 최대 128개, Host/Origin 결합 및 mutex 단일 소비를 적용한다. 잘못된 body/만료/재사용은 config 저장 전에 거절하며 저장 실패한 challenge도 재사용되지 않는다.
- Web의 동시 재인증은 하나의 Promise를 공유한다. build/lint PASS, embedded 동기화 후 `go test ./internal/server -run 'BrowserBootstrap|S10Browser' -count=1` PASS. bootstrap/auth/CORS focused `-count=3`도 PASS.
- 독립 코드 검토에서 nonce 소비→config 갱신 순서, 요청 경계, UI 재인증 연결을 확인했다. 외부 TLS/OS ACL 전수 검증을 대신하지 않는다.
