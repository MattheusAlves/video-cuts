package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	pl "github.com/mitnick/yt-short/pipeline"
)

//go:embed web/dist
var webFiles embed.FS

type jobRequest struct {
	URL             string `json:"url"`
	SourceID        string `json:"source_id"`
	VideoID         string `json:"video_id"`
	Mode            string `json:"mode"`
	MinSeconds      int    `json:"min_seconds"`
	MaxSeconds      int    `json:"max_seconds"`
	Resolution      string `json:"resolution"`
	Framing         string `json:"framing"`
	CaptionsEnabled *bool  `json:"captions_enabled"`
}

type jobRecord struct {
	ID         string       `json:"id"`
	Status     string       `json:"status"`
	Message    string       `json:"message,omitempty"`
	Error      string       `json:"error,omitempty"`
	Request    jobRequest   `json:"request"`
	CreatedAt  time.Time    `json:"created_at"`
	StartedAt  *time.Time   `json:"started_at,omitempty"`
	FinishedAt *time.Time   `json:"finished_at,omitempty"`
	Progress   *pl.Progress `json:"progress,omitempty"`
}

type uiServer struct {
	cfg        pl.Config
	state      *pl.State
	mu         sync.RWMutex
	runMu      sync.Mutex
	jobs       map[string]jobRecord
	ctx        context.Context
	processJob func(context.Context, pl.Config, *pl.State, jobRequest) error
}

