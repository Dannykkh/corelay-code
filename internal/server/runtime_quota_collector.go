package server

import (
	"context"
	"log"
	"time"

	"github.com/Dannykkh/corelay-code/internal/config"
	"github.com/Dannykkh/corelay-code/internal/runtimeplane"
)

func (s *Server) StartRuntimeQuotaCollectors(sources []config.RuntimeQuotaSource) int {
	mapped := runtimeQuotaSourcesFromConfig(sources)
	if len(mapped) == 0 {
		return 0
	}

	s.mu.RLock()
	telemetry := s.runtimeTelemetry
	s.mu.RUnlock()
	if telemetry == nil {
		return 0
	}

	collector := runtimeplane.NewQuotaCollector(telemetry, mapped)
	count := collector.EnabledSourceCount()
	if count == 0 {
		return 0
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	if s.runtimeQuotaStop != nil {
		s.runtimeQuotaStop()
	}
	s.runtimeQuota = collector
	s.runtimeQuotaStop = cancel
	s.mu.Unlock()

	collector.Start(ctx, log.Printf)
	return count
}

func (s *Server) SetRuntimeQuotaCollectors(sources []config.RuntimeQuotaSource) int {
	s.StopRuntimeQuotaCollectors()
	return s.StartRuntimeQuotaCollectors(sources)
}

func (s *Server) StopRuntimeQuotaCollectors() {
	s.mu.Lock()
	stop := s.runtimeQuotaStop
	s.runtimeQuotaStop = nil
	s.runtimeQuota = nil
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
}

func (s *Server) RuntimeQuotaCollectorCount() int {
	s.mu.RLock()
	collector := s.runtimeQuota
	s.mu.RUnlock()
	if collector == nil {
		return 0
	}
	return collector.EnabledSourceCount()
}

func runtimeQuotaSourcesFromConfig(sources []config.RuntimeQuotaSource) []runtimeplane.QuotaSource {
	out := make([]runtimeplane.QuotaSource, 0, len(sources))
	for _, source := range sources {
		if source.Disabled {
			continue
		}
		mapped := runtimeplane.QuotaSource{
			Name:     source.Name,
			Type:     source.Type,
			Path:     source.Path,
			URL:      source.URL,
			Headers:  source.Headers,
			Disabled: source.Disabled,
		}
		if source.IntervalSeconds > 0 {
			mapped.Interval = time.Duration(source.IntervalSeconds) * time.Second
		}
		if source.TimeoutSeconds > 0 {
			mapped.Timeout = time.Duration(source.TimeoutSeconds) * time.Second
		}
		out = append(out, mapped)
	}
	return out
}
