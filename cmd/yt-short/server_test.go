package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pl "github.com/mitnick/yt-short/pipeline"
)

func TestPrepareRequestAppliesModeDefaultsAndValidatesInputs(t *testing.T) {
	cfg := uiTestConfig(t)
	s := &uiServer{cfg: cfg}
	tests := []struct {
		name    string
		request jobRequest
		wantErr string
		want    jobRequest
	}{
		{
			name:    "short defaults",
			request: jobRequest{SourceID: "channel", VideoID: "video"},
			want:    jobRequest{SourceID: "channel", VideoID: "video", Mode: "short", MinSeconds: 45, MaxSeconds: 60, Resolution: "1080x1920", Framing: "center_crop"},
		},
		{
			name:    "regular video defaults",
			request: jobRequest{URL: "https://youtu.be/video", Mode: "video"},
			want:    jobRequest{URL: "https://youtu.be/video", Mode: "video", MinSeconds: 240, MaxSeconds: 1680, Resolution: "1920x1080", Framing: "center_crop"},
		},
		{name: "rejects non YouTube link", request: jobRequest{URL: "https://example.com/watch?v=video"}, wantErr: "YouTube"},
		{name: "rejects short at 180 seconds", request: jobRequest{SourceID: "channel", VideoID: "video", MaxSeconds: 180}, wantErr: "shorter than 180"},
		{name: "rejects regular video above 28 minutes", request: jobRequest{URL: "https://youtu.be/video", Mode: "video", MinSeconds: 240, MaxSeconds: 1681}, wantErr: "28 minutes"},
		{
			name:    "accepts regular video at 28 minutes",
			request: jobRequest{URL: "https://youtu.be/video", Mode: "video", MinSeconds: 240, MaxSeconds: 1680},
			want:    jobRequest{URL: "https://youtu.be/video", Mode: "video", MinSeconds: 240, MaxSeconds: 1680, Resolution: "1920x1080", Framing: "center_crop"},
		},
		{name: "rejects partial source selector", request: jobRequest{SourceID: "channel"}, wantErr: "either a YouTube URL"},
		{name: "rejects mixed selectors", request: jobRequest{URL: "https://youtu.be/video", SourceID: "channel", VideoID: "video"}, wantErr: "either a YouTube URL"},
		{name: "rejects invalid range", request: jobRequest{SourceID: "channel", VideoID: "video", MinSeconds: 70, MaxSeconds: 60}, wantErr: "duration range"},
		{name: "rejects unknown source", request: jobRequest{SourceID: "missing", VideoID: "video"}, wantErr: "unknown source"},
		{name: "rejects unknown framing", request: jobRequest{URL: "https://youtu.be/video", Framing: "stretch"}, wantErr: "framing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := tc.request
			err := s.prepareRequest(&request)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("prepareRequest() error = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("prepareRequest() error = %v", err)
			}
			if request != tc.want {
				t.Fatalf("normalized request = %#v, want %#v", request, tc.want)
			}
		})
	}
}

