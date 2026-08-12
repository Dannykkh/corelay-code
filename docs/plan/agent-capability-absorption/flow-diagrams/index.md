# Agent Capability Absorption Flow Diagrams

이 디렉터리의 diagram은 target architecture의 행동 계약을 표현한다.
구현 class diagram이 아니며 provider별 별도 loop를 정의하지 않는다.

| Diagram | 목적 | 규모 |
|---|---|---|
| [agent-kernel.mmd](agent-kernel.mmd) | 모든 ingress가 RunMode, context/catalog, safety, completion, terminal finalizer를 공유하는 단일 kernel 흐름 | 20 nodes |
| [tool-execution-safety.mmd](tool-execution-safety.mmd) | serial과 parallel이 공유하는 permission, approval, sandbox, durable pre-execution journal 흐름 | 23 nodes |
| [durable-session-protocol.mmd](durable-session-protocol.mmd) | HTTP/ACP create·resume·fork와 write-ahead interruption, atomic terminal commit, hard-exit reconciliation protocol | 8 participants |
| [capability-profiler.mmd](capability-profiler.mmd) | isolated empirical probe와 profile quarantine 흐름 | 19 nodes |

## 공통 해석 규칙

- 모든 decision edge는 happy path와 error or blocked path를 함께 표시한다.
- Typed failure는 조용한 성공으로 변환되지 않는다.
- durable session ID와 active run ID는 다르다.
- tool concurrency 판정은 permission과 isolation 준비 이후에만 수행한다.
- authorized tool은 synchronous durable marker가 성공한 뒤에만 start event와 executor로 이동한다.
- successful terminal은 transcript·terminal·marker clear를 하나의 exact CAS로 커밋한다.
- profile은 한 run 동안 immutable하다.
- diagram의 source inspiration은 spec.md의 clean-room provenance 규칙을
  따른다.

## 검증 기준

1. 각 .mmd 파일 첫 non-empty line이 Mermaid diagram type이다.
2. flowchart node ID는 파일 안에서 유일하다.
3. 모든 decision node는 두 개 이상의 label edge를 가진다.
4. sequenceDiagram의 alt, else, opt, loop block은 end로 닫힌다.
5. 링크 대상 파일이 모두 존재한다.
