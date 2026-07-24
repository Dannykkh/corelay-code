package runtimeplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	QuotaSourceFile = "file"
	QuotaSourceHTTP = "http"

	defaultQuotaCollectorInterval = time.Minute
	defaultQuotaCollectorTimeout  = 5 * time.Second
)

type QuotaSource struct {
	Name     string
	Type     string
	Path     string
	URL      string
	Headers  map[string]string
	Interval time.Duration
	Timeout  time.Duration
	Disabled bool
}

type QuotaCollector struct {
	store   *TelemetryStore
	client  *http.Client
	sources []QuotaSource
}

type QuotaCollectReport struct {
	Source   string
	Accounts int
	Error    string
}

func NewQuotaCollector(store *TelemetryStore, sources []QuotaSource) *QuotaCollector {
	enabled := make([]QuotaSource, 0, len(sources))
	for _, source := range sources {
		normalized := normalizeQuotaSource(source)
		if normalized.Disabled {
			continue
		}
		enabled = append(enabled, normalized)
	}
	return &QuotaCollector{
		store:   store,
		client:  &http.Client{},
		sources: enabled,
	}
}

func (c *QuotaCollector) EnabledSourceCount() int {
	if c == nil {
		return 0
	}
	return len(c.sources)
}

func (c *QuotaCollector) CollectOnce(ctx context.Context, now time.Time) []QuotaCollectReport {
	if c == nil {
		return nil
	}
	reports := make([]QuotaCollectReport, 0, len(c.sources))
	for _, source := range c.sources {
		reports = append(reports, c.collectSource(ctx, source, now))
	}
	return reports
}

func (c *QuotaCollector) Start(ctx context.Context, logf func(format string, args ...any)) {
	if c == nil || len(c.sources) == 0 {
		return
	}
	for _, source := range c.sources {
		source := source
		go c.runSource(ctx, source, logf)
	}
}

func (c *QuotaCollector) runSource(ctx context.Context, source QuotaSource, logf func(format string, args ...any)) {
	collect := func() {
		report := c.collectSource(ctx, source, time.Now().UTC())
		if report.Error != "" && logf != nil {
			logf("runtime quota collector %s failed: %s", report.Source, report.Error)
		}
	}

	collect()
	ticker := time.NewTicker(source.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			collect()
		}
	}
}

func (c *QuotaCollector) collectSource(ctx context.Context, source QuotaSource, now time.Time) QuotaCollectReport {
	report := QuotaCollectReport{Source: source.Name}
	data, err := c.readSource(ctx, source)
	if err != nil {
		report.Error = err.Error()
		return report
	}

	accounts, err := ParseQuotaSnapshotPayload(data, now)
	if err != nil {
		report.Error = err.Error()
		return report
	}
	for _, account := range accounts {
		c.store.RecordAccountSnapshot(account, now)
	}
	report.Accounts = len(accounts)
	return report
}

func (c *QuotaCollector) readSource(ctx context.Context, source QuotaSource) ([]byte, error) {
	switch source.Type {
	case QuotaSourceFile:
		return os.ReadFile(source.Path)
	case QuotaSourceHTTP:
		reqCtx, cancel := context.WithTimeout(ctx, source.Timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, source.URL, nil)
		if err != nil {
			return nil, err
		}
		for key, value := range source.Headers {
			req.Header.Set(key, value)
		}
		resp, err := c.client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("source returned HTTP %d", resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	default:
		return nil, fmt.Errorf("unsupported quota source type %q", source.Type)
	}
}

func ParseQuotaSnapshotPayload(data []byte, now time.Time) ([]AccountState, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty quota snapshot")
	}

	var accounts []AccountState
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &accounts); err != nil {
			return nil, err
		}
		return normalizeQuotaAccounts(accounts, now)
	}

	var wrapper struct {
		Accounts []AccountState `json:"accounts"`
	}
	if err := json.Unmarshal(trimmed, &wrapper); err != nil {
		return nil, err
	}
	if len(wrapper.Accounts) > 0 {
		return normalizeQuotaAccounts(wrapper.Accounts, now)
	}

	var account AccountState
	if err := json.Unmarshal(trimmed, &account); err != nil {
		return nil, err
	}
	if account.Provider == "" {
		return nil, fmt.Errorf("quota snapshot requires provider")
	}
	return normalizeQuotaAccounts([]AccountState{account}, now)
}

func normalizeQuotaAccounts(accounts []AccountState, now time.Time) ([]AccountState, error) {
	out := make([]AccountState, 0, len(accounts))
	for _, account := range accounts {
		account.Provider = strings.TrimSpace(account.Provider)
		if account.Provider == "" {
			return nil, fmt.Errorf("quota snapshot requires provider")
		}
		if account.ID == "" {
			account.ID = "provider:" + account.Provider
		}
		if account.Group == "" {
			account.Group = ClassifyProviderGroup(account.Provider)
		}
		if account.Status == "" {
			account.Status = AccountHealthy
		}
		if account.ObservedAt.IsZero() {
			account.ObservedAt = now
		}
		out = append(out, account)
	}
	return out, nil
}

func normalizeQuotaSource(source QuotaSource) QuotaSource {
	source.Type = strings.ToLower(strings.TrimSpace(source.Type))
	if source.Name == "" {
		switch source.Type {
		case QuotaSourceFile:
			source.Name = source.Path
		case QuotaSourceHTTP:
			source.Name = source.URL
		default:
			source.Name = "quota-source"
		}
	}
	if source.Interval <= 0 {
		source.Interval = defaultQuotaCollectorInterval
	}
	if source.Timeout <= 0 {
		source.Timeout = defaultQuotaCollectorTimeout
	}
	return source
}
