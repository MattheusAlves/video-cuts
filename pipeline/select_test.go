package pipeline

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSelectRejectsTranscriptWithoutWordTimestamps(t *testing.T) {
	_, err := Select(context.Background(), Config{}, Episode{VideoID: "v"}, Transcript{Segments: []Segment{{Text: "texto"}}})
	if err == nil || !strings.Contains(err.Error(), "no word timestamps") {
		t.Fatalf("expected missing word timestamps error, got %v", err)
	}
}

const testStory = `,"hook_quote":"começo claro","attention_signals":["curiosity"],"virality_reason":"A fala abre uma curiosidade concreta.","story":{"setup_quote":"começo claro","development_quote":"algo mudou","conclusion_quote":"final chegou"}`

func storyWords(starts ...float64) []Word {
	var words []Word
	for _, start := range starts {
		words = append(words,
			Word{Word: "começo", Start: start, End: start + 1},
			Word{Word: "claro", Start: start + 1, End: start + 2},
			Word{Word: "algo", Start: start + 20, End: start + 21},
			Word{Word: "mudou", Start: start + 21, End: start + 22},
			Word{Word: "final", Start: start + 48, End: start + 49},
			Word{Word: "chegou.", Start: start + 49, End: start + 50},
		)
	}
	return words
}

func TestStoryEvidenceRequiresCompleteVerifiableEnding(t *testing.T) {
	words := storyWords(0)
	c := Candidate{Start: 0, End: 50, Story: StoryEvidence{SetupQuote: "começo claro", DevelopmentQuote: "algo mudou", ConclusionQuote: "final chegou"}}
	if !hasStoryEvidence(c, words) {
		t.Fatal("complete story rejected")
	}
	missing := c
	missing.Story.ConclusionQuote = "nunca aconteceu"
	if hasStoryEvidence(missing, words) {
		t.Fatal("invented conclusion accepted")
	}
	cutOff := c
	cutOff.End = 49
	if hasStoryEvidence(cutOff, words) {
		t.Fatal("cut-off conclusion accepted")
	}
	unfinished := append(append([]Word(nil), words...), Word{Word: "mas", Start: 50, End: 52})
	unfinishedClip := c
	unfinishedClip.End = 52
	if hasStoryEvidence(unfinishedClip, unfinished) {
		t.Fatal("trailing unfinished thought accepted")
	}
	misordered := c
	misordered.Story.DevelopmentQuote, misordered.Story.ConclusionQuote = misordered.Story.ConclusionQuote, misordered.Story.DevelopmentQuote
	if hasStoryEvidence(misordered, words) {
		t.Fatal("out-of-order story accepted")
	}
}

func TestAlignWordBoundariesIncludesEntireWords(t *testing.T) {
	words := []Word{{Word: "começo", Start: 2, End: 3}, {Word: "claro", Start: 3, End: 4}, {Word: "fim", Start: 49, End: 50}}
	c := alignWordBoundaries(Candidate{Start: 2.5, End: 49.5}, words)
	if c.Start != 2 || c.End != 50 {
		t.Fatalf("unexpected boundaries: %.2f–%.2f", c.Start, c.End)
	}
}

type testChatProvider struct{ response string }

func (p testChatProvider) Complete(context.Context, string, string) (string, error) {
	return p.response, nil
}

func TestReviewStoriesRejectsIncompleteAndMissingReviews(t *testing.T) {
	candidates := []Candidate{{Start: 0, End: 50}, {Start: 60, End: 110}}
	tr := Transcript{Words: storyWords(0, 60)}
	provider := testChatProvider{response: `{"reviews":[{"index":0,"complete":false},{"index":1,"complete":true}]}`}
	got, err := reviewStories(context.Background(), provider, candidates, tr)
	if err != nil || len(got) != 1 || got[0].Start != 60 {
		t.Fatalf("reviewed=%+v err=%v", got, err)
	}
	provider.response = `{"reviews":[{"index":0,"complete":true}]}`
	if _, err := reviewStories(context.Background(), provider, candidates, tr); err == nil {
		t.Fatal("missing review accepted")
	}
}

