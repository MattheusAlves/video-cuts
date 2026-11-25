package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Cut renders one Candidate into a vertical 1080x1920 mp4 with burned-in ASS captions.
//
// Strategy:
//   - Fit the complete source frame over a blurred background, or use a
//     configurable centered crop, then scale to 1080x1920.
//   - Build an .ass subtitle file from the word-level transcript clipped to the window.
//   - Normalize audio with loudnorm to YouTube-friendly LUFS.
func Cut(ctx context.Context, cfg Config, ep Episode, t Transcript, c Candidate) (string, error) {
	if ep.VideoPath == "" {
		return "", fmt.Errorf("episode %s has no video path", ep.VideoID)
	}
	if err := os.MkdirAll(cfg.Storage.ClipsPending, 0o755); err != nil {
		return "", err
	}
	start := c.Start
	end := c.End
	dur := end - start
	if dur <= 0 {
		return "", fmt.Errorf("candidate %s: non-positive duration", ep.VideoID)
	}
	width, height, err := parseResolution(cfg.Clip.Resolution)
	if err != nil {
		return "", err
	}

	// Output paths
	outName := fmt.Sprintf("%s__%05d-%05d.mp4", ep.SourceID+"__"+ep.VideoID, int(start), int(end))
	outPath := filepath.Join(cfg.Storage.ClipsPending, outName)
	temp, err := os.CreateTemp(cfg.Storage.ClipsPending, ".render-*.mp4")
	if err != nil {
		return "", fmt.Errorf("create temporary clip: %w", err)
	}
	tempPath := temp.Name()
	if err := temp.Close(); err != nil {
		os.Remove(tempPath)
		return "", err
	}
	defer os.Remove(tempPath)

	// Build ASS file with word-level captions from the transcript
	assPath := filepath.Join(cfg.Storage.ClipsPending, strings.TrimSuffix(outName, ".mp4")+".ass")
	if cfg.Clip.Captions.Enabled {
		assBody, err := BuildASSWithHook(cfg, t, start, end)
		if err != nil {
			return "", fmt.Errorf("build ass: %w", err)
		}
		if err := os.WriteFile(assPath, []byte(assBody), 0o644); err != nil {
			return "", err
		}
	}

	// ffmpeg: -ss before -i for fast seek
	assPathEsc := escapeFFmpegFilterPath(assPath)
	subtitleFilter := ""
	if cfg.Clip.Captions.Enabled {
		subtitleFilter = fmt.Sprintf("subtitles=%s", assPathEsc)
	}
	args := []string{
		"-y",
		"-ss", fmt.Sprintf("%.3f", start),
		"-i", ep.VideoPath,
		"-t", fmt.Sprintf("%.3f", dur),
	}
	switch cfg.Clip.Framing {
	case "fit_blur":
		quarterW, quarterH := max(2, width/4), max(2, height/4)
		filter := fmt.Sprintf("[0:v]split=2[bg][fg];[bg]scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,gblur=sigma=10,scale=%d:%d,eq=brightness=-0.2[background];[fg]scale=%d:%d:force_original_aspect_ratio=decrease[foreground];[background][foreground]overlay=(W-w)/2:(H-h)/2,setsar=1",
			quarterW, quarterH, quarterW, quarterH, width, height, width, height)
		if subtitleFilter != "" {
			filter += "," + subtitleFilter
		}
		filter += "[vout]"
		args = append(args, "-filter_complex_threads", "2", "-filter_complex", filter, "-map", "[vout]", "-map", "0:a:0?")
	case "", "center_crop":
		filters := []string{
			fmt.Sprintf("crop='min(iw,ih*%d/%d)':'min(ih,iw*%d/%d)'", width, height, height, width),
			fmt.Sprintf("scale=%d:%d:flags=lanczos", width, height), "setsar=1",
		}
		if subtitleFilter != "" {
			filters = append(filters, subtitleFilter)
		}
		args = append(args, "-vf", strings.Join(filters, ","))
	default:
		return "", fmt.Errorf("unknown clip framing %q", cfg.Clip.Framing)
	}
	args = append(args,
		"-af", "loudnorm=I=-14:TP=-1.5:LRA=11",
		"-r", "30",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-threads", "3",
		"-c:a", "aac", "-b:a", "192k",
		"-movflags", "+faststart",
		tempPath,
	)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	log.Printf("cut %s [%s->%s] -> %s", ep.VideoID, fmtStart(start), fmtStart(end), outPath)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffmpeg cut: %w", err)
	}
	if err := verifyRenderedClipDimensions(ctx, tempPath, dur, width, height); err != nil {
		return "", fmt.Errorf("verify rendered clip: %w", err)
	}
	if err := os.Rename(tempPath, outPath); err != nil {
		return "", fmt.Errorf("publish rendered clip: %w", err)
	}
	return outPath, nil
}

