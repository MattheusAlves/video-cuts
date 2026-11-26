package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOpenRouterUsageIsPersistedWithoutPrompt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	ctx := WithTiming(context.Background(), NewTimingRecorder(path, "job"))
	p := OpenAICompatChat{APIKey: "test-key", BaseURL: "https://openrouter.ai/api/v1", Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"provider":"example","model":"z-ai/glm-5.3","usage":{"prompt_tokens":120,"completion_tokens":30,"cost":0.002,"completion_tokens_details":{"reasoning_tokens":20},"prompt_tokens_details":{"cached_tokens":50}},"choices":[{"message":{"content":"{}"}}]}`))}, nil
	})}}
	if _, err := p.Complete(ctx, "private system", "private transcript"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var event TimingEvent
	if err := json.Unmarshal(b, &event); err != nil {
		t.Fatal(err)
	}
	if event.Usage == nil || event.Usage.PromptTokens != 120 || event.Usage.CompletionTokens != 30 || event.Usage.ReasoningTokens != 20 || event.Usage.CachedTokens != 50 || event.Usage.Cost == nil || *event.Usage.Cost != 0.002 {
		t.Fatalf("bad usage: %+v", event.Usage)
	}
	if strings.Contains(string(b), "private") || strings.Contains(string(b), "test-key") {
		t.Fatal("sensitive content in usage log")
	}
}

func TestProviderIncludesReasoningLimitsAndRejectsTruncation(t *testing.T) {
	p := OpenAICompatChat{APIKey: "key", BaseURL: "https://openrouter.ai/api/v1", ReasoningEffort: "low", MaxTokens: 4096, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var data map[string]any
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			t.Fatal(err)
		}
		reasoning, ok := data["reasoning"].(map[string]any)
		if !ok || reasoning["effort"] != "low" || data["max_tokens"] != float64(4096) {
			t.Errorf("bad limits: %+v", data)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"choices":[{"finish_reason":"length","message":{"content":"{}"}}]}`))}, nil
	})}}
	if _, err := p.Complete(context.Background(), "s", "u"); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("expected truncation error: %v", err)
	}
}

func TestCompactCandidatesResolveQuotesToWordTimes(t *testing.T) {
	tr := Transcript{Segments: []Segment{{ID: 4, Start: 10, End: 20, Text: "primeira frase clara"}, {ID: 5, Start: 40, End: 50, Text: "e este final chegou"}}, Words: []Word{{Word: "primeira", Start: 11, End: 12}, {Word: "frase", Start: 12, End: 13}, {Word: "clara", Start: 13, End: 14}, {Word: "e", Start: 45, End: 46}, {Word: "este", Start: 46, End: 47}, {Word: "final", Start: 47, End: 48}, {Word: "chegou", Start: 48, End: 49}}}
	raw := `{"clips":[{"start_segment":4,"end_segment":5,"start_quote":"frase clara","end_quote":"final chegou","title":"title"}]}`
	got, err := ParseCompactCandidates(Config{}, "s", "v", raw, tr)
	if err != nil || len(got) != 1 || got[0].Start != 12 || got[0].End != 49 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	bad := strings.Replace(raw, `"end_segment":5`, `"end_segment":99`, 1)
	if clips, err := ParseCompactCandidates(Config{}, "s", "v", bad, tr); err != nil || len(clips) != 0 {
		t.Fatal("unknown segment accepted or failed the whole window")
	}
	bad = strings.Replace(raw, "final chegou", "não existe", 1)
	if clips, err := ParseCompactCandidates(Config{}, "s", "v", bad, tr); err != nil || len(clips) != 0 {
		t.Fatal("invented boundary accepted or failed the whole window")
	}
}

func TestCompactPromptUsesSegmentIDsWithoutTimestampPairs(t *testing.T) {
	cfg := Config{}
	cfg.Selector.PromptFormat = "compact"
	tr := Transcript{Segments: []Segment{{ID: 7, Start: 10.5, End: 20.5, Text: "fala literal"}}}
	got := SelectorPrompt(cfg, "s", Episode{}, tr)
	if !strings.Contains(got, "[7] fala literal") || strings.Contains(got, "[10.5-20.5]") {
		t.Fatalf("unexpected compact prompt: %s", got)
	}
}

