package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pl "github.com/mitnick/yt-short/pipeline"
)

type testRoundTrip func(*http.Request) (*http.Response, error)

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
