# 구현 순서와 상태

정본 계약: [spec.md](spec.md). 검증: [validation.md](validation.md).

| 단계 | 상태 | 변경/산출물 | 통과 조건 |
|---|---|---|---|
| P0 연결 지점·계약 | DONE (문서) | 기존 guard/recorder/profiler 조사, 질문·비용·평가 계약 | 실제 경로 확인, 문서 링크·공백 검사 |
| P1 결정론적 관찰 기반 | DONE (로컬 검증) | shadow_judgment.go, RunOptions seam, loop 결과 집계 연결 | fake judge/실제 kernel 회귀, Linux race PASS |
| P2 기록·평가 fixture 연결 | DONE (로컬 검증) | 기존 recorder 영속 저장, corpus/label 형식, shadow-eval 오프라인 비교 | kernel→composite→trace 재개방, 관련 4개 suite/race PASS |
| P3 모델 비교 | PARTIAL (합성 smoke) | 명시 `shadow-judge` Ollama/Jev HTTP adapter, 토큰·지연·Jev 확률 기록 | 실제 reviewed corpus 전에는 품질 판정 보류 |
| P4 제한 개입 | DEFERRED | 질문 결과에 따른 기존 조사/전환 경로 연결 | 관찰 근거 확인 후 live paired 실험에서 가치 확인 |
| P5 개선 후보·채택 | DEFERRED | 정책 artifact identity/평가/명시 activation/복귀 | unseen 과제 검증, 잘못된 후보 비활성 유지 |
| P6 기록 재생 | DEFERRED | 관측 행동과 parent state가 연결된 replay world | unseen은 unsupported, 미래 근거 누출 없음, live 검증 별도 |

## 첫 구현 작업서 (P1–P2)

1. 제품 git status와 해당 파일 변경을 확인한다. 제품 명령은 corelay-code에서 실행한다.
2. 기존 RunGuard 결과와 dispatch 결과에서 run-local decision snapshot을 만든다. raw model transcript를 입력으로 재사용하지 않는다.
3. internal/agent의 작은 파일에 contract/validator/pure advisory mapping을 둔다. provider 호출은 주입 interface로 분리해 loop가 특정 SDK에 의존하지 않게 한다.
4. trigger는 결과 집계 지점 한 곳에서 만들고, async observer에 immutable snapshot을 전달한다. 기존 guard의 판단·reset을 호출해 변경하지 않는다.
5. off/default nil, saturated queue, invalid 응답, timeout, cancellation에서 기존 도구 호출·completion·receipt 계약이 보존되는지 검증한다.
6. agent가 observability/server를 import하지 않도록 기존 recorder 방향을 유지한다. optional typed judgment recorder → server adapter → 기존 trace 저장 경로로 연결한다.
7. cmd/corelaycode-profile의 실제 kernel 시험 하네스를 이용해 성공/정체 fixture를 평가한다. 기존 lesson run/improve의 게시 동작에 관찰 profile을 섞지 않는다.
8. P1/P2 증거가 생긴 뒤에만 문서 상태를 갱신한다. API key가 없는 경우 model comparison을 NOT RUN으로 남기고 deterministic 증거와 구분한다.

P1은 주입 가능한 관찰 기능의 구현 완료, P2는 기록과 시험 경로 연결 완료다. 둘만으로 사용자용 모델 설정이나 실제 효과 검증까지 완료한 것은 아니다.

## 현재 판정과 다음 행동

P0/P1/P2 완료. P3 모델 adapter와 합성 사례 호출·측정까지 완료했으며, 실제 사례에 대한 정확도·비용 비교는 남았다.
로컬 모델과 TypeSafe Jev에 합성 development 사례만 사용했다. Jev API는 확률 기록 보완 전후로 3건씩 총 6회 호출했고 실제 청구 금액은 확인하지 않았다. 전역 스킬 설치·실행 정책 채택은 하지 않았다.
사용자의 “성능은 괜찮았다”는 전제를 유지한다. 초기 실제 사례 10건은 진단 자료이며 성공·비개입 사례도 따로 포함한다.
기존 신뢰성 로드맵 S13 잔여 검증은 이 계획의 완료로 대체되지 않는다.

## P1 구현 증거 (2026-09-22)

- [관찰 구현](../../../internal/agent/shadow_judgment.go): run별 worker 1개, queue 1개, 최대 3개 기록, 요청 timeout/byte 상한, 명시 근거 준비와 strict typed response 검사.
- [회귀](../../../internal/agent/shadow_judgment_test.go): 정상 실행 judge 미호출; 실패 실행 off/shadow 도구 실행 여부·provider 호출 수·중단 보존; queue/dedup/budget; 취소/늦은 응답; 잘못된 enum/근거 참조; judge의 입력 변조; audit 원문 미포함.
- `go test ./internal/agent -run TestShadow -count=1`: PASS (최종 정상 실행·중복 JSON 거절 회귀 포함, 0.784s).
- `go test ./internal/agent -count=1`: PASS (286.227s). 이후 중복 JSON 거절 보완은 위 집중 회귀와 agent vet로 재검증했다.
- Docker Linux, Go 1.26.8: `go test -race ./internal/agent -run TestShadow -count=1`: PASS (1.089s).
- `go vet ./internal/agent`, `go build ./...`: exit 0.
- 문서 상대 링크·`git diff --check`: PASS. 새 파일 gofmt 적용.
- 실제 provider와 비용/품질 실험: NOT RUN. 기존 recorder를 확장 구현하지 않은 ingress에는 영속 판단 기록이 남지 않는다.