func TestSelectionWindowSizeChangesNumberOfCalls(t *testing.T) {
	previous := http.DefaultTransport
	defer func() { http.DefaultTransport = previous }()
	requests := 0
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"{\"clips\":[]}"}}]}`))}, nil
	})
	t.Setenv("TEST_SELECTOR_KEY", "test")
	cfg := Config{}
	cfg.Selector.Provider = "openai_compat"
	cfg.Selector.APIKeyEnv = "TEST_SELECTOR_KEY"
	cfg.Selector.BaseURL = "https://example.invalid"
	cfg.Clip.TargetSecondsMin = 45
	cfg.Clip.TargetSecondsMax = 60
	tr := Transcript{DurationS: 1146, Segments: []Segment{{Start: 0, End: 1146, Text: "text"}}, Words: []Word{{Word: "text", Start: 0, End: 1}}}
	for _, tc := range []struct{ window, want int }{{180, 10}, {360, 4}} {
		requests = 0
		cfg.Selector.WindowSeconds = tc.window
		if _, err := Select(context.Background(), cfg, Episode{VideoID: "v", DurationS: 1146}, tr); err != nil || requests != tc.want {
			t.Fatalf("window=%d calls=%d expected=%d err=%v", tc.window, requests, tc.want, err)
		}
	}
	cfg.Selector.WindowSeconds = 60
	if _, err := Select(context.Background(), cfg, Episode{VideoID: "v"}, tr); err == nil {
		t.Fatal("window no larger than overlap accepted")
	}
}

func TestSelectionConcurrencyIsBoundedAndRunsWindowsTogether(t *testing.T) {
	previous := http.DefaultTransport
	defer func() { http.DefaultTransport = previous }()
	var active, maxActive, calls int32
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		n := atomic.AddInt32(&active, 1)
		defer atomic.AddInt32(&active, -1)
		atomic.AddInt32(&calls, 1)
		for {
			m := atomic.LoadInt32(&maxActive)
			if n <= m || atomic.CompareAndSwapInt32(&maxActive, m, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"{\"clips\":[]}"}}]}`))}, nil
	})
	t.Setenv("TEST_SELECTOR_KEY", "test")
	cfg := Config{}
	cfg.Selector.Provider = "openai_compat"
	cfg.Selector.APIKeyEnv = "TEST_SELECTOR_KEY"
	cfg.Selector.BaseURL = "https://example.invalid"
	cfg.Selector.Concurrency = 2
	cfg.Clip.TargetSecondsMin = 45
	cfg.Clip.TargetSecondsMax = 60
	tr := Transcript{DurationS: 300, Words: []Word{{Word: "fala", Start: 0, End: 1}}, Segments: []Segment{{Start: 0, End: 300, Text: "fala"}}}
	if _, err := Select(context.Background(), cfg, Episode{VideoID: "v", DurationS: 300}, tr); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || maxActive != 2 {
		t.Fatalf("calls=%d max simultaneous=%d", calls, maxActive)
	}
}

