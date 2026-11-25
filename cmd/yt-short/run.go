package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	pl "github.com/mitnick/yt-short/pipeline"
)

// runPipeline processes at most one unseen recent episode per configured source,
// then renders only the highest-scoring candidate from that episode. The user
// chooses which of those clips gets the single daily upload.
func runPipeline(ctx context.Context, cfg pl.Config, st *pl.State, sourceFilter, videoFilter string) error {
	if videoFilter != "" && sourceFilter == "" {
		return fmt.Errorf("--pipeline --video requires --source")
	}
	if err := pipelinePreflight(ctx, cfg); err != nil {
		return err
	}
	var failures []error
	matched := false
	produced := 0
	onlyMBL := true
	for _, src := range cfg.Sources {
		if sourceFilter != "" && src.ID != sourceFilter {
			continue
		}
		matched = true
		if err := ctx.Err(); err != nil {
			log.Printf("stopping: %v", err)
			failures = append(failures, err)
			break
		}
		seenBefore := len(st.Seen)
		if err := processSource(ctx, cfg, st, src, videoFilter); err != nil {
			log.Printf("%s: %v", src.ID, err)
			failures = append(failures, fmt.Errorf("%s: %w", src.ID, err))
			if isProviderAccountError(err) {
				log.Printf("stopping remaining sources: provider authorization or quota must be resolved")
				break
			}
		} else if len(st.Seen) > seenBefore {
			produced++
			if src.Polarity != "mbl" {
				onlyMBL = false
			}
		}
	}
	if produced > 0 && onlyMBL {
		log.Printf("editorial balance: all %d clips produced in this run are from the MBL family", produced)
	}
	log.Printf("daily summary: %d clips produced, %d source failures", produced, len(failures))
	if !matched && sourceFilter != "" {
		failures = append(failures, fmt.Errorf("unknown source %q", sourceFilter))
	}
	if err := pl.WriteReviewPage(cfg); err != nil {
		log.Printf("review page: %v", err)
		failures = append(failures, fmt.Errorf("review page: %w", err))
	} else {
		abs, _ := filepath.Abs(filepath.Join(cfg.Storage.ClipsPending, "index.html"))
		log.Printf("review page updated: %s", abs)
	}
	if err := st.Save(); err != nil {
		log.Printf("save state: %v", err)
		failures = append(failures, fmt.Errorf("save state: %w", err))
	}
	return errors.Join(failures...)
}

func runPipelineURL(ctx context.Context, cfg pl.Config, st *pl.State, videoURL string) error {
	if err := pipelinePreflight(ctx, cfg); err != nil {
		return err
	}
	ep, err := pl.InspectYouTubeURL(ctx, videoURL)
	if err != nil {
		return fmt.Errorf("inspect URL: %w", err)
	}
	if err := processEpisode(ctx, cfg, st, ep); err != nil {
		return err
	}
	if err := pl.WriteReviewPage(cfg); err != nil {
		return fmt.Errorf("review page: %w", err)
	}
	return st.Save()
}

func isProviderAccountError(err error) bool {
	var providerErr *pl.ProviderHTTPError
	return errors.As(err, &providerErr) && (providerErr.StatusCode == http.StatusUnauthorized || providerErr.StatusCode == http.StatusForbidden || providerErr.StatusCode == http.StatusTooManyRequests)
}

func useLongFormProfile(cfg *pl.Config) error {
	if cfg.Clip.LongFormMinSeconds <= 0 {
		cfg.Clip.LongFormMinSeconds = 240
	}
	if cfg.Clip.LongFormMaxSeconds <= 0 {
		cfg.Clip.LongFormMaxSeconds = 28 * 60
	}
	if cfg.Clip.LongFormMinSeconds < 1 || cfg.Clip.LongFormMaxSeconds < cfg.Clip.LongFormMinSeconds || cfg.Clip.LongFormMaxSeconds > 28*60 {
		return fmt.Errorf("long-form duration must be positive and no longer than 28 minutes")
	}
	cfg.Clip.TargetSecondsMin = cfg.Clip.LongFormMinSeconds
	cfg.Clip.TargetSecondsMax = cfg.Clip.LongFormMaxSeconds
	cfg.Clip.LongForm = true
	cfg.Clip.Resolution = "1920x1080"
	cfg.Clip.Framing = "center_crop"
	return nil
}

