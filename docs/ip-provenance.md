# IP Provenance — Corelay Code

> 코드 출처 검증 기록. 2026-07-25 실시.

## 배경

Corelay Code(당시 명칭 AniClew, `proxy-go/`)는 2026년 4월부터 7월까지 Anthropic Claude Code의 유출 소스 트리와 같은 워크스페이스(`D:\git\claudecode`)에서 개발되었다. 유출 트리는 2026-03-31 npm 소스맵 사고로 노출된 독점 코드이며, 저장소 `LICENSE`가 `UNLICENSED — NOT FOR REDISTRIBUTION`으로 명시한다.

개발 과정에서 유출 트리를 **개념 수준으로 벤치마킹**했다(메모리 시스템, 하네스 구조, 설정 UI 구성 등). 저작권은 아이디어·방법·시스템이 아니라 표현을 보호하므로, 실제 리스크는 "참고 여부"가 아니라 "표현을 그대로 옮겼는가"에 있다. 이 문서는 그 점을 기계적으로 검증한 기록이다.

## 검증 방법

`proxy-go` 트리에서 길이 30자 이상의 문자열 리터럴을 전부 추출하고, 유출 트리(`src/` + 상위 `web/`, 2,432파일 / 34.4MB) 전체 텍스트에 동일 문자열이 존재하는지 대조했다.

문자열 리터럴 대조를 선택한 이유:

- 구현 언어가 다르면(Go ↔ TypeScript) 문자적 코드 복제는 성립하기 어렵지만, **문자열은 언어를 넘어 그대로 남는다**. 따라서 표현 복제의 가장 민감한 지표다.
- 정규화·의미 유사도가 아닌 완전 일치만 세므로 거짓 양성이 적다.

추가로 Claude Code 특유 식별자(`You are Claude Code`, `system-reminder`, `tengu`, `compactBoundary` 등)의 잔존 여부를 별도 탐침했다.

노이즈 제외: MIME 타입, URL, 헤더명, 파일 경로, 단일 소문자 토큰.

## 1차 결과 (수정 전)

| 대상 | 추출 문자열 | 유출본 일치 |
|------|-------------|-------------|
| `internal/**/*.go` (191파일) | 1,455 | **4** |
| `web/src/**/*.ts(x)` (23파일) | 269 | **4** |
| Claude Code 지문 (코드 내) | — | **0** |

### 일치 항목 분류

| 문자열 | 위치 | 판정 |
|--------|------|------|
| `. Only part of it was loaded. Keep index entries to one line ` | `internal/memory/entrypoint.go:89` | 창작적 문구 — **수정** |
| `under ~200 chars; move detail into topic files.` | `internal/memory/entrypoint.go:90` | 창작적 문구 — **수정** |
| `integration tests must hit a real database` | `internal/memory/relevance_test.go:38` | 테스트 픽스처 — **수정** |
| `No verification evidence recorded` | `internal/server/evidence_api.go:282` | 일반적 에러 문구, 경계선 — **수정** |
| `bg-{orange,purple,yellow,green}-500/15 text-*-400` (4건) | `web/src/pages/Routes.tsx` | Tailwind 유틸리티 클래스. 표현 방식이 사실상 유일하므로 저작권 보호 대상 아님 — **유지** |

지문 탐침의 유일한 히트(`tengu`)는 코드가 아니라 대화 로그 `conversations/2026-04-06-claude.md`이며 빌드 산출물과 무관하다.

## 조치

Go 측 4건을 전부 재작성했다. `entrypoint.go`는 테스트가 검증하는 `WARNING` 접두사와 `truncationReason()` 반환값은 보존하고 뒤따르는 안내 문장만 교체했다.

```
internal/memory/entrypoint.go:88-90     경고 안내문 재작성
internal/memory/relevance_test.go:38    픽스처 문구 교체 (매칭 토큰 integration/database 유지)
internal/server/evidence_api.go:282     에러 문구 교체
```

## 2차 결과 (수정 후)

| 대상 | 유출본 일치 |
|------|-------------|
| `internal/**/*.go` | **0** |
| `web/src/**/*.ts(x)` | 4 (Tailwind 클래스만) |
| Claude Code 지문 | **0** |

검증: `go build ./...` / `go vet ./...` 통과, `go test ./...` 15개 패키지 전부 `ok`, 실패 0.

## 남는 리스크와 한계

정직하게 기록한다.

- **접근(access)이 증명된 상태다.** 유출 트리와 같은 워크스페이스에서 개발했고 git 이력에 남아 있다. clean room이 아니므로, 향후 유사성이 주장될 경우 "독립 창작" 방어는 쓸 수 없다.
- **이 검사는 문자열 표현만 본다.** 구조·순서·조직(SSO) 차원의 비문자적 유사성은 자동 검출 대상이 아니다. 다만 구현 언어와 제품 기능이 모두 다르다는 점(Go 프록시/게이트웨이 ↔ TypeScript CLI 하네스)이 완화 요소다.
- **API 스키마 호환은 별개 쟁점이다.** Anthropic Messages API 형식을 따르는 것은 공개 문서 기반의 상호운용이며, Google v. Oracle(2021, 미 대법원)이 Java API 선언 코드의 fair use를 인정한 선례가 있다.
- **이 문서는 법률 자문이 아니다.** 상업화 전 IP 변호사 검토를 권한다.

## 재현

검사 스크립트는 유출 트리 경로를 입력으로 요구하므로, 저장소 분리 후에는 그대로 재현할 수 없다. 방법론은 위 "검증 방법" 절에 기술된 대로이며, 동일 절차를 임의의 참조 코퍼스에 대해 재실행할 수 있다.
