package agent

type RunOptions struct {
	ResponseLang      string
	WorkstreamContext string
	Recorder          RunRecorder
	WorkerID          string
	OwnershipChecker  func(workerID, filePath string) (bool, string)
}

type RunRecorder interface {
	RunStarted()
	ReceiptWritten(path string, receipt AgentReceipt)
	RunCompleted(summary RunSummary)
}

type RunSpanRecorder interface {
	RunSpanStarted(id string, name string, data map[string]string)
	RunSpanCompleted(id string, status string, data map[string]string)
}

type RunFailureRecorder interface {
	RunFailed(message string)
}

func startRunSpan(recorder RunRecorder, id string, name string, data map[string]string) func(status string, data map[string]string) {
	spanRecorder, ok := recorder.(RunSpanRecorder)
	if !ok || spanRecorder == nil {
		return func(string, map[string]string) {}
	}
	spanRecorder.RunSpanStarted(id, name, data)
	return func(status string, data map[string]string) {
		spanRecorder.RunSpanCompleted(id, status, data)
	}
}

func failRun(recorder RunRecorder, message string) {
	failureRecorder, ok := recorder.(RunFailureRecorder)
	if ok && failureRecorder != nil {
		failureRecorder.RunFailed(message)
	}
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