func verifyRenderedClip(ctx context.Context, path string, expectedDuration float64) error {
	return verifyRenderedClipDimensions(ctx, path, expectedDuration, 1080, 1920)
}

func verifyRenderedClipDimensions(ctx context.Context, path string, expectedDuration float64, width, height int) error {
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "stream=codec_name,width,height:format=duration", "-of", "json", path)
	data, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("ffprobe: %w", err)
	}
	return validateClipProbeDimensions(data, expectedDuration, width, height)
}

func validateClipProbe(data []byte, expectedDuration float64) error {
	return validateClipProbeDimensions(data, expectedDuration, 1080, 1920)
}

func validateClipProbeDimensions(data []byte, expectedDuration float64, width, height int) error {
	var info struct {
		Streams []struct {
			Codec  string `json:"codec_name"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return fmt.Errorf("parse ffprobe output: %w", err)
	}
	video, audio := false, false
	for _, s := range info.Streams {
		if s.Codec == "h264" && s.Width == width && s.Height == height {
			video = true
		}
		if s.Codec == "aac" {
			audio = true
		}
	}
	duration, err := strconv.ParseFloat(info.Format.Duration, 64)
	if err != nil || math.IsNaN(duration) || math.IsInf(duration, 0) {
		return fmt.Errorf("invalid clip duration %q", info.Format.Duration)
	}
	if !video || !audio || math.Abs(duration-expectedDuration) > 0.25 {
		return fmt.Errorf("unexpected streams or duration (h264 %dx%d=%t, aac=%t, got %.3fs, expected %.3fs)", width, height, video, audio, duration, expectedDuration)
	}
	return nil
}

func parseResolution(resolution string) (int, int, error) {
	if resolution == "" {
		return 1080, 1920, nil
	}
	parts := strings.Split(strings.ToLower(resolution), "x")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid clip resolution %q; expected WIDTHxHEIGHT", resolution)
	}
	width, errW := strconv.Atoi(parts[0])
	height, errH := strconv.Atoi(parts[1])
	if errW != nil || errH != nil || width < 2 || height < 2 || width%2 != 0 || height%2 != 0 {
		return 0, 0, fmt.Errorf("invalid clip resolution %q; dimensions must be positive even numbers", resolution)
	}
	return width, height, nil
}

// ParseResolution validates and parses a WIDTHxHEIGHT render size.
func ParseResolution(resolution string) (int, int, error) {
	return parseResolution(resolution)
}

// escapeFFmpegFilterPath escapes characters that have meaning in ffmpeg filtergraph paths.
func escapeFFmpegFilterPath(p string) string {
	r := strings.NewReplacer(
		"\\", "\\\\",
		":", "\\:",
		"'", "\\'",
		" ", "\\ ",
		"[", "\\[",
		"]", "\\]",
		",", "\\,",
		";", "\\;",
	)
	return r.Replace(p)
}

func fmtStart(s float64) string {
	m := int(s) / 60
	sec := s - float64(m*60)
	return fmt.Sprintf("%02d:%05.2f", m, sec)
}