## P2 구현 (2026-09-23)

- [사용법·데이터 계약](../../shadow-evaluation.md): `shadow-eval --corpus ... --responses ... --split ...`.
- 기존 composite/observability recorder가 typed 판단을 검증해 trace span에 저장. 동일 trace의 종료·검증 결과 및 kernel run ID와 연결한다.
- 이후 반복 실패 카운터는 실제 관측만 기록한다. 사용자 개입과 성공 라벨은 corpus의 명시 검토값이며 자동 추정하지 않는다.
- 입력 변경 응답·중복·split 혼입·누락 검사를 포함하며, profile 게시/실행 정책 변경은 없다.
- 합성 사례 6개와 golden response는 스키마/실행 smoke용이다. 실제 성능 데이터로 사용할 수 없다.
- `go test ./internal/agent ./internal/server ./internal/observability ./cmd/corelaycode-profile -count=1`: PASS (160.869s / 30.202s / 0.165s / 12.393s).
- Linux Docker Go 1.26.8, 위 4개 패키지 `-race -run 'TestShadow|TestRecordRunChecked|TestTrackerRecordRun' -count=1`: PASS (1.133s / 1.064s / 1.040s / 1.045s).
- 위 4개 패키지 vet, `go build ./...`: exit 0. 이후 추가한 bundled corpus의 세 split 검사도 집중 CLI suite PASS (0.093s).
- `go run ./cmd/corelaycode-profile shadow-eval ... --split development`: exit 0, 합성 사례 3개 평가·누락 0. 실제 모델 정확도로 환산하지 않음.
- 문서 링크 및 `git diff --check`: PASS. 실제 provider/유료 API·실사용 라벨 수집·정책 채택: NOT RUN.

## P3 합성 smoke (2026-09-23)

명시적 `shadow-judge`로 Ollama `/api/chat`와 TypeSafe Jev `/v1/systemone`를 연결했다. 평가는 기존 `shadow-eval`의 request digest/응답 계약을 사용하며, 원래 agent 실행 정책에는 연결하지 않았다. Jev의 세 Noul은 한 호출에 묶고 0.2 이하 `no`, 0.8 이상 `yes`, 중간은 `unknown`으로 처리한다. Jev는 근거 ID를 선택해 돌려주지 않아 제공한 excerpt ID를 참조용으로만 기록한다. Jev 응답 파일에는 실제 확률과 서버가 반환한 모델 버전을 보존해 후속 보정 근거를 잃지 않는다. 이 임계값은 실제 데이터에서 아직 보정되지 않았다.

로컬 Ollama 합성 development 사례 3개 결과. 초깃값/고정 규칙의 advisory 일치도는 각각 1/3이다. 두 모델 모두 유효 응답 3/3, 기권 3/3, advisory 일치 1/3이다. 질문별 정확 일치는 Qwen3 0.6B가 3/9, Gemma4 12B가 5/9다. 합성 정답으로 만든 fixture이므로 이 숫자는 실제 성능이나 개선 효과가 아니다.

| 모델 | 입력/출력 토큰 | 총 호출 시간 | p50/p95 | 산출물 |
|---|---:|---:|---:|---|
| Qwen3 0.6B | 1404/309 | 1,528ms | 480/573ms | [responses](qwen3-0.6b-development-responses-v2.json) |
| Gemma4 12B | 1402/339 | 51,795ms | 3,940/44,089ms | [responses](gemma4-12b-development-responses.json) |
| Jev (`jev-1.13.0`) | 2317/189 | 854ms | 240/378ms | [responses](jev-development-responses-v2.json) |

Jev는 승인된 합성 development 사례 3건을 실호출했고 유효 응답 3/3, 기권 3/3, advisory 일치 1/3, 질문 일치 6/9였다. 첫 실호출은 1,387ms였지만 확률 저장 누락을 발견해 응답 계약을 보완한 뒤 재호출한 최종 산출물의 지연만 표에 적었다. Gemma4 p95에는 모델 적재가 포함되고 Qwen3 재측정은 이미 적재된 상태다. Jev는 원격 호출이고 tokenizer도 다르므로 이 숫자만으로 공정한 속도·토큰 효율 비교를 할 수 없다. Qwen3 첫 JSON-only 시도는 형식 오류 3/3이었고 JSON Schema를 지정한 뒤 유효 응답 3/3으로 바뀌었다. 이 수정은 development split에서만 했다. 실제 청구 금액·전력·GPU 경합은 측정하지 않았다. 세 모델의 합성 일치도는 실사용 품질 근거가 아니다.

로컬 run trace 저장소를 읽기 전용으로 확인한 결과 전체 21개(agent 17개, team 4개) 중 실패는 agent 1개와 team 3개였고 shadow judgment는 0개였다. 기존 trace에는 이 평가 계약의 전후 tool excerpt/독립 라벨이 없어 reviewed corpus를 추론하거나 자동 생성하지 않았다. 다음 작업은 실제 실행에서 승인된 excerpt와 독립 라벨을 수집한 뒤 새 selection/evaluation 과제로 비교하는 것이다. 그 전까지 P3 품질 판정과 P4 개입은 보류한다.
