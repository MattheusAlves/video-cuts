package pipeline

import (
	"context"
	"fmt"
	"image"
	_ "image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
)

// ThumbnailPath is unique per rendered cut and stays beside the video.
func ThumbnailPath(clipPath string) string {
	return strings.TrimSuffix(clipPath, filepath.Ext(clipPath)) + ".cover.jpg"
}

// GenerateThumbnail extracts a frame from a finished clip and adds its title.
// A temporary output is validated before it replaces any existing cover.
func GenerateThumbnail(ctx context.Context, clipPath, title string, duration float64) error {
	dir := filepath.Dir(clipPath)
	lines := strings.Split(coverTitle(title), "\n")
	var textFiles []string
	defer func() {
		for _, name := range textFiles {
			_ = os.Remove(name)
		}
	}()
	for _, line := range lines {
		text, err := os.CreateTemp(dir, ".cover-text-*.txt")
		if err != nil {
			return err
		}
		textFiles = append(textFiles, text.Name())
		if _, err := text.WriteString(line); err != nil {
			_ = text.Close()
			return err
		}
		if err := text.Close(); err != nil {
			return err
		}
	}
	out, err := os.CreateTemp(dir, ".cover-*.jpg")
	if err != nil {
		return err
	}
	temp := out.Name()
	defer os.Remove(temp)
	if err := out.Close(); err != nil {
		return err
	}
	// Keep title text out of the filter expression: model output can contain
	// quotes, colons, percent signs or characters meaningful to FFmpeg.
	filter := "scale='if(gt(iw,ih),1280,720)':'if(gt(iw,ih),720,1280)':force_original_aspect_ratio=decrease,pad='if(gt(iw,ih),1280,720)':'if(gt(iw,ih),720,1280)':(ow-iw)/2:(oh-ih)/2,drawbox=x=0:y=0:w=iw:h=ih*0.30:color=black@0.72:t=fill"
	for i, name := range textFiles {
		filter += fmt.Sprintf(",drawtext=textfile='%s':expansion=none:font='DejaVu Sans':fontcolor=white:fontsize=h/30:x=(w-text_w)/2:y=(h*0.30-%d*(h/30+8))/2+%d*(h/30+8)", escapeFFmpegFilterPath(name), len(textFiles), i)
	}
	seek := 1.0
	if duration > 0 && duration < 2 {
		seek = duration / 2
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error", "-y", "-ss", fmt.Sprintf("%.3f", seek), "-i", clipPath, "-frames:v", "1", "-vf", filter, "-q:v", "4", temp)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("render cover: %w: %s", err, strings.TrimSpace(string(output)))
	}
	f, err := os.Open(temp)
	if err != nil {
		return err
	}
	_, format, err := image.DecodeConfig(f)
	_ = f.Close()
	if err != nil || format != "jpeg" {
		return fmt.Errorf("render cover: output is not a valid JPEG")
	}
	if err := os.Rename(temp, ThumbnailPath(clipPath)); err != nil {
		return fmt.Errorf("publish cover: %w", err)
	}
	return nil
}

func coverTitle(title string) string {
	title = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.So, r) || unicode.Is(unicode.Sk, r) {
			return -1
		}
		return r
	}, title)
	words := strings.Fields(title)
	var lines []string
	line := ""
	for _, word := range words {
		next := word
		if line != "" {
			next = line + " " + word
		}
		if len([]rune(next)) > 20 && line != "" {
			lines = append(lines, line)
			line = word
		} else {
			line = next
		}
		if len(lines) == 3 {
			lines[2] += "…"
			break
		}
	}
	if len(lines) < 3 && line != "" {
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
