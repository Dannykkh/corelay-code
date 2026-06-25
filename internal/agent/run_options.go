package agent

type RunOptions struct {
	ResponseLang      string
	WorkstreamContext string
	Recorder          RunRecorder
}

type RunRecorder interface {
	RunStarted()
	ReceiptWritten(path string, receipt AgentReceipt)
	RunCompleted(summary RunSummary)
}

type RunSummary struct {
	Provider     string
	Model        string
	ProjectType  string
	PlanMode     bool
	Iterations   int
	EditedFiles  []string
	Verification ReceiptVerification
	ReceiptPath  string
}
