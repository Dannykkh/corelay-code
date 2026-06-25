export type Lang = 'ko' | 'en';

const translations = {
  // ── Sidebar ──
  'nav.chat': { ko: '채팅', en: 'Chat' },
  'nav.settings': { ko: '설정', en: 'Settings' },
  'nav.routes': { ko: '라우팅 규칙', en: 'Routes' },
  'nav.costs': { ko: '비용', en: 'Costs' },
  'nav.kairos': { ko: 'KAIROS', en: 'KAIROS' },
  'nav.workspace': { ko: '워크스페이스', en: 'Workspace' },
  'nav.explorer': { ko: '파일 탐색기', en: 'Explorer' },
  'nav.memory': { ko: '메모리', en: 'Memory' },
  'nav.team': { ko: '팀', en: 'Team' },

  // ── Sidebar status ──
  'status.online': { ko: '온라인', en: 'Online' },
  'status.offline': { ko: '오프라인', en: 'Offline' },
  'status.routerOn': { ko: '라우터 ON', en: 'Router ON' },
  'status.singleModel': { ko: '단일 모델', en: 'Single model' },

  // ── Chat ──
  'chat.title': { ko: '코딩 에이전트', en: 'Coding Agent' },
  'chat.turns': { ko: '턴', en: 'turns' },
  'chat.welcome': { ko: 'AniClew 코딩 에이전트', en: 'AniClew Coding Agent' },
  'chat.welcomeSub': { ko: '파일 읽기, 코드 작성, 명령 실행 — 코딩 에이전트.', en: 'Reads files, writes code, runs commands — your coding agent.' },
  'chat.tools': { ko: '도구: Bash, Read, Write, Edit, Glob, Grep', en: 'Tools: Bash, Read, Write, Edit, Glob, Grep' },
  'chat.placeholder': { ko: 'AniClew에게 코딩 요청... (Enter로 전송)', en: 'Ask AniClew to code... (Enter to send)' },
  'chat.send': { ko: '전송', en: 'Send' },
  'chat.generating': { ko: '생성 중...', en: 'Generating...' },
  'chat.thinking': { ko: '생각 중...', en: 'Thinking...' },
  'chat.executing': { ko: '실행 중...', en: 'Executing...' },
  'chat.input': { ko: '입력', en: 'Input' },
  'chat.output': { ko: '출력', en: 'Output' },

  // ── Settings ──
  'settings.title': { ko: '설정', en: 'Settings' },
  'settings.defaultProvider': { ko: '기본 프로바이더 / 모델', en: 'Default Provider / Model' },
  'settings.current': { ko: '현재', en: 'Current' },
  'settings.provider': { ko: '프로바이더', en: 'Provider' },
  'settings.model': { ko: '모델', en: 'Model' },
  'settings.apply': { ko: '변경 적용', en: 'Apply Changes' },
  'settings.applied': { ko: '적용됨!', en: 'Applied!' },
  'settings.options': { ko: '옵션', en: 'Options' },
  'settings.smartRouter': { ko: '스마트 라우터', en: 'Smart Router' },
  'settings.smartRouterDesc': { ko: '작업 유형별 자동 라우팅', en: 'Auto-route requests by task role' },
  'settings.authPass': { ko: '인증 패스스루', en: 'Auth Passthrough' },
  'settings.authPassDesc': { ko: 'CLI OAuth 토큰 전달', en: 'Forward CLI OAuth tokens' },
  'settings.language': { ko: '언어 / Language', en: 'Language / 언어' },

  // ── Routes ──
  'routes.title': { ko: '라우팅 규칙', en: 'Routing Rules' },
  'routes.desc': { ko: '요청 내용에 따라 자동으로 Role이 선택되고, 지정된 모델로 라우팅됩니다.', en: 'Requests are auto-classified by role and routed to the assigned model.' },
  'routes.role': { ko: 'Role / 설명', en: 'Role / Description' },
  'routes.provider': { ko: '프로바이더', en: 'Provider' },
  'routes.model': { ko: '모델', en: 'Model' },
  'routes.fallback': { ko: '폴백', en: 'Fallback' },

  // ── Costs ──
  'costs.title': { ko: '비용 분석', en: 'Cost Breakdown' },
  'costs.total': { ko: '총 비용', en: 'Total Cost' },
  'costs.requests': { ko: '요청 수', en: 'Requests' },
  'costs.modelsUsed': { ko: '사용 모델', en: 'Models Used' },
  'costs.noData': { ko: '아직 요청이 없습니다', en: 'No requests yet' },
  'costs.share': { ko: '비중', en: 'Share' },
  'costs.tokens': { ko: '토큰', en: 'Tokens' },

  // ── KAIROS ──
  'kairos.title': { ko: 'KAIROS 데몬', en: 'KAIROS Daemon' },
  'kairos.start': { ko: '시작', en: 'Start' },
  'kairos.stop': { ko: '중지', en: 'Stop' },
  'kairos.state': { ko: '상태', en: 'State' },
  'kairos.autonomy': { ko: '자율성', en: 'Autonomy' },
  'kairos.tasks': { ko: '태스크', en: 'Tasks' },
  'kairos.tickInterval': { ko: '틱 간격', en: 'Tick Interval' },
  'kairos.autonomyMode': { ko: '자율성 모드', en: 'Autonomy Mode' },
  'kairos.collaborative': { ko: '협업', en: 'Collaborative' },
  'kairos.autonomous': { ko: '자율', en: 'Autonomous' },
  'kairos.night': { ko: '야간', en: 'Night' },
  'kairos.addTask': { ko: '백그라운드 태스크 추가', en: 'Add Background Task' },
  'kairos.activeTasks': { ko: '활성 태스크', en: 'Active Tasks' },
  'kairos.logs': { ko: '데몬 로그', en: 'Daemon Logs' },
  'kairos.noLogs': { ko: '로그가 없습니다', en: 'No logs yet' },

  // ── Memory ──
  'memory.title': { ko: 'AutoDream 메모리', en: 'AutoDream Memory' },
  'memory.dream': { ko: 'Dream 실행', en: 'Run Dream Cycle' },
  'memory.entries': { ko: '항목 수', en: 'Entries' },
  'memory.size': { ko: '크기', en: 'Size' },
  'memory.sessions': { ko: '세션', en: 'Sessions' },
  'memory.search': { ko: '메모리 검색...', en: 'Search memory...' },
  'memory.addMemory': { ko: '메모리 추가', en: 'Add Memory' },
  'memory.key': { ko: '키', en: 'Key' },
  'memory.value': { ko: '값', en: 'Value' },
  'memory.category': { ko: '분류', en: 'Category' },
  'memory.source': { ko: '소스', en: 'Source' },

  // ── Sessions ──
  'session.history': { ko: '대화 기록', en: 'Chat History' },
  'session.new': { ko: '새 대화', en: 'New Chat' },
  'session.noHistory': { ko: '대화 기록 없음', en: 'No chat history' },
  'session.delete': { ko: '삭제', en: 'Delete' },
  'session.turns': { ko: '턴', en: 'turns' },
  'session.autoSaved': { ko: '자동 저장됨', en: 'Auto-saved' },

  // ── Team ──
  'team.title': { ko: '팀 게이트웨이', en: 'Team Gateway' },
  'team.addMember': { ko: '팀원 추가', en: 'Add Team Member' },
  'team.name': { ko: '이름', en: 'Name' },
  'team.role': { ko: '역할', en: 'Role' },
  'team.budget': { ko: '예산', en: 'Budget' },
  'team.spent': { ko: '사용액', en: 'Spent' },
  'team.token': { ko: '토큰', en: 'Token' },
  'team.audit': { ko: '감사 로그', en: 'Audit Log' },
  'team.noAudit': { ko: '감사 기록 없음', en: 'No audit entries yet' },
  'team.noUsers': { ko: '사용자 없음', en: 'No users yet' },
} as const;

export type TransKey = keyof typeof translations;

let currentLang: Lang = (typeof localStorage !== 'undefined' && localStorage.getItem('kairos-lang') as Lang) || 'ko';

export function getLang(): Lang {
  return currentLang;
}

export function setLang(lang: Lang) {
  currentLang = lang;
  if (typeof localStorage !== 'undefined') {
    localStorage.setItem('kairos-lang', lang);
  }
}

export function t(key: TransKey): string {
  const entry = translations[key];
  if (!entry) return key;
  return entry[currentLang] || entry['en'] || key;
}