func serveUI(ctx context.Context, cfg pl.Config, state *pl.State) error {
	s := &uiServer{cfg: cfg, state: state, jobs: map[string]jobRecord{}, ctx: ctx}
	handler, err := newUIHandler(s)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: "127.0.0.1:8765", Handler: withLocalHeaders(handler), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Printf("local video studio: http://127.0.0.1:8765/")
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func newUIHandler(s *uiServer) (http.Handler, error) {
	assets, err := fs.Sub(webFiles, "web/dist")
	if err != nil {
		return nil, fmt.Errorf("open web assets: %w", err)
	}
	if _, err := fs.Stat(assets, "index.html"); err != nil {
		return nil, fmt.Errorf("React build missing; run npm run build:embedded in web: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/videos", s.handleVideos)
	mux.HandleFunc("GET /api/jobs", s.handleJobs)
	mux.HandleFunc("POST /api/jobs", s.handleStartJob)
	mux.HandleFunc("GET /api/jobs/{id}", s.handleJob)
	mux.HandleFunc("GET /api/clips", s.handleClips)
	mux.HandleFunc("GET /clips/{name}", s.handleClipFile)
	mux.Handle("/", http.FileServer(http.FS(assets)))
	return mux, nil
}

func withLocalHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			allowed := err == nil && u.Scheme == "http" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" &&
				(u.Host == "127.0.0.1:8765" || u.Host == "localhost:8765" || u.Host == "127.0.0.1:5173" || u.Host == "localhost:5173")
			if !allowed {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/jobs" {
			mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mediaType != "application/json" {
				http.Error(w, "content type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *uiServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	type source struct {
		ID         string `json:"id"`
		ChannelURL string `json:"channel_url"`
	}
	sources := make([]source, 0, len(s.cfg.Sources))
	for _, item := range s.cfg.Sources {
		sources = append(sources, source{ID: item.ID, ChannelURL: item.ChannelURL})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sources":          sources,
		"short":            map[string]int{"min_seconds": s.cfg.Clip.TargetSecondsMin, "max_seconds": s.cfg.Clip.TargetSecondsMax},
		"video":            map[string]int{"min_seconds": s.cfg.Clip.LongFormMinSeconds, "max_seconds": s.cfg.Clip.LongFormMaxSeconds},
		"captions_enabled": s.cfg.Clip.Captions.Enabled,
	})
}

func (s *uiServer) handleVideos(w http.ResponseWriter, r *http.Request) {
	sourceID := r.URL.Query().Get("source")
	var src *pl.Source
	for i := range s.cfg.Sources {
		if s.cfg.Sources[i].ID == sourceID {
			src = &s.cfg.Sources[i]
			break
		}
	}
	if src == nil {
		writeError(w, http.StatusBadRequest, "unknown source")
		return
	}
	episodes, err := pl.ListRecentEpisodes(r.Context(), *src, s.cfg, 60)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, episodes)
}

func (s *uiServer) handleStartJob(w http.ResponseWriter, r *http.Request) {
	var request jobRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.prepareRequest(&request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := newJobID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create job id")
		return
	}
	job := jobRecord{ID: id, Status: "queued", Message: "Waiting to start", Request: request, CreatedAt: time.Now().UTC()}
	s.mu.Lock()
	s.jobs[id] = job
	s.mu.Unlock()
	go s.runJob(id, request)
	writeJSON(w, http.StatusAccepted, job)
}

func (s *uiServer) prepareRequest(request *jobRequest) error {
	request.URL = strings.TrimSpace(request.URL)
	request.SourceID = strings.TrimSpace(request.SourceID)
	request.VideoID = strings.TrimSpace(request.VideoID)
	hasURL := request.URL != ""
	hasSourceVideo := request.SourceID != "" && request.VideoID != ""
	if hasURL == hasSourceVideo {
		return fmt.Errorf("provide either a YouTube URL or a configured source and video")
	}
	if hasURL {
		if request.SourceID != "" || request.VideoID != "" {
			return fmt.Errorf("provide either a YouTube URL or a configured source and video")
		}
		if err := pl.ValidateYouTubeVideoURL(request.URL); err != nil {
			return err
		}
	}
	if request.Mode == "" {
		request.Mode = "short"
	}
	var minDefault, maxDefault int
	switch request.Mode {
	case "short":
		minDefault, maxDefault = s.cfg.Clip.TargetSecondsMin, s.cfg.Clip.TargetSecondsMax
	case "video":
		minDefault, maxDefault = s.cfg.Clip.LongFormMinSeconds, s.cfg.Clip.LongFormMaxSeconds
	default:
		return fmt.Errorf("mode must be short or video")
	}
	if request.MinSeconds == 0 {
		request.MinSeconds = minDefault
	}
	if request.MaxSeconds == 0 {
		request.MaxSeconds = maxDefault
	}
	if request.MinSeconds <= 0 || request.MaxSeconds < request.MinSeconds {
		return fmt.Errorf("invalid duration range")
	}
	if request.Mode == "short" && request.MaxSeconds >= 180 {
		return fmt.Errorf("Short cuts must be shorter than 180 seconds")
	}
	if request.Mode == "video" && request.MaxSeconds > 28*60 {
		return fmt.Errorf("regular video cuts cannot exceed 28 minutes")
	}
	if request.Resolution == "" {
		if request.Mode == "video" {
			request.Resolution = "1920x1080"
		} else {
			request.Resolution = "1080x1920"
		}
	}
	if _, _, err := pl.ParseResolution(request.Resolution); err != nil {
		return err
	}
	if request.Framing == "" {
		request.Framing = "center_crop"
	}
	if request.Framing != "center_crop" && request.Framing != "fit_blur" {
		return fmt.Errorf("framing must be center_crop or fit_blur")
	}
	if request.SourceID != "" {
		for _, source := range s.cfg.Sources {
			if request.SourceID == source.ID {
				return nil
			}
		}
		return fmt.Errorf("unknown source %q", request.SourceID)
	}
	return nil
}

func (s *uiServer) runJob(id string, request jobRequest) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	started := time.Now().UTC()
	s.updateJob(id, func(job *jobRecord) {
		job.Status = "running"
		job.Message = "Downloading and processing video"
		job.StartedAt = &started
	})
	cfg := s.cfg
	cfg.Clip.LongForm = request.Mode == "video"
	cfg.Clip.TargetSecondsMin, cfg.Clip.TargetSecondsMax = request.MinSeconds, request.MaxSeconds
	cfg.Clip.Resolution, cfg.Clip.Framing = request.Resolution, request.Framing
	if request.CaptionsEnabled != nil {
		cfg.Clip.Captions.Enabled = *request.CaptionsEnabled
	}
	ctx, cancel := context.WithTimeout(s.ctx, 24*time.Hour)
	defer cancel()
	ctx = pl.WithProgress(ctx, func(progress pl.Progress) {
		s.updateJob(id, func(job *jobRecord) {
			job.Progress = &progress
			job.Message = progress.Message
		})
	})
	pl.ReportProgress(ctx, pl.Progress{Stage: "preparing", Message: "Checking video and pipeline settings"})
	var err error
	if s.processJob != nil {
		err = s.processJob(ctx, cfg, s.state, request)
	} else if request.URL != "" {
		ep, inspectErr := pl.InspectYouTubeURL(ctx, request.URL)
		if inspectErr != nil {
			err = fmt.Errorf("inspect URL: %w", inspectErr)
		} else {
			err = pipelinePreflight(ctx, cfg)
			if err == nil {
				err = processEpisode(ctx, cfg, s.state, ep)
			}
			if err == nil {
				err = pl.WriteReviewPage(cfg)
			}
			if err == nil {
				err = s.state.Save()
			}
		}
	} else {
		err = runPipeline(ctx, cfg, s.state, request.SourceID, request.VideoID)
	}
	finished := time.Now().UTC()
	if err != nil {
		s.updateJob(id, func(job *jobRecord) {
			job.Status = "failed"
			job.Error = err.Error()
			job.Message = "Processing failed"
			job.FinishedAt = &finished
		})
		return
	}
	s.updateJob(id, func(job *jobRecord) {
		job.Status = "done"
		job.Message = "Video is ready for review"
		job.FinishedAt = &finished
	})
}

func (s *uiServer) updateJob(id string, update func(*jobRecord)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[id]
	update(&job)
	s.jobs[id] = job
}

func (s *uiServer) handleJobs(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	jobs := make([]jobRecord, 0, len(s.jobs))
	for _, job := range s.jobs {
		jobs = append(jobs, job)
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *uiServer) handleJob(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	job, ok := s.jobs[r.PathValue("id")]
	s.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *uiServer) handleClips(w http.ResponseWriter, _ *http.Request) {
	clips, err := pl.ListPending(s.cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if clips == nil {
		clips = []pl.PendingClip{}
	}
	writeJSON(w, http.StatusOK, clips)
}

func (s *uiServer) handleClipFile(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.PathValue("name"))
	if name == "." || !strings.HasSuffix(strings.ToLower(name), ".mp4") {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(s.cfg.Storage.ClipsPending, name)
	if _, err := os.Stat(path); err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, path)
}

func newJobID() (string, error) {
	var bytes [12]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