func pipelinePreflight(ctx context.Context, cfg pl.Config) error {
	if cfg.Transcription.Provider != "local" && cfg.Transcription.Provider != "whisper.cpp" {
		return fmt.Errorf("unsupported transcription provider %q", cfg.Transcription.Provider)
	}
	bin := cfg.Transcription.WhisperBin
	if bin == "" {
		bin = "whisper-cli"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("whisper executable: %w", err)
	}
	if !nonemptyFile(cfg.Transcription.WhisperModel) {
		return fmt.Errorf("whisper model missing or empty: %s", cfg.Transcription.WhisperModel)
	}
	if cfg.Selector.Provider != "openai_compat" {
		return fmt.Errorf("unsupported selector provider %q", cfg.Selector.Provider)
	}
	endpoint, err := url.Parse(cfg.Selector.BaseURL)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return fmt.Errorf("selector.base_url must be an HTTP(S) URL")
	}
	if cfg.Selector.ChatModel == "" {
		return fmt.Errorf("selector.chat_model is required")
	}
	if cfg.Selector.APIKeyEnv == "" || os.Getenv(cfg.Selector.APIKeyEnv) == "" {
		return fmt.Errorf("selector API key environment variable %q is missing", cfg.Selector.APIKeyEnv)
	}
	if cfg.Clip.TargetSecondsMin <= 0 || cfg.Clip.TargetSecondsMax < cfg.Clip.TargetSecondsMin {
		return fmt.Errorf("invalid target clip duration")
	}
	if cfg.Clip.LongForm {
		if cfg.Clip.TargetSecondsMax > 28*60 {
			return fmt.Errorf("long-form clips cannot exceed 28 minutes")
		}
	} else if cfg.Clip.TargetSecondsMax >= 180 {
		return fmt.Errorf("short-form clips must be shorter than 180 seconds")
	}
	if strings.EqualFold(endpoint.Hostname(), "opencode.ai") && strings.HasPrefix(endpoint.Path, "/inference/go/") {
		if err := checkOpenCodeAllowance(ctx, cfg.Selector.APIKeyEnv); err != nil {
			return err
		}
	}
	return nil
}

