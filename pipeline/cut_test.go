package pipeline

import (
	"context"
	"os"
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

func TestCutPreservesExistingClipWhenRenderFails(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(t.TempDir(), "ffmpeg")
	script := "#!/bin/sh\nfor arg do last=$arg; done\nprintf partial > \"$last\"\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(bin))
	existing := filepath.Join(dir, "s__v__00000-00050.mp4")
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
