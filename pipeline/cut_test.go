package pipeline

import (
	"context"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestValidateClipProbeRequiresExpectedStreamsAndDuration(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		ok         bool
	}{
		{"valid", `{"streams":[{"codec_name":"h264","width":1080,"height":1920},{"codec_name":"aac"}],"format":{"duration":"52.500"}}`, true},
		{"missing audio", `{"streams":[{"codec_name":"h264","width":1080,"height":1920}],"format":{"duration":"52.500"}}`, false},
		{"wrong resolution", `{"streams":[{"codec_name":"h264","width":720,"height":1280},{"codec_name":"aac"}],"format":{"duration":"52.500"}}`, false},
		{"truncated", `{"streams":[{"codec_name":"h264","width":1080,"height":1920},{"codec_name":"aac"}],"format":{"duration":"30.000"}}`, false},
		{"bad duration", `{"streams":[{"codec_name":"h264","width":1080,"height":1920},{"codec_name":"aac"}],"format":{"duration":"NaN"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateClipProbe([]byte(tc.data), 52.5)
			if (err == nil) != tc.ok {
				t.Fatalf("got error %v, want success=%v", err, tc.ok)
			}
		})
	}
}

func TestCutOutputPathDistinguishesSubsecondBoundaries(t *testing.T) {
	cfg := Config{}
	cfg.Storage.ClipsPending = t.TempDir()
	ep := Episode{SourceID: "s", VideoID: "v"}
	a := CutOutputPath(cfg, ep, Candidate{Start: 1.1, End: 50.1})
	b := CutOutputPath(cfg, ep, Candidate{Start: 1.2, End: 50.2})
	if a == b {
		t.Fatalf("different cuts share output path %s", a)
	}
}

func TestVideoEncoderSettings(t *testing.T) {
	for _, tc := range []struct {
		setting, encoder string
		wantErr          bool
	}{{"", "libx264", false}, {"libx264", "libx264", false}, {"h264_nvenc", "h264_nvenc", false}, {"unknown", "", true}} {
		args, err := videoEncoderArgs(tc.setting)
		if (err != nil) != tc.wantErr {
			t.Errorf("encoder %q: %v", tc.setting, err)
			continue
		}
		if !tc.wantErr && (len(args) < 2 || args[0] != "-c:v" || args[1] != tc.encoder) {
			t.Errorf("encoder %q got %v", tc.setting, args)
		}
	}
}

func TestCutPreservesExistingClipWhenRenderFails(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(t.TempDir(), "ffmpeg")
	script := "#!/bin/sh\nfor arg do last=$arg; done\nprintf partial > \"$last\"\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(bin))
	existing := CutOutputPath(Config{Storage: Storage{ClipsPending: dir}}, Episode{SourceID: "s", VideoID: "v"}, Candidate{Start: 0, End: 50})
	if err := os.WriteFile(existing, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{}
	cfg.Storage.ClipsPending = dir
	ep := Episode{SourceID: "s", VideoID: "v", VideoPath: "input.mp4"}
	tr := Transcript{VideoID: "v", Words: []Word{{Word: "olá", Start: 1, End: 2}}}
	if _, err := Cut(context.Background(), cfg, ep, tr, Candidate{Start: 0, End: 50}); err == nil {
		t.Fatal("expected render failure")
	}
	got, err := os.ReadFile(existing)
	if err != nil || string(got) != "original" {
		t.Fatalf("existing clip changed: %q (%v)", got, err)
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, ".render-*.mp4"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary render remains: %v (%v)", leftovers, err)
	}
}

func TestThumbnailPreservesExistingFileWhenFFmpegFails(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	clip := filepath.Join(dir, "clip.mp4")
	thumb := ThumbnailPath(clip)
	if err := os.WriteFile(thumb, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := GenerateThumbnail(context.Background(), clip, "A title", 54); err == nil {
		t.Fatal("expected error")
	}
	got, err := os.ReadFile(thumb)
	if err != nil || string(got) != "original" {
		t.Fatalf("original thumbnail changed: %q %v", got, err)
	}
}

func TestGenerateThumbnailFromVideo(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	dir := t.TempDir()
	clip := filepath.Join(dir, "clip.mp4")
	cmd := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "color=c=blue:s=360x640:r=1:d=2", "-c:v", "libx264", "-pix_fmt", "yuv420p", clip)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, b)
	}
	if err := GenerateThumbnail(context.Background(), clip, "Por que a IA mudou tudo? 100%", 2); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(ThumbnailPath(clip))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	size, format, err := image.DecodeConfig(f)
	if err != nil || format != "jpeg" || size.Width != 720 || size.Height != 1280 {
		t.Fatalf("thumbnail: %+v %s %v", size, format, err)
	}
}

func TestPlainCaptionOutputDoesNotOverwriteHighlightedCut(t *testing.T) {
	cfg := Config{}
	cfg.Storage.ClipsPending = t.TempDir()
	ep := Episode{SourceID: "s", VideoID: "v"}
	c := Candidate{Start: 1, End: 51}
	cfg.Clip.Captions.Style = "word_highlight"
	highlight := CutOutputPath(cfg, ep, c)
	cfg.Clip.Captions.Style = "plain"
	plain := CutOutputPath(cfg, ep, c)
	if highlight == plain {
		t.Fatalf("caption variants share output path %s", plain)
	}
}