func TestConcurrentSelectionPreservesSuccessfulNeighborOnRateLimit(t *testing.T) {
	previous := http.DefaultTransport
	defer func() { http.DefaultTransport = previous }()
	var calls, limited int32
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "entre 120.0 e 300.0") && atomic.CompareAndSwapInt32(&limited, 0, 1) {
			time.Sleep(100 * time.Millisecond)
			return &http.Response{StatusCode: 429, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("rate limited"))}, nil
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"{\"clips\":[]}"}}]}`))}, nil
	})
	t.Setenv("TEST_SELECTOR_KEY", "test")
	cfg := Config{}
	cfg.Storage.Transcripts = t.TempDir()
	cfg.Selector.Provider = "openai_compat"
	cfg.Selector.APIKeyEnv = "TEST_SELECTOR_KEY"
	cfg.Selector.BaseURL = "https://example.invalid"
	cfg.Selector.Concurrency = 2
	cfg.Clip.TargetSecondsMin = 45
	cfg.Clip.TargetSecondsMax = 60
	tr := Transcript{DurationS: 500, Words: []Word{{Word: "fala", Start: 0, End: 1}}, Segments: []Segment{{Start: 0, End: 500, Text: "fala"}}}
	ep := Episode{SourceID: "s", VideoID: "v", DurationS: 500}
	_, err := Select(context.Background(), cfg, ep, tr)
	var providerErr *ProviderHTTPError
	if !errors.As(err, &providerErr) || providerErr.StatusCode != 429 || atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	b, err := os.ReadFile(filepath.Join(cfg.Storage.Transcripts, "s__v.selection-progress.json"))
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint struct {
		Windows map[string]json.RawMessage `json:"windows"`
	}
	if err = json.Unmarshal(b, &checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, ok := checkpoint.Windows["0.000-180.000"]; !ok {
		t.Fatal("successful window not checkpointed")
	}
	if _, err = Select(context.Background(), cfg, ep, tr); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&calls) != 5 {
		t.Fatalf("expected only three missing windows on retry; calls=%d", calls)
	}
}

func TestConcurrentFastSelectionStopsAfterApprovedCut(t *testing.T) {
	previous := http.DefaultTransport
	defer func() { http.DefaultTransport = previous }()
	var selections int32
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		content := `{"clips":[]}`
		if strings.Contains(string(b), "Avalie cada corte") {
			content = `{"reviews":[{"index":0,"complete":true,"hook_strong":true}]}`
		} else {
			atomic.AddInt32(&selections, 1)
			if strings.Contains(string(b), "entre 0.0 e 180.0") {
				content = `{"clips":[{"start":10,"end":60,"score":0.8,"title":"first"` + testStory + `}]}`
			}
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, content)))}, nil
	})
	t.Setenv("TEST_SELECTOR_KEY", "test")
	cfg := Config{}
	cfg.Storage.Transcripts = t.TempDir()
	cfg.Selector.Provider = "openai_compat"
	cfg.Selector.APIKeyEnv = "TEST_SELECTOR_KEY"
	cfg.Selector.BaseURL = "https://example.invalid"
	cfg.Selector.Concurrency = 2
	cfg.Selector.SelectionMode = "fast"
	cfg.Selector.TargetCuts = 1
	cfg.Selector.CandidatesPerEpisode = 5
	cfg.Clip.TargetSecondsMin = 45
	cfg.Clip.TargetSecondsMax = 60
	tr := Transcript{DurationS: 500, Words: storyWords(10), Segments: []Segment{{Start: 0, End: 500, Text: "test"}}}
	ep := Episode{SourceID: "s", VideoID: "v", DurationS: 500}
	got, err := Select(context.Background(), cfg, ep, tr)
	if err != nil || len(got) != 1 || atomic.LoadInt32(&selections) != 2 {
		t.Fatalf("got=%+v selections=%d err=%v", got, selections, err)
	}
	got, err = Select(context.Background(), cfg, ep, tr)
	if err != nil || len(got) != 1 || atomic.LoadInt32(&selections) != 2 {
		t.Fatalf("retry repeated requests: got=%+v selections=%d err=%v", got, selections, err)
	}
}

func TestCompactSelectionValidatesAndReviewsWordBoundaries(t *testing.T) {
	previous := http.DefaultTransport
	defer func() { http.DefaultTransport = previous }()
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		content := `{"clips":[{"start_segment":7,"end_segment":7,"start_quote":"começo claro","end_quote":"final chegou","score":0.8,"title":"cut"` + testStory + `}]}`
		if strings.Contains(string(body), "Avalie cada corte") {
			content = `{"reviews":[{"index":0,"complete":true,"hook_strong":true}]}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, content)))}, nil
	})
	t.Setenv("TEST_SELECTOR_KEY", "test")
	cfg := Config{}
	cfg.Selector.Provider = "openai_compat"
	cfg.Selector.APIKeyEnv = "TEST_SELECTOR_KEY"
	cfg.Selector.BaseURL = "https://example.invalid"
	cfg.Selector.PromptFormat = "compact"
	cfg.Clip.TargetSecondsMin = 45
	cfg.Clip.TargetSecondsMax = 60
	tr := Transcript{DurationS: 50, Segments: []Segment{{ID: 7, Start: 0, End: 50, Text: "começo claro algo mudou final chegou"}}, Words: storyWords(0)}
	got, err := Select(context.Background(), cfg, Episode{VideoID: "v", DurationS: 50}, tr)
	if err != nil || len(got) != 1 || got[0].Start != 0 || got[0].End != 50 {
		t.Fatalf("compact selection got=%+v err=%v", got, err)
	}
}

func TestOpenRouterRequestDoesNotSendOpenCodeHeader(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://openrouter.ai/api/v1/chat/completions" {
			t.Errorf("unexpected endpoint: %s", r.URL)
		}
		if r.Header.Get("x-opencode-session") != "" {
			t.Error("OpenCode header sent to OpenRouter")
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing bearer authorization")
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"z-ai/glm-5.3"`) {
			t.Errorf("wrong model: %s", body)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"{}"}}]}`))}, nil
	})}
	p := OpenAICompatChat{APIKey: "test-key", Model: "z-ai/glm-5.3", BaseURL: "https://openrouter.ai/api/v1", SessionID: "session", Client: client}
	if _, err := p.Complete(context.Background(), "system", "user"); err != nil {
		t.Fatal(err)
	}
}

func TestSelectRejectsTranscriptWithoutWordTimestamps(t *testing.T) {
	_, err := Select(context.Background(), Config{}, Episode{VideoID: "v"}, Transcript{Segments: []Segment{{Text: "texto"}}})
	if err == nil || !strings.Contains(err.Error(), "no word timestamps") {
		t.Fatalf("expected missing word timestamps error, got %v", err)
	}
}