func TestLocalServerRejectsCrossOriginAndNonJSONJobRequests(t *testing.T) {
	handler := withLocalHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, tc := range []struct {
		name, method, origin, contentType string
		want                              int
	}{
		{"same origin", "POST", "http://127.0.0.1:8765", "application/json", http.StatusNoContent},
		{"vite development", "POST", "http://localhost:5173", "application/json", http.StatusNoContent},
		{"remote origin", "POST", "https://evil.example", "application/json", http.StatusForbidden},
		{"plain text form", "POST", "", "text/plain", http.StatusUnsupportedMediaType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "http://127.0.0.1:8765/api/jobs", nil)
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			r.Header.Set("Content-Type", tc.contentType)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestJobExposesProgressWhileRunning(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	s := &uiServer{cfg: uiTestConfig(t), jobs: map[string]jobRecord{}, ctx: context.Background()}
	s.processJob = func(ctx context.Context, _ pl.Config, _ *pl.State, _ jobRequest) error {
		pl.ReportProgress(ctx, pl.Progress{Stage: "select", Message: "Analyzing transcript window 2 of 4", Completed: 1, Total: 4})
		close(started)
		<-release
		return nil
	}
	defer close(release)
	s.jobs["job"] = jobRecord{ID: "job", Status: "queued"}
	go s.runJob("job", jobRequest{Mode: "short"})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("job did not start")
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/jobs/job", nil)
	r.SetPathValue("id", "job")
	s.handleJob(w, r)
	var job jobRecord
	if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if job.Status != "running" || job.Progress == nil || job.Progress.Stage != "select" || job.Progress.Completed != 1 || job.Progress.Total != 4 || job.StartedAt == nil {
		t.Fatalf("unexpected running progress: %#v", job)
	}
}

func TestUIHandlerServesEmbeddedStudioAndLocalAPI(t *testing.T) {
	cfg := uiTestConfig(t)
	cfg.Storage.ClipsPending = t.TempDir()
	state, err := pl.LoadState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{}, 1)
	var processedLongForm bool
	var processedResolution string
	var processedCaptions bool
	s := &uiServer{
		cfg:   cfg,
		state: state,
		jobs:  make(map[string]jobRecord),
		ctx:   context.Background(),
		processJob: func(_ context.Context, received pl.Config, _ *pl.State, _ jobRequest) error {
			processedLongForm = received.Clip.LongForm
			processedResolution = received.Clip.Resolution
			processedCaptions = received.Clip.Captions.Enabled
			finished <- struct{}{}
			return nil
		},
	}
	handler, err := newUIHandler(s)
	if err != nil {
		t.Fatalf("newUIHandler(): %v", err)
	}
	handler = withLocalHeaders(handler)

	root := httptest.NewRecorder()
	handler.ServeHTTP(root, httptest.NewRequest(http.MethodGet, "/", nil))
	if root.Code != http.StatusOK || !strings.Contains(root.Body.String(), "Cutroom") {
		t.Fatalf("GET / = %d %q, want embedded studio", root.Code, root.Body.String())
	}
	if root.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("security header missing: %v", root.Header())
	}

	configResponse := httptest.NewRecorder()
	handler.ServeHTTP(configResponse, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	var configPayload struct {
		Sources []struct {
			ID string `json:"id"`
		} `json:"sources"`
		Video map[string]int `json:"video"`
	}
	if err := json.Unmarshal(configResponse.Body.Bytes(), &configPayload); err != nil {
		t.Fatal(err)
	}
	if configResponse.Code != http.StatusOK || len(configPayload.Sources) != 1 || configPayload.Sources[0].ID != "channel" || configPayload.Video["max_seconds"] != 1680 {
		t.Fatalf("GET /api/config = %d %#v", configResponse.Code, configPayload)
	}
	clipsResponse := httptest.NewRecorder()
	handler.ServeHTTP(clipsResponse, httptest.NewRequest(http.MethodGet, "/api/clips", nil))
	if clipsResponse.Code != http.StatusOK || strings.TrimSpace(clipsResponse.Body.String()) != "[]" {
		t.Fatalf("empty GET /api/clips = %d %q", clipsResponse.Code, clipsResponse.Body.String())
	}

	// The actual channel listing boundary runs through ListRecentEpisodes and a
	// fake yt-dlp executable, so this does not need network access.
	binDir := t.TempDir()
	ytDLP := filepath.Join(binDir, "yt-dlp")
	output := `{"entries":[{"id":"listed-video","title":"A recent video","duration":712,"webpage_url":"https://www.youtube.com/watch?v=listed-video"}]}`
	if err := os.WriteFile(ytDLP, []byte("#!/bin/sh\nprintf '%s' '"+output+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	videoResponse := httptest.NewRecorder()
	handler.ServeHTTP(videoResponse, httptest.NewRequest(http.MethodGet, "/api/videos?source=channel", nil))
	var episodes []pl.Episode
	if err := json.Unmarshal(videoResponse.Body.Bytes(), &episodes); err != nil {
		t.Fatal(err)
	}
	if videoResponse.Code != http.StatusOK || len(episodes) != 1 || episodes[0].VideoID != "listed-video" || episodes[0].DurationS != 712 {
		t.Fatalf("GET /api/videos = %d %#v (%s)", videoResponse.Code, episodes, videoResponse.Body.String())
	}
	unknownSource := httptest.NewRecorder()
	handler.ServeHTTP(unknownSource, httptest.NewRequest(http.MethodGet, "/api/videos?source=missing", nil))
	if unknownSource.Code != http.StatusBadRequest {
		t.Fatalf("unknown source status = %d, want 400", unknownSource.Code)
	}

	clipFile := filepath.Join(cfg.Storage.ClipsPending, "ready.mp4")
	if err := os.WriteFile(clipFile, []byte("clip bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	clipResponse := httptest.NewRecorder()
	handler.ServeHTTP(clipResponse, httptest.NewRequest(http.MethodGet, "/clips/ready.mp4", nil))
	if clipResponse.Code != http.StatusOK || clipResponse.Body.String() != "clip bytes" {
		t.Fatalf("GET clip = %d %q", clipResponse.Code, clipResponse.Body.String())
	}
	missingClip := httptest.NewRecorder()
	handler.ServeHTTP(missingClip, httptest.NewRequest(http.MethodGet, "/clips/missing.mp4", nil))
	if missingClip.Code != http.StatusNotFound {
		t.Fatalf("missing clip status = %d, want 404", missingClip.Code)
	}

	postBody := `{"url":"https://youtu.be/video","mode":"video","min_seconds":240,"max_seconds":1680,"resolution":"1920x1080","framing":"fit_blur","captions_enabled":false}`
	start := httptest.NewRecorder()
	startRequest := httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(postBody))
	startRequest.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(start, startRequest)
	if start.Code != http.StatusAccepted {
		t.Fatalf("POST /api/jobs = %d %s", start.Code, start.Body.String())
	}
	var queued jobRecord
	if err := json.Unmarshal(start.Body.Bytes(), &queued); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("job processor did not run")
	}
	var completed jobRecord
	deadline := time.NewTimer(2 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for completed.Status != "done" {
		jobResponse := httptest.NewRecorder()
		handler.ServeHTTP(jobResponse, httptest.NewRequest(http.MethodGet, "/api/jobs/"+queued.ID, nil))
		if err := json.Unmarshal(jobResponse.Body.Bytes(), &completed); err != nil {
			t.Fatal(err)
		}
		if completed.Status == "done" {
			if jobResponse.Code != http.StatusOK {
				t.Fatalf("GET /api/jobs/{id} = %d", jobResponse.Code)
			}
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("job did not complete: %#v", completed)
		}
	}
	if !processedLongForm || processedResolution != "1920x1080" || processedCaptions || completed.Request.CaptionsEnabled == nil || *completed.Request.CaptionsEnabled {
		t.Fatalf("completed job = %#v; processed longform=%t resolution=%s captions=%t", completed, processedLongForm, processedResolution, processedCaptions)
	}

	invalid := httptest.NewRecorder()
	invalidRequest := httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(`{"url":"https://example.com/video"}`))
	invalidRequest.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(invalid, invalidRequest)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid URL status = %d, want 400", invalid.Code)
	}
}

func uiTestConfig(t *testing.T) pl.Config {
	t.Helper()
	cfg := pl.Config{}
	cfg.Sources = []pl.Source{{ID: "channel", ChannelURL: "https://www.youtube.com/@channel"}}
	cfg.Clip.TargetSecondsMin = 45
	cfg.Clip.TargetSecondsMax = 60
	cfg.Clip.LongFormMinSeconds = 240
	cfg.Clip.LongFormMaxSeconds = 1680
	cfg.Storage.MaxEpisodeAgeDays = 14
	return cfg
}
