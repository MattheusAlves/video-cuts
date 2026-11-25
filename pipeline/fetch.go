package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Episode is a single YouTube video we may want to clip.
type Episode struct {
	VideoID    string    `json:"id"`
	SourceID   string    `json:"source_id"`
	Title      string    `json:"title"`
	ChannelURL string    `json:"channel_url"`
	URL        string    `json:"webpage_url"`
	DurationS  float64   `json:"duration"`
	UploadDate string    `json:"upload_date"` // YYYYMMDD
	VideoPath  string    `json:"video_path"`
	AudioPath  string    `json:"audio_path"`
	FetchedAt  time.Time `json:"fetched_at"`
}

// ListRecentEpisodes uses yt-dlp to enumerate recent uploads for a channel
// without downloading the videos. It returns episodes newer than maxAgeDays
// and longer than minDurationS (we filter out Shorts and tiny clips).
func ListRecentEpisodes(ctx context.Context, src Source, cfg Config, minDurationS int) ([]Episode, error) {
	args := []string{
		"--flat-playlist",
		"--dump-single-json",
		"--no-warnings",
		"--ignore-errors",
		"--playlist-end", "15",
		"-o", "%(id)s", // not used with --dump-single-json but keeps stdout clean
		src.ChannelURL + "/videos",
	}
	cmd := exec.CommandContext(ctx, "yt-dlp", args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("yt-dlp list %s: %w\n%s", src.ID, err, string(ee.Stderr))
		}
		return nil, fmt.Errorf("yt-dlp list %s: %w", src.ID, err)
	}
	var resp struct {
		Entries []struct {
			ID         string  `json:"id"`
			Title      string  `json:"title"`
			URL        string  `json:"url"`
			Duration   float64 `json:"duration"`
			UploadDate string  `json:"upload_date"`
			WebpageURL string  `json:"webpage_url"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse playlist %s: %w", src.ID, err)
	}
	cutoff := time.Now().AddDate(0, 0, -cfg.Storage.MaxEpisodeAgeDays)
	episodes := make([]Episode, 0, len(resp.Entries))
	for _, e := range resp.Entries {
		if e.ID == "" {
			continue
		}
		if e.Duration > 0 && e.Duration < float64(minDurationS) {
			continue // skip Shorts / mini-clips
		}
		if e.UploadDate != "" {
			if t, err := time.Parse("20060102", e.UploadDate); err == nil && t.Before(cutoff) {
				continue
			}
		}
		url := e.WebpageURL
		if url == "" {
			url = "https://www.youtube.com/watch?v=" + e.ID
		}
		episodes = append(episodes, Episode{
			VideoID:    e.ID,
			SourceID:   src.ID,
			Title:      strings.TrimSpace(e.Title),
			ChannelURL: src.ChannelURL,
			URL:        url,
			DurationS:  e.Duration,
			UploadDate: e.UploadDate,
		})
	}
	return episodes, nil
}

// InspectEpisode fills fields often omitted by flat playlist listings.
func InspectEpisode(ctx context.Context, ep Episode) (Episode, error) {
	cmd := exec.CommandContext(ctx, "yt-dlp", "--skip-download", "--dump-single-json", "--no-warnings", ep.URL)
	raw, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ep, fmt.Errorf("episode metadata: %w: %s", err, ee.Stderr)
		}
		return ep, fmt.Errorf("episode metadata: %w", err)
	}
	var data struct {
		ID         string  `json:"id"`
		Title      string  `json:"title"`
		UploadDate string  `json:"upload_date"`
		Duration   float64 `json:"duration"`
		LiveStatus string  `json:"live_status"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return ep, fmt.Errorf("parse episode metadata: %w", err)
	}
	if data.ID != ep.VideoID {
		return ep, fmt.Errorf("episode metadata returned unexpected video id")
	}
	if data.LiveStatus == "is_live" || data.LiveStatus == "is_upcoming" {
		return ep, fmt.Errorf("episode is live or upcoming")
	}
	if _, err := time.Parse("20060102", data.UploadDate); err != nil {
		return ep, fmt.Errorf("episode has no valid upload date")
	}
	if data.Duration <= 0 {
		return ep, fmt.Errorf("episode has no valid duration")
	}
	ep.UploadDate, ep.DurationS = data.UploadDate, data.Duration
	if data.Title != "" {
		ep.Title = data.Title
	}
	return ep, nil
}