func checkOpenCodeAllowance(ctx context.Context, keyEnv string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://opencode.ai/inference/go/v1/usage", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv(keyEnv))
	req.Header.Set("User-Agent", "yt-short/1.0")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("check OpenCode Go allowance: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("check OpenCode Go allowance: status %d", resp.StatusCode)
	}
	var data struct {
		Usage map[string]struct {
			Status   string `json:"status"`
			ResetsAt string `json:"resetsAt"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return fmt.Errorf("parse OpenCode Go allowance: %w", err)
	}
	if len(data.Usage) == 0 {
		return fmt.Errorf("OpenCode Go allowance response has no usage meters")
	}
	for _, name := range []string{"rolling", "weekly", "monthly"} {
		if meter, ok := data.Usage[name]; ok && meter.Status == "rate-limited" {
			return fmt.Errorf("OpenCode Go %s allowance exhausted (resets %s)", name, meter.ResetsAt)
		}
	}
	return nil
}

func processSource(ctx context.Context, cfg pl.Config, st *pl.State, src pl.Source, videoFilter string) (resultErr error) {
	eps, err := pl.ListRecentEpisodes(ctx, src, cfg, 600)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	var next *pl.Episode
	for i := range eps {
		if videoFilter != "" && eps[i].VideoID != videoFilter {
			continue
		}
		if st.IsSeen(eps[i].VideoID) {
			continue
		}
		verified, err := pl.InspectEpisode(ctx, eps[i])
		if err != nil {
			return fmt.Errorf("inspect %s: %w", eps[i].VideoID, err)
		}
		published, _ := time.Parse("20060102", verified.UploadDate)
		if cfg.Storage.MaxEpisodeAgeDays > 0 && published.Before(time.Now().UTC().AddDate(0, 0, -cfg.Storage.MaxEpisodeAgeDays).Truncate(24*time.Hour)) {
			continue
		}
		if verified.DurationS < 600 {
			continue
		}
		next = &verified
		break
	}
	if next == nil {
		if videoFilter != "" {
			return fmt.Errorf("requested episode %s is not an eligible unseen recent episode", videoFilter)
		}
		log.Printf("%s: no new episodes", src.ID)
		return nil
	}
	return processEpisode(ctx, cfg, st, *next)
}

func processEpisode(ctx context.Context, cfg pl.Config, st *pl.State, input pl.Episode) (resultErr error) {
	ep := input
	var err error
	defer func() {
		if resultErr != nil {
			st.Fails[ep.VideoID] = resultErr.Error()
		}
	}()
	base := filepath.Join(cfg.Storage.SourcesDir, ep.SourceID+"__"+ep.VideoID)
	if nonemptyFile(base+".audio.m4a") && nonemptyFile(base+".mp4") {
		pl.ReportProgress(ctx, pl.Progress{Stage: "download", Message: "Reusing downloaded video and audio"})
		ep.AudioPath, ep.VideoPath = base+".audio.m4a", base+".mp4"
		log.Printf("%s: reusing downloaded media for %s", ep.SourceID, ep.VideoID)
	} else {
		pl.ReportProgress(ctx, pl.Progress{Stage: "download", Message: "Downloading video and audio"})
		log.Printf("%s: downloading %s %q", ep.SourceID, ep.VideoID, ep.Title)
		ep, err = pl.DownloadEpisode(ctx, ep, cfg)
		if err != nil {
			st.Fails[ep.VideoID] = err.Error()
			return fmt.Errorf("download: %w", err)
		}
	}
	if err := saveEpisodeMeta(ep, cfg); err != nil {
		log.Printf("%s: save metadata: %v", ep.SourceID, err)
	}

	transPath := filepath.Join(cfg.Storage.Transcripts, ep.SourceID+"__"+ep.VideoID+".json")
	var tr pl.Transcript
	if err := loadJSON(transPath, &tr); err != nil || tr.VideoID != ep.VideoID || tr.DurationS <= 0 || len(tr.Segments) == 0 || len(tr.Words) == 0 {
		pl.ReportProgress(ctx, pl.Progress{Stage: "transcribe", Message: "Transcribing audio locally"})
		tr, err = pl.Transcribe(ctx, cfg, ep, "")
		if err != nil {
			st.Fails[ep.VideoID] = err.Error()
			return fmt.Errorf("transcribe: %w", err)
		}
		transPath, err = pl.SaveTranscript(cfg, ep.SourceID, tr)
		if err != nil {
			return fmt.Errorf("save transcript: %w", err)
		}
	} else {
		pl.ReportProgress(ctx, pl.Progress{Stage: "transcribe", Message: "Reusing existing transcript"})
		log.Printf("%s: reusing transcript %s", ep.SourceID, transPath)
	}
	log.Printf("%s: transcript %s (%d words, %d segments)", ep.SourceID, transPath, len(tr.Words), len(tr.Segments))

	pl.ReportProgress(ctx, pl.Progress{Stage: "select", Message: "Searching transcript for strong cuts"})
	cands, err := pl.Select(ctx, cfg, ep, tr)
	if err != nil {
		st.Fails[ep.VideoID] = err.Error()
		return fmt.Errorf("select: %w", err)
	}
	if len(cands) == 0 {
		return fmt.Errorf("no candidates")
	}
	candPath := transPath[:len(transPath)-len(".json")] + ".candidates.json"
	if err := saveJSON(candPath, cands); err != nil {
		return fmt.Errorf("save candidates: %w", err)
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Score > cands[j].Score })

	var chosen *pl.Candidate
	for i := range cands {
		d := cands[i].End - cands[i].Start
		// Keep the chosen duration inside the active Shorts or long-form profile.
		if d >= float64(cfg.Clip.TargetSecondsMin) && d <= float64(cfg.Clip.TargetSecondsMax) && cands[i].Start >= 0 && cands[i].End <= tr.DurationS {
			chosen = &cands[i]
			break
		}
	}
	if chosen == nil {
		return fmt.Errorf("all candidates outside accepted duration")
	}
	pl.ReportProgress(ctx, pl.Progress{Stage: "render", Message: "Rendering video and captions"})
	clipPath, err := pl.Cut(ctx, cfg, ep, tr, *chosen)
	if err != nil {
		st.Fails[ep.VideoID] = err.Error()
		return fmt.Errorf("cut: %w", err)
	}
	pl.ReportProgress(ctx, pl.Progress{Stage: "finalize", Message: "Saving cut for review"})
	meta := pl.PendingClip{
		Candidate:   *chosen,
		SourceTitle: ep.Title,
		SourceURL:   ep.URL,
		ClipPath:    clipPath,
		CreatedAt:   pl.NowUTC(),
	}
	if err := pl.WritePendingSidecar(clipPath, meta); err != nil {
		return fmt.Errorf("sidecar: %w", err)
	}
	st.MarkSeen(ep.VideoID)
	delete(st.Fails, ep.VideoID)
	if err := st.Save(); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	log.Printf("%s: ready %s — %s", ep.SourceID, clipPath, chosen.Title)

	if !cfg.Storage.KeepSources {
		for _, p := range []string{ep.AudioPath, ep.VideoPath} {
			if p != "" {
				if err := os.Remove(p); err != nil {
					log.Printf("cleanup %s: %v", p, err)
				}
			}
		}
	}
	return nil
}

func nonemptyFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}