func TestFastSelectionStopsOnlyAfterReviewedTarget(t *testing.T) {
	requests, reviews := 0, 0
	previous := http.DefaultTransport
	defer func() { http.DefaultTransport = previous }()
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		var content string
		if strings.Contains(string(b), "Avalie cada corte") {
			reviews++
			content = fmt.Sprintf(`{"reviews":[{"index":0,"complete":%t,"hook_strong":true}]}`, reviews > 1)
		} else {
			requests++
			start := 10.0
			if requests > 1 {
				start = 200
			}
			content = fmt.Sprintf(`{"clips":[{"start":%.0f,"end":%.0f,"score":0.8,"title":"cut"%s}]}`, start, start+50, testStory)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, content)))}, nil
	})
	t.Setenv("TEST_SELECTOR_KEY", "secret")
	cfg := Config{}
	cfg.Storage.Transcripts = t.TempDir()
	cfg.Selector.Provider, cfg.Selector.APIKeyEnv, cfg.Selector.BaseURL = "openai_compat", "TEST_SELECTOR_KEY", "https://example.invalid/v1"
	cfg.Selector.CandidatesPerEpisode, cfg.Selector.SelectionMode, cfg.Selector.TargetCuts = 5, "fast", 1
	cfg.Clip.TargetSecondsMin, cfg.Clip.TargetSecondsMax = 45, 60
	tr := Transcript{DurationS: 500, Words: storyWords(10, 200), Segments: []Segment{{Start: 0, End: 150, Text: "a"}, {Start: 150, End: 500, Text: "b"}}}
	got, err := Select(context.Background(), cfg, Episode{VideoID: "v", SourceID: "s", DurationS: 500}, tr)
	if err != nil || len(got) != 1 || got[0].Start != 200 || requests != 2 || reviews != 2 {
		t.Fatalf("got=%+v requests=%d reviews=%d err=%v", got, requests, reviews, err)
	}
	got, err = Select(context.Background(), cfg, Episode{VideoID: "v", SourceID: "s", DurationS: 500}, tr)
	if err != nil || len(got) != 1 || got[0].Start != 200 || requests != 2 || reviews != 2 {
		t.Fatalf("retry repeated work: got=%+v requests=%d reviews=%d err=%v", got, requests, reviews, err)
	}
}

