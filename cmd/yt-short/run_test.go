package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	pl "github.com/mitnick/yt-short/pipeline"
)

type testRoundTrip func(*http.Request) (*http.Response, error)

func TestSimulatedJobRecordsAllStages(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake executables require a POSIX shell")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0755); err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{
		"ffmpeg":  "#!/bin/sh\nfor arg do last=$arg; done\nprintf video > \"$last\"\n",
		"ffprobe": "#!/bin/sh\nprintf '%s' '{\"streams\":[{\"codec_name\":\"h264\",\"width\":1080,\"height\":1920},{\"codec_name\":\"aac\"}],\"format\":{\"duration\":\"50\"}}'\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_SELECTOR_KEY", "not-for-logging")
	previous := http.DefaultTransport
	defer func() { http.DefaultTransport = previous }()
	http.DefaultTransport = testRoundTrip(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		response := `{"clips":[{"start":0,"end":50,"score":0.8,"title":"title","hook_quote":"começo claro","attention_signals":["curiosity"],"virality_reason":"reason","story":{"setup_quote":"começo claro","development_quote":"algo mudou","conclusion_quote":"final chegou"}},{"start":70,"end":120,"score":0.7,"title":"second","hook_quote":"começo claro","attention_signals":["curiosity"],"virality_reason":"reason","story":{"setup_quote":"começo claro","development_quote":"algo mudou","conclusion_quote":"final chegou"}}]}`
		if strings.Contains(string(body), "Avalie cada corte") {
			response = `{"reviews":[{"index":0,"complete":true,"hook_strong":true},{"index":1,"complete":true,"hook_strong":true}]}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"usage":{"cost":0.001},"choices":[{"message":{"content":%q}}]}`, response)))}, nil
	})
	cfg := pl.Config{}
	cfg.Storage.SourcesDir, cfg.Storage.Transcripts, cfg.Storage.ClipsPending = filepath.Join(dir, "sources"), filepath.Join(dir, "transcripts"), filepath.Join(dir, "clips")
	cfg.Storage.StateFile, cfg.Storage.KeepSources = filepath.Join(dir, "state", "state.json"), true
	cfg.Selector.Provider, cfg.Selector.APIKeyEnv, cfg.Selector.BaseURL, cfg.Selector.CandidatesPerEpisode = "openai_compat", "TEST_SELECTOR_KEY", "https://example.invalid/v1", 5
	cfg.Clip.TargetSecondsMin, cfg.Clip.TargetSecondsMax = 45, 60
	cfg.Clip.Resolution, cfg.Clip.Framing = "1080x1920", "center_crop"
	if err := os.MkdirAll(cfg.Storage.SourcesDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".audio.m4a", ".mp4"} {
		if err := os.WriteFile(filepath.Join(cfg.Storage.SourcesDir, "s__v"+suffix), []byte("cached"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	words := []pl.Word{{Word: "começo", Start: 0, End: 1}, {Word: "claro", Start: 1, End: 2}, {Word: "algo", Start: 20, End: 21}, {Word: "mudou", Start: 21, End: 22}, {Word: "final", Start: 48, End: 49}, {Word: "chegou.", Start: 49, End: 50}}
	for _, w := range append([]pl.Word(nil), words...) {
		w.Start += 70
		w.End += 70
		words = append(words, w)
	}
	if _, err := pl.SaveTranscript(cfg, "s", pl.Transcript{VideoID: "v", DurationS: 120, Segments: []pl.Segment{{Start: 0, End: 120, Text: "começo claro algo mudou final chegou"}}, Words: words}); err != nil {
		t.Fatal(err)
	}
	state, err := pl.LoadState(cfg.Storage.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	s := &uiServer{cfg: cfg, state: state, ctx: context.Background(), jobs: map[string]jobRecord{"job": {ID: "job"}}}
	s.processJob = func(ctx context.Context, cfg pl.Config, st *pl.State, _ jobRequest) error {
		return processEpisode(ctx, cfg, st, pl.Episode{SourceID: "s", VideoID: "v", DurationS: 120})
	}
	s.runJob("job", jobRequest{Mode: "short", MinSeconds: 45, MaxSeconds: 60, Resolution: "1080x1920", Framing: "center_crop", TargetCuts: 1})
	if s.jobs["job"].Status != "done" {
		t.Fatalf("job failed: %s", s.jobs["job"].Error)
	}
	if s.jobs["job"].LLMCalls < 1 || s.jobs["job"].LLMCostUSD == nil || *s.jobs["job"].LLMCostUSD <= 0 || s.jobs["job"].LLMCostMissingCalls != 0 {
		t.Fatalf("provider cost not recorded: %+v", s.jobs["job"])
	}

	clips, err := pl.ListPending(cfg)
	if err != nil || len(clips) != 2 {
		t.Fatalf("expected two clips, got %d: %v", len(clips), err)
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(cfg.Storage.StateFile), "timings", "job.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	stages := map[string]pl.TimingEvent{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e pl.TimingEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		stages[e.Stage] = e
	}
	for _, stage := range []string{"download", "transcription", "selection_window", "review", "selection", "render", "finalize", "job"} {
		if e, ok := stages[stage]; !ok || e.Outcome != "success" {
			t.Errorf("missing successful %s: %+v", stage, e)
		}
	}
	if !stages["download"].Reused || !stages["transcription"].Reused || stages["job"].Fingerprint == nil {
		t.Fatalf("missing reuse or fingerprint: %s", b)
	}
	if strings.Contains(string(b), "not-for-logging") || strings.Contains(string(b), "começo claro") {
		t.Fatal("sensitive content in timings")
	}
	// Simulate interruption after publishing a clip but before publishing its sidecar.
	orphan := clips[0]
	if err := saveJSON(orphan.ClipPath+".rendering.json", orphan); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(strings.TrimSuffix(orphan.ClipPath, ".mp4") + ".json"); err != nil {
		t.Fatal(err)
	}
	s.jobs["retry"] = jobRecord{ID: "retry"}
	s.runJob("retry", jobRequest{Mode: "short", MinSeconds: 45, MaxSeconds: 60, Resolution: "1080x1920", Framing: "center_crop", TargetCuts: 1})
	if s.jobs["retry"].Status != "done" {
		t.Fatalf("retry failed: %s", s.jobs["retry"].Error)
	}
	clips, err = pl.ListPending(cfg)
	if err != nil || len(clips) != 2 {
		t.Fatalf("orphan sidecar not recovered: %d %v", len(clips), err)
	}
	b, err = os.ReadFile(filepath.Join(filepath.Dir(cfg.Storage.StateFile), "timings", "retry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), `"stage":"render"`) != 2 || strings.Count(string(b), `"stage":"render","started_at"`) != 2 {
		t.Fatalf("expected two render events: %s", b)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var event pl.TimingEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Stage == "render" && !event.Reused {
			t.Fatal("completed render was repeated")
		}
	}
	// A truncated pre-render marker from an interrupted older write is recoverable.
	victim := clips[0].ClipPath
	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(strings.TrimSuffix(victim, ".mp4") + ".json"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim+".rendering.json", []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	s.jobs["truncated"] = jobRecord{ID: "truncated"}
	s.runJob("truncated", jobRequest{Mode: "short", MinSeconds: 45, MaxSeconds: 60, Resolution: "1080x1920", Framing: "center_crop", TargetCuts: 1})
	if s.jobs["truncated"].Status != "done" {
		t.Fatalf("truncated marker retry: %s", s.jobs["truncated"].Error)
	}

}

func TestProcessEpisodeRecordsReusedInputsAndSelectionFailure(t *testing.T) {
	dir := t.TempDir()
	cfg := pl.Config{}
	cfg.Storage.SourcesDir = filepath.Join(dir, "sources")
	cfg.Storage.Transcripts = filepath.Join(dir, "transcripts")
	if err := os.MkdirAll(cfg.Storage.SourcesDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".audio.m4a", ".mp4"} {
		if err := os.WriteFile(filepath.Join(cfg.Storage.SourcesDir, "s__v"+suffix), []byte("cached"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := pl.SaveTranscript(cfg, "s", pl.Transcript{VideoID: "v", DurationS: 60, Segments: []pl.Segment{{Text: "example", Start: 0, End: 60}}, Words: []pl.Word{{Word: "example", Start: 0, End: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	state, err := pl.LoadState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "timing.jsonl")
	ctx := pl.WithTiming(context.Background(), pl.NewTimingRecorder(path, "job"))
	if err := processEpisode(ctx, cfg, state, pl.Episode{SourceID: "s", VideoID: "v"}); err == nil {
		t.Fatal("expected selector failure")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"stage":"download"`, `"stage":"transcription"`, `"reused":true`, `"stage":"selection"`, `"outcome":"failed"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %s in %s", want, b)
		}
	}
}

func (f testRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCheckOpenCodeAllowanceFailsBeforeProcessingWhenLimited(t *testing.T) {
	t.Setenv("YT_SHORT_TEST_KEY", "secret")
	previous := http.DefaultTransport
	defer func() { http.DefaultTransport = previous }()
	body := `{"usage":{"rolling":{"status":"rate-limited","resetsAt":"2026-11-25T04:37:36Z"},"weekly":{"status":"ok"}}}`
	http.DefaultTransport = testRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/inference/go/v1/usage" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("incorrect allowance request")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	if err := checkOpenCodeAllowance(context.Background(), "YT_SHORT_TEST_KEY"); err == nil || !strings.Contains(err.Error(), "rolling allowance exhausted") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("limited allowance: %v", err)
	}
	body = `{"usage":{"rolling":{"status":"ok"},"weekly":{"status":"ok"},"monthly":{"status":"ok"}}}`
	if err := checkOpenCodeAllowance(context.Background(), "YT_SHORT_TEST_KEY"); err != nil {
		t.Fatalf("available allowance: %v", err)
	}
}

func TestProviderAccountErrorsStopRemainingSources(t *testing.T) {
	for _, tc := range []struct {
		code int
		want bool
	}{{401, true}, {403, true}, {429, true}, {400, false}, {503, false}} {
		err := fmt.Errorf("window: %w", &pl.ProviderHTTPError{StatusCode: tc.code})
		if got := isProviderAccountError(err); got != tc.want {
			t.Errorf("status %d: got %v, want %v", tc.code, got, tc.want)
		}
	}
}

func TestPipelinePreflightRejectsMissingRequirements(t *testing.T) {
	model := filepath.Join(t.TempDir(), "model.bin")
	if err := os.WriteFile(model, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := pl.Config{}
	cfg.Transcription.Provider, cfg.Transcription.WhisperBin, cfg.Transcription.WhisperModel = "local", bin, model
	cfg.Selector.Provider, cfg.Selector.BaseURL, cfg.Selector.ChatModel, cfg.Selector.APIKeyEnv = "openai_compat", "https://example.invalid/v1", "model", "YT_SHORT_TEST_KEY"
	cfg.Clip.TargetSecondsMin, cfg.Clip.TargetSecondsMax = 45, 60
	t.Setenv("YT_SHORT_TEST_KEY", "secret")
	if err := pipelinePreflight(context.Background(), cfg); err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}
	t.Setenv("YT_SHORT_TEST_KEY", "")
	if err := pipelinePreflight(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("missing key: %v", err)
	}
	t.Setenv("YT_SHORT_TEST_KEY", "secret")
	cfg.Transcription.WhisperModel = filepath.Join(t.TempDir(), "missing")
	if err := pipelinePreflight(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "whisper model") {
		t.Fatalf("missing model: %v", err)
	}
}
