package router

type RoleID string

const (
	RoleBashOnly      RoleID = "bash-only"
	RoleFileRead      RoleID = "file-read"
	RoleFileEdit      RoleID = "file-edit"
	RoleMultiFileEdit RoleID = "multi-file-edit"
	RoleAgentSpawn    RoleID = "agent-spawn"
	RoleExplain       RoleID = "explain"
	RoleGenerate      RoleID = "generate"
	RoleRefactor      RoleID = "refactor"
	RoleDebug         RoleID = "debug"
	RoleReview        RoleID = "review"
	RoleTest          RoleID = "test"
	RoleCommit        RoleID = "commit"
	RoleShortCtx      RoleID = "short-context"
	RoleMediumCtx     RoleID = "medium-context"
	RoleLongCtx       RoleID = "long-context"
	RoleDefault       RoleID = "default"
)

type RouteRule struct {
	Role     RoleID  `json:"role"`
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	Fallback *Target `json:"fallback,omitempty"`
}

type Target struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type RouterConfig struct {
	Enabled           bool        `json:"enabled"`
	Rules             []RouteRule `json:"rules"`
	ContextThresholds struct {
		Short int `json:"short"`
		Long  int `json:"long"`
	} `json:"contextThresholds"`
	AutoEscalate bool `json:"autoEscalate"`
}

type RouteDecision struct {
	Role     RoleID `json:"role"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Reason   string `json:"reason"`
}

// UsageEntry counts traffic per provider/model. Deliberately no cost field:
// pricing was hardcoded and fell back to a flat $5/M for anything unknown,
// which meant local models accrued charges they never incurred.
type UsageEntry struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Requests int    `json:"requests"`
	Tokens   int    `json:"tokens"`
}

func DefaultConfig() RouterConfig {
	cfg := RouterConfig{
		Enabled:      true,
		AutoEscalate: true,
		Rules: []RouteRule{
			{Role: RoleBashOnly, Provider: "ollama", Model: "qwen3:8b"},
			{Role: RoleFileRead, Provider: "ollama", Model: "qwen3:8b"},
			{Role: RoleFileEdit, Provider: "openai", Model: "gpt-4o-mini"},
			{Role: RoleMultiFileEdit, Provider: "openai", Model: "gpt-4o"},
			{Role: RoleAgentSpawn, Provider: "openai", Model: "gpt-4o"},
			{Role: RoleExplain, Provider: "ollama", Model: "qwen3:8b"},
			{Role: RoleGenerate, Provider: "openai", Model: "gpt-4o"},
			{Role: RoleRefactor, Provider: "anthropic", Model: "claude-sonnet-4-20250514",
				Fallback: &Target{Provider: "anthropic", Model: "claude-opus-4-20250514"}},
			{Role: RoleDebug, Provider: "gemini", Model: "gemini-2.5-pro-preview-05-06"},
			{Role: RoleReview, Provider: "openai", Model: "gpt-4o-mini"},
			{Role: RoleTest, Provider: "openai", Model: "gpt-4o-mini"},
			{Role: RoleCommit, Provider: "ollama", Model: "qwen3:8b"},
			{Role: RoleShortCtx, Provider: "ollama", Model: "qwen3:8b"},
			{Role: RoleMediumCtx, Provider: "openai", Model: "gpt-4o-mini"},
			{Role: RoleLongCtx, Provider: "gemini", Model: "gemini-2.5-pro-preview-05-06"},
			{Role: RoleDefault, Provider: "openai", Model: "gpt-4o-mini"},
		},
	}
	cfg.ContextThresholds.Short = 2000
	cfg.ContextThresholds.Long = 50000
	return cfg
}