func TestFastSelectionReviewsHighestScoringDuplicateFirst(t *testing.T) {
	previous := http.DefaultTransport
	defer func() { http.DefaultTransport = previous }()
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		content := `{"clips":[{"start":10,"end":60,"score":0.2,"title":"low"` + testStory + `},{"start":10,"end":60,"score":0.9,"title":"high"` + testStory + `}]}`
		if strings.Contains(string(b), "Avalie cada corte") {
			content = `{"reviews":[{"index":0,"complete":true,"hook_strong":true}]}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, content)))}, nil
	})
	t.Setenv("TEST_SELECTOR_KEY", "test")
	cfg := Config{}
	cfg.Selector.Provider = "openai_compat"
	cfg.Selector.APIKeyEnv = "TEST_SELECTOR_KEY"
	cfg.Selector.BaseURL = "https://example.invalid"
	cfg.Selector.CandidatesPerEpisode = 5
	cfg.Selector.SelectionMode = "fast"
	cfg.Selector.TargetCuts = 1
	cfg.Clip.TargetSecondsMin = 45
	cfg.Clip.TargetSecondsMax = 60
	tr := Transcript{DurationS: 90, Words: storyWords(10), Segments: []Segment{{Start: 0, End: 90, Text: "test"}}}
	got, err := Select(context.Background(), cfg, Episode{VideoID: "v", DurationS: 90}, tr)
	if err != nil || len(got) != 1 || got[0].Title != "high" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestFastSelectionCanReviewAlternativeAfterRejectedDuplicate(t *testing.T) {
	previous := http.DefaultTransport
	defer func() { http.DefaultTransport = previous }()
	reviews := 0
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		content := `{"clips":[{"start":10,"end":60,"score":0.2,"title":"alternative"` + testStory + `},{"start":10,"end":60,"score":0.9,"title":"first"` + testStory + `}]}`
		if strings.Contains(string(b), "Avalie cada corte") {
			reviews++
			content = fmt.Sprintf(`{"reviews":[{"index":0,"complete":%t,"hook_strong":true}]}`, reviews > 1)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, content)))}, nil
	})
	t.Setenv("TEST_SELECTOR_KEY", "test")
	cfg := Config{}
	cfg.Selector.Provider = "openai_compat"
	cfg.Selector.APIKeyEnv = "TEST_SELECTOR_KEY"
	cfg.Selector.BaseURL = "https://example.invalid"
	cfg.Selector.CandidatesPerEpisode = 5
	cfg.Selector.SelectionMode = "fast"
	cfg.Selector.TargetCuts = 1
	cfg.Clip.TargetSecondsMin = 45
	cfg.Clip.TargetSecondsMax = 60
	tr := Transcript{DurationS: 90, Words: storyWords(10), Segments: []Segment{{Start: 0, End: 90, Text: "test"}}}
	got, err := Select(context.Background(), cfg, Episode{VideoID: "v", DurationS: 90}, tr)
	if err != nil || len(got) != 1 || got[0].Title != "alternative" || reviews != 2 {
		t.Fatalf("got=%+v reviews=%d err=%v", got, reviews, err)
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

func TestHookMayContinueBeyondOpeningThreeSeconds(t *testing.T) {
	words := []Word{{Word: "uma", Start: 1, End: 2}, {Word: "ideia", Start: 2, End: 3}, {Word: "importante", Start: 3, End: 4}}
	c := Candidate{Start: 0, End: 10, HookQuote: "uma ideia importante"}
	if !hasHookEvidence(c, words) {
		t.Fatal("literal hook begins inside opening but ends after three seconds")
	}
	c.Start = 0
	words[0].Start = 3
	words[0].End = 3.2
	if hasHookEvidence(c, words) {
		t.Fatal("hook starting at three seconds accepted")
	}
	words = []Word{{Word: "uma", Start: 1, End: 2}, {Word: "ideia", Start: 2, End: 3}, {Word: "importante", Start: 10, End: 11}}
	if hasHookEvidence(c, words) {
		t.Fatal("hook extending outside clip accepted")
	}
}

func TestStoryEvidenceAcceptsLongExactQuotes(t *testing.T) {
	var words []Word
	var quotes [3][]string
	for i := 0; i < 3; i++ {
		for j := 0; j < 17; j++ {
			token := fmt.Sprintf("palavra%d%d", i, j)
			at := float64(i*20+j) / 2
			words = append(words, Word{Word: token, Start: at, End: at + 0.4})
			quotes[i] = append(quotes[i], token)
		}
	}
	c := Candidate{Start: 0, End: words[len(words)-1].End, Story: StoryEvidence{SetupQuote: strings.Join(quotes[0], " "), DevelopmentQuote: strings.Join(quotes[1], " "), ConclusionQuote: strings.Join(quotes[2], " ")}}
	if !hasStoryEvidence(c, words) {
		t.Fatal("long, exact, ordered story evidence rejected")
	}
	c.Story.ConclusionQuote += " inventado"
	if hasStoryEvidence(c, words) {
		t.Fatal("invented conclusion accepted")
	}
}

func TestAlignWordBoundariesIncludesEntireWords(t *testing.T) {
	words := []Word{{Word: "começo", Start: 2, End: 3}, {Word: "claro", Start: 3, End: 4}, {Word: "fim", Start: 49, End: 50}}
	c := alignWordBoundaries(Candidate{Start: 2.5, End: 49.5}, words)
	if c.Start != 2 || c.End != 50 {
		t.Fatalf("unexpected boundaries: %.2f–%.2f", c.Start, c.End)
	}
}

func TestRepairCandidateBoundariesUsesUniqueLiteralEvidence(t *testing.T) {
	cfg := Config{}
	cfg.Clip.TargetSecondsMin = 45
	cfg.Clip.TargetSecondsMax = 60
	tr := Transcript{DurationS: 180, Words: storyWords(10)}
	c := Candidate{Start: 13, End: 59, Score: 0.8, Title: "cut", HookQuote: "começo claro", AttentionSignals: []string{"curiosity"}, ViralityReason: "reason", Story: StoryEvidence{SetupQuote: "começo claro", DevelopmentQuote: "algo mudou", ConclusionQuote: "final chegou"}}
	got, ok := repairCandidateBoundaries(c, tr, cfg, 0, 180)
	if !ok || got.Start != 10 || got.End != 60 || !validCandidate(got, tr, cfg, 0, 180) {
		t.Fatalf("repair got=%+v ok=%t", got, ok)
	}
	if _, ok := repairCandidateBoundaries(c, tr, cfg, 11, 180); ok {
		t.Fatal("repair escaped selection window")
	}
	c.HookQuote = "palavras inventadas"
	if _, ok := repairCandidateBoundaries(c, tr, cfg, 0, 180); ok {
		t.Fatal("invented hook repaired")
	}
}

func TestRepairRejectsAmbiguousAndExcessiveShifts(t *testing.T) {
	cfg := Config{}
	cfg.Clip.TargetSecondsMin = 45
	cfg.Clip.TargetSecondsMax = 60
	tr := Transcript{DurationS: 180, Words: storyWords(10)}
	c := Candidate{Start: 19, End: 59, Score: 0.8, Title: "cut", HookQuote: "começo claro", AttentionSignals: []string{"curiosity"}, ViralityReason: "reason", Story: StoryEvidence{SetupQuote: "começo claro", DevelopmentQuote: "algo mudou", ConclusionQuote: "final chegou"}}
	if _, ok := repairCandidateBoundaries(c, tr, cfg, 0, 180); ok {
		t.Fatal("repair moved boundary more than eight seconds")
	}
	tr.Words = append(tr.Words, Word{Word: "começo", Start: 14, End: 15}, Word{Word: "claro", Start: 15, End: 16})
	c.Start = 13
	if _, ok := repairCandidateBoundaries(c, tr, cfg, 0, 180); ok {
		t.Fatal("ambiguous hook accepted")
	}
}

func TestCandidateFailureReasonsIdentifyOverlappingChecks(t *testing.T) {
	cfg := Config{}
	cfg.Clip.TargetSecondsMin = 45
	cfg.Clip.TargetSecondsMax = 60
	tr := Transcript{DurationS: 180, Words: storyWords(10)}
	c := Candidate{Start: 10, End: 40, Score: 0.8, Title: "cut", HookQuote: "invented words", AttentionSignals: []string{"curiosity"}, ViralityReason: "reason", Story: StoryEvidence{SetupQuote: "começo claro", DevelopmentQuote: "algo mudou", ConclusionQuote: "final chegou"}}
	reasons := candidateFailureReasons(c, tr, cfg, 0, 180)
	got := strings.Join(reasons, ",")
	for _, reason := range []string{"duration", "hook", "story"} {
		if !strings.Contains(got, reason) {
			t.Errorf("missing %s: %v", reason, reasons)
		}
	}
	if strings.Contains(got, "bounds") || validCandidate(c, tr, cfg, 0, 180) {
		t.Fatalf("bad validation: %v", reasons)
	}
}

type testChatProvider struct{ response string }

func (p testChatProvider) Complete(context.Context, string, string) (string, error) {
	return p.response, nil
}

type sequenceChatProvider struct {
	responses []string
	calls     int
}

func (p *sequenceChatProvider) Complete(context.Context, string, string) (string, error) {
	i := p.calls
	p.calls++
	if i >= len(p.responses) {
		return "", fmt.Errorf("unexpected request")
	}
	return p.responses[i], nil
}

func TestReviewRetriesIncompleteResponseWithoutAcceptingUnreviewed(t *testing.T) {
	candidates := []Candidate{{Start: 0, End: 50}, {Start: 60, End: 110}}
	tr := Transcript{Words: storyWords(0, 60)}
	p := &sequenceChatProvider{responses: []string{`{"reviews":[{"index":0,"complete":true,"hook_strong":true}]}`, `{"reviews":[{"index":0,"complete":false,"hook_strong":true},{"index":1,"complete":true,"hook_strong":true}]}`}}
	got, err := reviewStories(context.Background(), p, candidates, tr)
	if err != nil || p.calls != 2 || len(got) != 1 || got[0].Start != 60 {
		t.Fatalf("got=%+v calls=%d err=%v", got, p.calls, err)
	}
	p = &sequenceChatProvider{responses: []string{`{"reviews":[]}`, `{"reviews":[]}`}}
	if _, err := reviewStories(context.Background(), p, candidates, tr); err == nil || p.calls != 2 {
		t.Fatalf("invalid review should fail after two attempts: calls=%d err=%v", p.calls, err)
	}
	p = &sequenceChatProvider{responses: []string{`{"reviews":[{"index":0,"complete":true},{"index":1,"complete":true,"hook_strong":true}]}`, `{"reviews":[{"index":0,"complete":true},{"index":1,"complete":true,"hook_strong":true}]}`}}
	if _, err := reviewStories(context.Background(), p, candidates, tr); err == nil || p.calls != 2 {
		t.Fatal("missing hook judgment must not be accepted")
	}
}

func TestReviewStoriesRejectsIncompleteAndMissingReviews(t *testing.T) {
	candidates := []Candidate{{Start: 0, End: 50}, {Start: 60, End: 110}}
	tr := Transcript{Words: storyWords(0, 60)}
	provider := testChatProvider{response: `{"reviews":[{"index":0,"complete":false,"hook_strong":false},{"index":1,"complete":true,"hook_strong":true}]}`}
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
			body := fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, `{"reviews":[{"index":0,"complete":true,"hook_strong":true},{"index":1,"complete":true,"hook_strong":true}]}`)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
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
			body := fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, `{"reviews":[{"index":0,"complete":true,"hook_strong":true},{"index":1,"complete":true,"hook_strong":true}]}`)
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
	timingPath := filepath.Join(t.TempDir(), "timings.jsonl")
	ctx := WithTiming(context.Background(), NewTimingRecorder(timingPath, "job"))
	if _, err := Select(ctx, cfg, ep, tr); err == nil {
		t.Fatal("expected second window failure")
	}
	got, err := Select(ctx, cfg, ep, tr)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 4 || len(got) != 2 || got[0].Title != "second" || got[1].Title != "first" {
		t.Fatalf("requests=%d candidates=%+v", requests, got)
	}
	b, err := os.ReadFile(timingPath)
	if err != nil {
		t.Fatal(err)
	}
	var events []TimingEvent
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e TimingEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	if len(events) != 5 || events[0].Stage != "selection_window" || events[1].Outcome != "failed" || !events[2].Reused || events[4].Stage != "review" {
		t.Fatalf("unexpected timing events: %+v", events)
	}
}

func TestSelectorRejectsMissingClipsArray(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"clips":null}`} {
		if _, err := ParseCandidates(Config{}, "s", "v", raw); err == nil {
			t.Errorf("legacy accepted %s", raw)
		}
		if _, err := ParseCompactCandidates(Config{}, "s", "v", raw, Transcript{}); err == nil {
			t.Errorf("compact accepted %s", raw)
		}
	}
	if clips, err := ParseCandidates(Config{}, "s", "v", `{"clips":[]}`); err != nil || len(clips) != 0 {
		t.Fatalf("empty array: %v %v", clips, err)
	}
}