func TestSelectWindowsDeduplicatesAndRejectsOutOfWindow(t *testing.T) {
	requests := 0
	previous := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		bodyBytes, _ := io.ReadAll(r.Body)
		if strings.Contains(string(bodyBytes), "Avalie cada corte") {
			body := fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, `{"reviews":[{"index":0,"complete":true},{"index":1,"complete":true}]}`)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		}
		if r.Header.Get("x-opencode-session") == "" {
			t.Error("missing session header")
		}
		clips := `[{"start":130,"end":180,"score":0.8,"title":"first"` + testStory + `}]`
		if requests == 2 {
			clips = `[{"start":130,"end":180,"score":0.9,"title":"duplicate"` + testStory + `},{"start":10,"end":60,"score":1,"title":"outside"` + testStory + `},{"start":220,"end":270,"score":0.7,"title":"second"` + testStory + `}]`
		}
		body := fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, `{"clips":`+clips+`}`)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	defer func() { http.DefaultTransport = previous }()
	t.Setenv("TEST_SELECTOR_KEY", "key")
	cfg := Config{}
	cfg.Selector.Provider, cfg.Selector.APIKeyEnv, cfg.Selector.BaseURL = "openai_compat", "TEST_SELECTOR_KEY", "https://example.invalid/v1"
	cfg.Selector.CandidatesPerEpisode = 5
	cfg.Clip.TargetSecondsMin, cfg.Clip.TargetSecondsMax = 45, 60
	tr := Transcript{DurationS: 300, Words: storyWords(130, 220), Segments: []Segment{{Start: 0, End: 150, Text: "a"}, {Start: 150, End: 300, Text: "b"}}}
	got, err := Select(context.Background(), cfg, Episode{VideoID: "v", SourceID: "s", DurationS: 300}, tr)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 3 || len(got) != 2 || got[0].Title != "duplicate" || got[1].Title != "second" {
		t.Fatalf("requests=%d candidates=%+v", requests, got)
	}
	if strings.TrimSpace(got[0].VideoID) != "v" {
		t.Fatal("video id not preserved")
	}
}

func TestSelectResumesCompletedWindowAfterFailure(t *testing.T) {
	requests := 0
	previous := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		bodyBytes, _ := io.ReadAll(r.Body)
		if strings.Contains(string(bodyBytes), "Avalie cada corte") {
			body := fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, `{"reviews":[{"index":0,"complete":true},{"index":1,"complete":true}]}`)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		}
		if requests == 2 {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("unavailable")), Header: make(http.Header)}, nil
		}
		clip := `{"start":10,"end":60,"score":0.8,"title":"first"` + testStory + `}`
		if requests > 2 {
			clip = `{"start":200,"end":250,"score":0.9,"title":"second"` + testStory + `}`
		}
		body := fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, `{"clips":[`+clip+`]}`)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	defer func() { http.DefaultTransport = previous }()
	t.Setenv("TEST_SELECTOR_KEY", "key")
	cfg := Config{}
	cfg.Storage.Transcripts = t.TempDir()
	cfg.Selector.Provider, cfg.Selector.APIKeyEnv, cfg.Selector.BaseURL = "openai_compat", "TEST_SELECTOR_KEY", "https://example.invalid/v1"
	cfg.Selector.CandidatesPerEpisode = 5
	cfg.Clip.TargetSecondsMin, cfg.Clip.TargetSecondsMax = 45, 60
	tr := Transcript{DurationS: 300, Words: storyWords(10, 200), Segments: []Segment{{Start: 0, End: 150, Text: "a"}, {Start: 150, End: 300, Text: "b"}}}
	ep := Episode{VideoID: "v", SourceID: "s", DurationS: 300}
	if _, err := Select(context.Background(), cfg, ep, tr); err == nil {
		t.Fatal("expected second window failure")
	}
	got, err := Select(context.Background(), cfg, ep, tr)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 4 || len(got) != 2 || got[0].Title != "second" || got[1].Title != "first" {
		t.Fatalf("requests=%d candidates=%+v", requests, got)
	}
}
