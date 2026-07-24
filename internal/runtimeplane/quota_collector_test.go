package runtimeplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseQuotaSnapshotPayloadAcceptsWrapperArrayAndSingleAccount(t *testing.T) {
	now := time.Date(2026, 7, 4, 2, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		body string
		want int
	}{
		{
			name: "wrapper",
			body: `{"accounts":[{"provider":"anthropic","group":"claude","fiveHour":{"used":8,"limit":10}}]}`,
			want: 1,
		},
		{
			name: "array",
			body: `[{"provider":"openai","group":"codex"},{"provider":"anthropic"}]`,
			want: 2,
		},
		{
			name: "single",
			body: `{"provider":"openai","group":"codex","sevenDay":{"used":20,"limit":100}}`,
			want: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			accounts, err := ParseQuotaSnapshotPayload([]byte(tc.body), now)
			if err != nil {
				t.Fatalf("parse quota snapshot: %v", err)
			}
			if len(accounts) != tc.want {
				t.Fatalf("accounts=%d want=%d", len(accounts), tc.want)
			}
			for _, account := range accounts {
				if account.Provider == "" || account.ID == "" || account.Status == "" || account.ObservedAt.IsZero() {
					t.Fatalf("account was not normalized: %+v", account)
				}
			}
		})
	}
}

func TestQuotaCollectorCollectsFileSnapshot(t *testing.T) {
	now := time.Date(2026, 7, 4, 2, 10, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "quota.json")
	if err := os.WriteFile(path, []byte(`{
		"accounts": [
			{
				"provider": "anthropic",
				"group": "claude",
				"fiveHour": {"used": 80, "limit": 100},
				"sevenDay": {"used": 200, "limit": 1000}
			}
		]
	}`), 0644); err != nil {
		t.Fatal(err)
	}

	store := NewTelemetryStore()
	collector := NewQuotaCollector(store, []QuotaSource{{
		Name: "local-file",
		Type: QuotaSourceFile,
		Path: path,
	}})

	reports := collector.CollectOnce(context.Background(), now)
	if len(reports) != 1 || reports[0].Error != "" || reports[0].Accounts != 1 {
		t.Fatalf("unexpected reports: %+v", reports)
	}

	accounts := store.Merge(nil, now)
	if len(accounts) != 1 {
		t.Fatalf("unexpected accounts: %+v", accounts)
	}
	if accounts[0].Provider != "anthropic" || accounts[0].FiveHour.Used != 80 || accounts[0].SevenDay.Limit != 1000 {
		t.Fatalf("snapshot not merged: %+v", accounts[0])
	}
}

func TestQuotaCollectorCollectsHTTPSnapshot(t *testing.T) {
	now := time.Date(2026, 7, 4, 2, 20, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Collector-Token") != "test-token" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"provider":"openai","group":"codex","fiveHour":{"used":2,"limit":10}}`))
	}))
	defer server.Close()

	store := NewTelemetryStore()
	collector := NewQuotaCollector(store, []QuotaSource{{
		Name:    "http-source",
		Type:    QuotaSourceHTTP,
		URL:     server.URL,
		Headers: map[string]string{"X-Collector-Token": "test-token"},
	}})

	reports := collector.CollectOnce(context.Background(), now)
	if len(reports) != 1 || reports[0].Error != "" || reports[0].Accounts != 1 {
		t.Fatalf("unexpected reports: %+v", reports)
	}

	accounts := store.Merge(nil, now)
	if len(accounts) != 1 || accounts[0].Provider != "openai" || accounts[0].FiveHour.Used != 2 {
		t.Fatalf("snapshot not merged: %+v", accounts)
	}
}

func TestQuotaCollectorReportsSourceErrors(t *testing.T) {
	collector := NewQuotaCollector(NewTelemetryStore(), []QuotaSource{{
		Name: "bad-file",
		Type: QuotaSourceFile,
		Path: filepath.Join(t.TempDir(), "missing.json"),
	}})

	reports := collector.CollectOnce(context.Background(), time.Now().UTC())
	if len(reports) != 1 || reports[0].Error == "" {
		t.Fatalf("expected source error, got %+v", reports)
	}
}
