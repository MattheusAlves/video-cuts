package pipeline

import (
	"strings"
	"testing"
)

func TestPlainCaptionsHaveNoYellowOrKaraoke(t *testing.T) {
	cfg := Config{}
	cfg.Clip.Resolution = "1080x1920"
	cfg.Clip.Captions.Font = "DejaVu Sans"
	cfg.Clip.Captions.Style = "plain"
	tr := Transcript{VideoID: "v", Words: []Word{{Word: "Olá", Start: 0, End: 1}, {Word: "mundo", Start: 1, End: 2}}}
	output, err := BuildASSWithHook(cfg, tr, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "&H0000FFFF") || strings.Contains(output, `\kf`) || !strings.Contains(output, "Olá mundo") {
		t.Fatalf("plain captions contain highlight or missing text: %s", output)
	}
	cfg.Clip.Captions.Style = "word_highlight"
	output, err = BuildASSWithHook(cfg, tr, 0, 2)
	if err != nil || !strings.Contains(output, "&H0000FFFF") || !strings.Contains(output, `\kf`) {
		t.Fatalf("highlight captions changed: %s %v", output, err)
	}
}