func TestFullSelectionReusesEditorialReviewOnRetry(t *testing.T) {
	previous := http.DefaultTransport
	defer func() { http.DefaultTransport = previous }()
	reviews := 0
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		content := `{"clips":[{"start":10,"end":60,"score":0.8,"title":"first"` + testStory + `}]}`
		if strings.Contains(string(b), "Avalie cada corte") {
			reviews++
			content = `{"reviews":[{"index":0,"complete":true,"hook_strong":true}]}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, content)))}, nil
	})
	t.Setenv("TEST_SELECTOR_KEY", "test")
	cfg := Config{}
	cfg.Storage.Transcripts = t.TempDir()
	cfg.Selector.Provider = "openai_compat"
	cfg.Selector.APIKeyEnv = "TEST_SELECTOR_KEY"
	cfg.Selector.BaseURL = "https://example.invalid"
	cfg.Clip.TargetSecondsMin = 45
	cfg.Clip.TargetSecondsMax = 60
	tr := Transcript{DurationS: 100, Words: storyWords(10), Segments: []Segment{{Start: 0, End: 100, Text: "test"}}}
	ep := Episode{SourceID: "s", VideoID: "v", DurationS: 100}
	for i := 0; i < 2; i++ {
		got, err := Select(context.Background(), cfg, ep, tr)
		if err != nil || len(got) != 1 {
			t.Fatalf("run %d: %v %v", i, got, err)
		}
	}
	if reviews != 1 {
		t.Fatalf("reviews = %d", reviews)
	}
}

func TestFullSelectionFiltersBelowMinimumScore(t *testing.T) {
	previous := http.DefaultTransport
	defer func() { http.DefaultTransport = previous }()
	selections := 0
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		content := `{"clips":[{"start":10,"end":60,"score":0.8,"title":"high"` + testStory + `},{"start":80,"end":130,"score":0.6,"title":"low"` + testStory + `}]}`
		if strings.Contains(string(b), "Avalie cada corte") {
			content = `{"reviews":[{"index":0,"complete":true,"hook_strong":true}]}`
		} else {
			selections++
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, content)))}, nil
	})
	t.Setenv("TEST_SELECTOR_KEY", "test")
	cfg := Config{}
	cfg.Storage.Transcripts = t.TempDir()
	cfg.Selector.Provider = "openai_compat"
	cfg.Selector.APIKeyEnv = "TEST_SELECTOR_KEY"
	cfg.Selector.BaseURL = "https://example.invalid"
	cfg.Selector.MinScore = 0.7
	cfg.Clip.TargetSecondsMin = 45
	cfg.Clip.TargetSecondsMax = 60
	words := append(storyWords(10), storyWords(80)...)
	tr := Transcript{DurationS: 150, Words: words, Segments: []Segment{{Start: 0, End: 150, Text: "test"}}}
	got, err := Select(context.Background(), cfg, Episode{SourceID: "s", VideoID: "v", DurationS: 150}, tr)
	if err != nil || len(got) != 1 || got[0].Title != "high" {
		t.Fatalf("got %v err %v", got, err)
	}
	cfg.Selector.MinScore = 0.5
	got, err = Select(context.Background(), cfg, Episode{SourceID: "s", VideoID: "v", DurationS: 150}, tr)
	if err != nil || len(got) != 2 || selections != 1 {
		t.Fatalf("lower threshold: got %v err %v selections %d", got, err, selections)
	}
}

func TestSelectorPromptRequestsStandaloneHonestMetadata(t *testing.T) {
	for _, format := range []string{"legacy", "compact"} {
		cfg := Config{}
		cfg.Selector.PromptFormat = format
		got := SelectorPrompt(cfg, "source", Episode{Title: "Como usar IA"}, Transcript{})
		for _, term := range []string{"without prior context", "full video", "Do not invent", "Como usar IA", "referents"} {
			if !strings.Contains(got, term) {
				t.Errorf("%s prompt missing %q", format, term)
			}
		}
		if strings.Contains(got, "política brasileira") {
			t.Errorf("%s prompt assumes political content", format)
		}
	}
}

type captureReviewPrompt struct{ prompt string }

func (p *captureReviewPrompt) Complete(_ context.Context, _, user string) (string, error) {
	p.prompt = user
	return `{"reviews":[{"index":0,"complete":true,"hook_strong":true}]}`, nil
}
func TestStoryReviewRequiresContextInsideClip(t *testing.T) {
	p := &captureReviewPrompt{}
	_, err := reviewStories(context.Background(), p, []Candidate{{Start: 0, End: 1}}, Transcript{})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"new viewer", "spoken content", "unclear referent", "outside context cannot supply missing context"} {
		if !strings.Contains(p.prompt, text) {
			t.Errorf("review missing %q", text)
		}
	}
}

func TestConversionSelectsAnchoredDifferentRange(t *testing.T) {
	cfg := Config{}
	cfg.Clip.TargetSecondsMin = 45
	cfg.Clip.TargetSecondsMax = 60
	tr := Transcript{DurationS: 150, Words: storyWords(10), Segments: []Segment{{Start: 0, End: 150, Text: "começo claro algo mudou final chegou"}}}
	ep := Episode{SourceID: "s", VideoID: "v", DurationS: 150}
	raw := `{"clips":[{"start":10,"end":60,"score":0.8,"title":"new"` + testStory + `}]}`
	for _, tc := range []struct {
		name     string
		original Candidate
		expand   bool
		want     bool
	}{{"expand", Candidate{Start: 20, End: 40}, true, true}, {"shorten", Candidate{Start: 0, End: 100}, false, true}, {"outside", Candidate{Start: 65, End: 90}, true, false}, {"unchanged", Candidate{Start: 10, End: 60}, true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			p := &sequenceChatProvider{responses: []string{raw, `{"reviews":[{"index":0,"complete":true,"hook_strong":true}]}`}}
			got, err := selectConversion(context.Background(), cfg, ep, tr, tc.original, tc.expand, p)
			if (err == nil) != tc.want {
				t.Fatalf("got %+v err %v", got, err)
			}
		})
	}
}

func TestHookMustBeginWithFirstSpokenWords(t *testing.T) {
	words := []Word{{Word: "euros.", Start: 0, End: 0.3}, {Word: "Sou", Start: 0.3, End: 0.6}, {Word: "de", Start: 0.6, End: 0.8}, {Word: "direita", Start: 0.8, End: 1.2}}
	c := Candidate{Start: 0, End: 8, HookQuote: "Sou de direita"}
	if hasOpeningHookEvidence(c, words) {
		t.Fatal("accepted a hook that skips the first spoken word")
	}
	c.HookQuote = "euros Sou de direita"
	if !hasOpeningHookEvidence(c, words) {
		t.Fatal("rejected a literal opening quote")
	}
}
func TestStoryReviewIsolatesOpening(t *testing.T) {
	p := &captureReviewPrompt{}
	tr := Transcript{Words: []Word{{Word: "agora", Start: 0, End: 0.5}, {Word: "atenção", Start: 2.4, End: 2.9}, {Word: "depois", Start: 3.2, End: 3.6}}}
	_, err := reviewStories(context.Background(), p, []Candidate{{Start: 0, End: 10}}, tr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.prompt, "FIRST THREE SECONDS: agora atenção") || strings.Contains(p.prompt, "FIRST THREE SECONDS: agora atenção depois") {
		t.Fatalf("opening not isolated: %s", p.prompt)
	}
}