// InspectYouTubeURL resolves a single YouTube video URL to episode metadata.
// Playlist URLs and non-YouTube hosts are rejected before invoking yt-dlp.
func InspectYouTubeURL(ctx context.Context, rawURL string) (Episode, error) {
	if err := ValidateYouTubeVideoURL(rawURL); err != nil {
		return Episode{}, err
	}
	var err error
	cmd := exec.CommandContext(ctx, "yt-dlp", "--no-playlist", "--skip-download", "--dump-single-json", "--no-warnings", rawURL)
	raw, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return Episode{}, fmt.Errorf("video metadata: %w: %s", err, ee.Stderr)
		}
		return Episode{}, fmt.Errorf("video metadata: %w", err)
	}
	var data struct {
		ID         string  `json:"id"`
		Title      string  `json:"title"`
		ChannelURL string  `json:"channel_url"`
		WebpageURL string  `json:"webpage_url"`
		UploadDate string  `json:"upload_date"`
		Duration   float64 `json:"duration"`
		LiveStatus string  `json:"live_status"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return Episode{}, fmt.Errorf("parse video metadata: %w", err)
	}
	if data.ID == "" || data.Duration <= 0 {
		return Episode{}, fmt.Errorf("YouTube video has no valid id or duration")
	}
	if data.LiveStatus == "is_live" || data.LiveStatus == "is_upcoming" {
		return Episode{}, fmt.Errorf("YouTube video is live or upcoming")
	}
	if data.WebpageURL == "" {
		data.WebpageURL = "https://www.youtube.com/watch?v=" + data.ID
	}
	return Episode{VideoID: data.ID, SourceID: "adhoc", Title: strings.TrimSpace(data.Title), ChannelURL: data.ChannelURL, URL: data.WebpageURL, DurationS: data.Duration, UploadDate: data.UploadDate}, nil
}

// ValidateYouTubeVideoURL checks that rawURL identifies one YouTube video and
// does not include a playlist or another site's URL.
func ValidateYouTubeVideoURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || !isYouTubeVideoURL(u) {
		return fmt.Errorf("URL must point to a YouTube video")
	}
	return nil
}

func isYouTubeVideoURL(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	switch host {
	case "youtu.be", "www.youtu.be":
		return len(strings.Split(strings.Trim(u.Path, "/"), "/")) == 1 && strings.Trim(u.Path, "/") != ""
	case "youtube.com", "www.youtube.com", "m.youtube.com", "music.youtube.com":
		if u.Path == "/watch" {
			return u.Query().Get("v") != ""
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		return len(parts) == 2 && (parts[0] == "shorts" || parts[0] == "live" || parts[0] == "embed") && parts[1] != ""
	default:
		return false
	}
}

// DownloadEpisode fetches the audio (m4a) and the low-res video (mp4) of one
// episode into the sources directory. We keep both: the audio for Whisper,
// the video for the cut step.
func DownloadEpisode(ctx context.Context, ep Episode, cfg Config) (Episode, error) {
	if err := os.MkdirAll(cfg.Storage.SourcesDir, 0o755); err != nil {
		return ep, fmt.Errorf("mkdir sources: %w", err)
	}
	base := filepath.Join(cfg.Storage.SourcesDir, ep.SourceID+"__"+ep.VideoID)
	videoOut := base + ".%(ext)s"

	// Get audio for transcription (best quality m4a)
	audioCmd := exec.CommandContext(ctx, "yt-dlp",
		"-f", "bestaudio[ext=m4a]/bestaudio",
		"--extract-audio",
		"--audio-format", "m4a",
		"--no-warnings",
		"--ignore-errors",
		"-o", base+".audio.%(ext)s",
		ep.URL,
	)
	audioCmd.Stderr = os.Stderr
	if err := audioCmd.Run(); err != nil {
		return ep, fmt.Errorf("download audio: %w", err)
	}

	// Get a small video for the cut step
	videoCmd := exec.CommandContext(ctx, "yt-dlp",
		"-f", "bv*[height<=720][ext=mp4]+ba[ext=m4a]/b[height<=720][ext=mp4]/b",
		"--no-warnings",
		"--ignore-errors",
		"--recode-video", "mp4",
		"-o", videoOut,
		ep.URL,
	)
	videoCmd.Stderr = os.Stderr
	if err := videoCmd.Run(); err != nil {
		return ep, fmt.Errorf("download video: %w", err)
	}

	// Resolve produced filenames
	audioPath := firstExisting(base+".audio.m4a", base+".audio.mp4", base+".audio.webm", base+".audio.opus")
	videoPath := firstExisting(base+".mp4", base+".mkv", base+".webm")
	if audioPath == "" {
		return ep, fmt.Errorf("audio file not found after download (looked for %s.audio.*)", base)
	}
	if videoPath == "" {
		return ep, fmt.Errorf("video file not found after download (looked for %s.*)", base)
	}
	ep.AudioPath = audioPath
	ep.VideoPath = videoPath
	ep.FetchedAt = time.Now().UTC()
	log.Printf("downloaded %s [%s] audio=%s video=%s", ep.VideoID, ep.SourceID, audioPath, videoPath)
	return ep, nil
}

func firstExisting(paths ...string) string {
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && fi.Size() > 0 {
			return p
		}
	}
	return ""
}
