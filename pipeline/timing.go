package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"
)

// TimingEvent contains operational metadata only, never prompts or error text.
type TimingEvent struct {
	JobID       string          `json:"job_id"`
	SourceID    string          `json:"source_id,omitempty"`
	VideoID     string          `json:"video_id,omitempty"`
	Stage       string          `json:"stage"`
	StartedAt   time.Time       `json:"started_at"`
	FinishedAt  time.Time       `json:"finished_at"`
	DurationMS  int64           `json:"duration_ms"`
	Outcome     string          `json:"outcome"`
	Reused      bool            `json:"reused"`
	WindowStart *float64        `json:"window_start,omitempty"`
	WindowEnd   *float64        `json:"window_end,omitempty"`
	Fingerprint *TimingIdentity `json:"fingerprint,omitempty"`
	Usage       *ProviderUsage  `json:"usage,omitempty"`
}

type ProviderUsage struct {
	PromptTokens     int      `json:"prompt_tokens"`
	CompletionTokens int      `json:"completion_tokens"`
	ReasoningTokens  int      `json:"reasoning_tokens"`
	CachedTokens     int      `json:"cached_tokens"`
	Cost             *float64 `json:"cost,omitempty"`
	Model            string   `json:"model,omitempty"`
	Provider         string   `json:"provider,omitempty"`
}

type TimingIdentity struct {
	ConfigHash string `json:"config_sha256"`
	ModelHash  string `json:"model_sha256,omitempty"`
	Build      string `json:"build"`
}

// TimingFingerprint hashes only settings relevant to performance and quality.
// Paths, API key names, credentials, source data and prompts are excluded.
func TimingFingerprint(cfg Config) TimingIdentity {
	settings := struct {
		Clip                                                       Clip
		Provider, Model, Language, SelectorProvider, SelectorModel string
		Candidates                                                 int
		SelectionMode                                              string
		TargetCuts                                                 int
		MinScore                                                   float32
		PromptFormat                                               string
		WindowSeconds                                              int
		ReasoningEffort                                            string
		MaxTokens                                                  int
		Concurrency                                                int
	}{cfg.Clip, cfg.Transcription.Provider, cfg.Transcription.Model, cfg.Transcription.Language, cfg.Selector.Provider, cfg.Selector.ChatModel, cfg.Selector.CandidatesPerEpisode, cfg.Selector.SelectionMode, cfg.Selector.TargetCuts, cfg.Selector.MinScore, cfg.Selector.PromptFormat, cfg.Selector.WindowSeconds, cfg.Selector.ReasoningEffort, cfg.Selector.MaxTokens, cfg.Selector.Concurrency}
	b, _ := json.Marshal(settings)
	sum := sha256.Sum256(b)
	id := TimingIdentity{ConfigHash: fmt.Sprintf("%x", sum), Build: "unknown"}
	if info, ok := debug.ReadBuildInfo(); ok {
		id.Build = info.GoVersion
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				id.Build += ":" + setting.Value
			}
		}
	}
	if f, err := os.Open(cfg.Transcription.WhisperModel); err == nil {
		h := sha256.New()
		if _, err := io.Copy(h, f); err == nil {
			id.ModelHash = fmt.Sprintf("%x", h.Sum(nil))
		}
		_ = f.Close()
	}
	return id
}

type TimingRecorder struct {
	path, jobID string
	mu          sync.Mutex
	now         func() time.Time
}

func NewTimingRecorder(path, jobID string) *TimingRecorder {
	return &TimingRecorder{path: path, jobID: jobID, now: time.Now}
}

type timingKey struct{}

func WithTiming(ctx context.Context, recorder *TimingRecorder) context.Context {
	return context.WithValue(ctx, timingKey{}, recorder)
}

// StartTiming measures elapsed monotonic time and appends one event on finish.
func StartTiming(ctx context.Context, event TimingEvent) func(error) {
	r, _ := ctx.Value(timingKey{}).(*TimingRecorder)
	if r == nil {
		return func(error) {}
	}
	started := r.now()
	return func(err error) {
		finished := r.now()
		event.JobID, event.StartedAt, event.FinishedAt = r.jobID, started.UTC(), finished.UTC()
		event.DurationMS = finished.Sub(started).Milliseconds()
		event.Outcome = "success"
		if err != nil {
			event.Outcome = "failed"
		}
		if err != nil && (ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			event.Outcome = "cancelled"
		}
		if err := r.append(event); err != nil {
			log.Printf("timing report unavailable: %v", err)
		}
	}
}

func (r *TimingRecorder) append(event TimingEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(event)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// WithProviderUsage observes each attempted provider call, including calls
// whose cost could not be determined. The callback must be concurrency safe.
func WithProviderUsage(ctx context.Context, fn func(*ProviderUsage)) context.Context {
	return context.WithValue(ctx, providerUsageKey{}, fn)
}

type providerUsageKey struct{}

func ReportProviderUsage(ctx context.Context, usage *ProviderUsage) {
	if fn, ok := ctx.Value(providerUsageKey{}).(func(*ProviderUsage)); ok {
		fn(usage)
	}
}
