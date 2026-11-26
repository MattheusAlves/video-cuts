package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTimingPersistsStageOutcomeAndReuse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.jsonl")
	r := NewTimingRecorder(path, "job-1")
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	r.now = func() time.Time { return start }
	ctx := WithTiming(context.Background(), r)
	finish := StartTiming(ctx, TimingEvent{Stage: "download", SourceID: "source", VideoID: "video", Reused: true})
	r.now = func() time.Time { return start.Add(2 * time.Second) }
	finish(nil)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var event TimingEvent
	if err := json.Unmarshal(b, &event); err != nil {
		t.Fatal(err)
	}
	if event.JobID != "job-1" || event.Stage != "download" || event.SourceID != "source" || event.VideoID != "video" || !event.Reused || event.Outcome != "success" || event.DurationMS != 2000 {
		t.Fatalf("unexpected event: %+v", event)
	}
	if strings.Contains(string(b), "secret transcript") {
		t.Fatal("sensitive content recorded")
	}
}

func TestTimingRecordsFailureAndCancellationWithoutErrorText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.jsonl")
	ctx := WithTiming(context.Background(), NewTimingRecorder(path, "job"))
	StartTiming(ctx, TimingEvent{Stage: "select"})(context.Canceled)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var event TimingEvent
	if err := json.Unmarshal(b, &event); err != nil {
		t.Fatal(err)
	}
	if event.Outcome != "cancelled" {
		t.Fatalf("outcome = %q", event.Outcome)
	}
}

func TestTimingFingerprintTracksModelAndConfigWithoutCredentials(t *testing.T) {
	model := filepath.Join(t.TempDir(), "model")
	if err := os.WriteFile(model, []byte("model one"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{}
	cfg.Transcription.WhisperModel = model
	cfg.Selector.ChatModel = "model-a"
	cfg.Selector.BaseURL = "https://secret-user:secret-password@example.test/v1?token=secret-token"
	first := TimingFingerprint(cfg)
	if first.ConfigHash == "" || first.ModelHash == "" || first.Build == "" {
		t.Fatalf("missing identity: %+v", first)
	}
	cfg.Selector.ChatModel = "model-b"
	second := TimingFingerprint(cfg)
	if first.ConfigHash == second.ConfigHash || first.ModelHash != second.ModelHash {
		t.Fatalf("config change not captured: %+v %+v", first, second)
	}
	if err := os.WriteFile(model, []byte("model two"), 0600); err != nil {
		t.Fatal(err)
	}
	third := TimingFingerprint(cfg)
	if second.ModelHash == third.ModelHash {
		t.Fatal("model change not captured")
	}
	b, _ := json.Marshal(third)
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), model) {
		t.Fatalf("sensitive data leaked: %s", b)
	}
}
