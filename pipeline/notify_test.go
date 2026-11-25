package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReviewPageShowsExistingClipAndEscapesStory(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{}
	cfg.Storage.ClipsPending = dir
	video := filepath.Join(dir, "valid.mp4")
	if err := os.WriteFile(video, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	meta := PendingClip{
		Candidate: Candidate{Title: "Título <novo>", Story: StoryEvidence{SetupQuote: "início <tag>", DevelopmentQuote: "meio & fim", ConclusionQuote: "concluiu"}},
		ClipPath:  video, SourceTitle: "Fonte", SourceURL: "https://example.com/video", CreatedAt: time.Now(),
	}
	if err := WritePendingSidecar(video, meta); err != nil {
		t.Fatal(err)
	}
	missing := meta
	missing.ClipPath = filepath.Join(dir, "missing.mp4")
	missing.Title = "missing"
	if err := WritePendingSidecar(missing.ClipPath, missing); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.mp4")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	emptyMeta := meta
	emptyMeta.ClipPath, emptyMeta.Title = empty, "empty"
	if err := WritePendingSidecar(empty, emptyMeta); err != nil {
		t.Fatal(err)
	}
	if err := WriteReviewPage(cfg); err != nil {
		t.Fatal(err)
	}
	html, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	page := string(html)
	for _, want := range []string{"valid.mp4", "Título &lt;novo&gt;", "início &lt;tag&gt;", "meio &amp; fim", "concluiu"} {
		if !strings.Contains(page, want) {
			t.Errorf("review page missing %q", want)
		}
	}
	if strings.Contains(page, "missing.mp4") || strings.Contains(page, "empty.mp4") {
		t.Fatal("review page includes an unavailable clip")
	}
}
