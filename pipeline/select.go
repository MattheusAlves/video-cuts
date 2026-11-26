package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Candidate is a chosen moment inside an episode.
type Candidate struct {
	VideoID          string        `json:"video_id"`
	SourceID         string        `json:"source_id"`
	Start            float64       `json:"start"`
	End              float64       `json:"end"`
	Score            float32       `json:"score"`      // 0..1 estimated retention potential
	HookQuote        string        `json:"hook_quote"` // literal opening line
	AttentionSignals []string      `json:"attention_signals"`
	ViralityReason   string        `json:"virality_reason"`
	Title            string        `json:"title"`       // Portuguese
	Description      string        `json:"description"` // Portuguese
	Hashtags         []string      `json:"hashtags"`
	Rationale        string        `json:"rationale"`
	Story            StoryEvidence `json:"story"`
}

type StoryEvidence struct {
	SetupQuote       string `json:"setup_quote"`
	DevelopmentQuote string `json:"development_quote"`
	ConclusionQuote  string `json:"conclusion_quote"`
}

// SelectorPrompt builds the LLM instruction for picking moments.
// It expects a Portuguese politics transcript and a target duration.
func SelectorPrompt(cfg Config, sourceID string, ep Episode, t Transcript) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Source video title (background only): %s\n", ep.Title)
	fmt.Fprintln(&b, "Choose cuts that a new viewer can understand without prior context: include the subject, who or what is being discussed, and the essential setup within the spoken clip itself. Resolve unclear pronouns and referents by including the necessary earlier words; reject a moment if that cannot fit without losing its natural opening, story, and payoff. The title and description cannot repair a clip that depends on omitted context.")
	fmt.Fprintln(&b, "Write the title and description in Brazilian Portuguese for a viewer who has not seen the source. The title must stand alone, name the subject or concrete stakes, and create curiosity about the clip and full video without relying on vague pronouns, missing names, or unexplained references. The description must briefly explain the relevant context and give an honest reason to watch the full video. Do not invent facts, identities, outcomes or promises about material outside the supplied transcript. Do not write a URL or hashtags inside the description; these are added separately.")
	fmt.Fprintln(&b, "Score each candidate independently for its suitability to the requested YouTube format, based only on this transcript. Consider the opening's immediate ability to stop a scroll, clarity without prior context, tension or curiosity, development and payoff, and whether the moment rewards continued attention. Use the full 0–1 scale: below 0.5 weak, 0.5–0.69 ordinary, 0.7–0.84 strong, 0.85–1 exceptional. Explain the concrete evidence and limitations in virality_reason; do not promise virality or infer visuals or audience behavior from text. Scores are editorial judgments, not probabilities.")
	if cfg.Selector.PromptFormat == "compact" {
		fmt.Fprintf(&b, "Selecione até %d cortes independentes de %d a %d segundos em português. Preserve contexto e sentido. Cada corte deve abrir com gancho literal forte, ter desenvolvimento e conclusão completa. Evite publicidade, saudações e frases incompletas. Não invente fatos.\n", cfg.Selector.CandidatesPerEpisode, cfg.Clip.TargetSecondsMin, cfg.Clip.TargetSecondsMax)
		fmt.Fprintln(&b, "Cada linha tem [ID] texto e tempo inicial aproximado em segundos. Para cada corte, informe os IDs de início e fim e citações literais curtas start_quote e end_quote que marcam exatamente suas primeiras e últimas palavras. As citações devem aparecer nas linhas correspondentes.")
		fmt.Fprintln(&b, "Retorne JSON: {\"clips\":[{start_segment,end_segment,start_quote,end_quote,score,hook_quote,attention_signals,virality_reason,title,description,hashtags,rationale,story:{setup_quote,development_quote,conclusion_quote}}]}. O gancho deve começar antes de três segundos e terminar dentro do corte. Citações do gancho e da história devem ter entre 2 e 60 palavras literais. As três citações da história devem ser distintas e ordenadas; a conclusão deve terminar o corte. Se não houver cortes completos, retorne lista vazia.")
		for _, s := range t.Segments {
			fmt.Fprintf(&b, "[%d] %s (%.0fs)\n", s.ID, strings.TrimSpace(s.Text), s.Start)
		}
		return b.String()
	}
	fmt.Fprintf(&b, "You are a video editor for channel %s. Use the source topic and context, not assumptions about the subject.\n", cfg.Channel.Name)
	if cfg.Clip.LongForm {
		fmt.Fprintf(&b, "Produza um vídeo longo para o YouTube, não um Short. A saída é horizontal e deve funcionar como um vídeo autônomo.\n")
	}
	fmt.Fprintf(&b, "Fonte deste episódio: %s (canal: %s)\n", ep.Title, sourceID)
	fmt.Fprintf(&b, "Duração total: %.0f segundos\n\n", ep.DurationS)
	fmt.Fprintf(&b, "TAREFA:\n")
	formatName := "Shorts"
	if cfg.Clip.LongForm {
		formatName = "vídeo longo do YouTube"
	}
	fmt.Fprintf(&b, "1. Escolha até %d momentos com duração entre %d e %d segundos que maximizem retenção em %s. Priorize potencial de manter a pessoa assistindo, não apenas qualidade ou importância geral.\n",
		cfg.Selector.CandidatesPerEpisode, cfg.Clip.TargetSecondsMin, cfg.Clip.TargetSecondsMax, formatName)
	fmt.Fprintf(&b, "2. Procure sinais concretos de atenção: surpresa/revelação, opinião forte ou contraintuitiva, conflito/contestação, reação emocional espontânea, frase memorável, virada/desfecho ou dica prática. Não chame algo de viral só por ser polêmico.\n")
	fmt.Fprintf(&b, "3. Todo corte precisa abrir com um gancho falado forte nos primeiros 3 segundos do próprio corte. Comece o corte imediatamente antes dessa fala. O gancho precisa criar curiosidade, surpresa, tensão ou reação sem distorcer o sentido; não invente uma chamada.\n")
	fmt.Fprintf(&b, "4. Cada corte deve contar uma história ou argumento completo: apresente o contexto necessário, desenvolva a ideia e mostre a conclusão ou desfecho antes do final. Um gancho forte nunca substitui o desfecho.\n")
	fmt.Fprintf(&b, "5. Evite: saudações, recados comerciais, blocos de leitura de comentários, segmentos que dependem de 1+ min de contexto anterior.\n")
	fmt.Fprintf(&b, "6. Comece e termine em pausas naturais, sem cortar palavras ou frases. Não termine antes da resposta, consequência ou conclusão; rejeite trechos que dependam de continuação fora do corte. Se nenhuma história couber inteira na duração, devolva clips vazio.\n")
	fmt.Fprintf(&b, "Não invente fatos, causalidade ou contexto; título e descrição devem refletir fielmente o que é dito no corte. Evite promessas e generalizações sem evidência.\n")
	fmt.Fprintf(&b, "7. Para cada momento devolva:\n")
	fmt.Fprintf(&b, "   - start (segundos, float)\n")
	fmt.Fprintf(&b, "   - end (segundos, float)\n")
	fmt.Fprintf(&b, "   - score (0.0–1.0 de potencial estimado de retenção)\n")
	fmt.Fprintf(&b, "   - hook_quote (citação literal de 2 a 60 palavras que começa antes de 3 segundos e termina dentro do corte)\n")
	fmt.Fprintf(&b, "   - attention_signals (1-3 itens entre curiosity, surprise, strong_opinion, conflict, emotional_reaction, revelation, quotable_line, story_payoff, practical_value)\n")
	fmt.Fprintf(&b, "   - virality_reason (PT-BR, uma frase específica explicando por que pode parar a rolagem/manter atenção; não prometa viralização)\n")
	fmt.Fprintf(&b, "   - title (PT-BR <= 90 chars, com 1 emoji no máximo)\n")
	fmt.Fprintf(&b, "   - description (PT-BR 1-2 frases)\n")
	fmt.Fprintf(&b, "   - hashtags (3-5 strings relevantes ao tema do vídeo)\n")
	fmt.Fprintf(&b, "   - rationale (PT-BR, 1 frase curta resumindo o motivo editorial)\n\n")
	fmt.Fprintf(&b, "   - story: objeto com setup_quote, development_quote e conclusion_quote. Cada um deve ser uma citação literal distinta de 2 a 60 palavras, na ordem em que é falada dentro do corte. A primeira deve aparecer nos primeiros 15 segundos; a conclusão deve conter as últimas palavras do corte, sem começar outra frase que não termine.\n\n")
	fmt.Fprintf(&b, "TRANSCRIÇÃO COM TIMESTAMPS (formato: [start-end] texto):\n")
	// Combine ~10s chunks of segments so the LLM sees timing clearly.
	for _, s := range t.Segments {
		fmt.Fprintf(&b, "[%.1f-%.1f] %s\n", s.Start, s.End, strings.TrimSpace(s.Text))
	}
	fmt.Fprintf(&b, "\nDevolva APENAS um JSON com a chave \"clips\": [ {start, end, score, hook_quote, attention_signals, virality_reason, title, description, hashtags, rationale, story: {setup_quote, development_quote, conclusion_quote}}, ... ]\n")
	fmt.Fprintf(&b, "Não inclua markdown. Não inclua comentários fora do JSON.\n")
	return b.String()
}

// ParseCompactCandidates resolves quoted boundaries only within the specified
// segments. Missing, invented, or ambiguous boundaries are rejected.
func ParseCompactCandidates(cfg Config, sourceID, videoID, raw string, t Transcript) ([]Candidate, error) {
	var bounds struct {
		Clips []struct {
			StartSegment *int   `json:"start_segment"`
			EndSegment   *int   `json:"end_segment"`
			StartQuote   string `json:"start_quote"`
			EndQuote     string `json:"end_quote"`
		} `json:"clips"`
	}
	if err := json.Unmarshal([]byte(raw), &bounds); err != nil {
		return nil, err
	}
	candidates, err := ParseCandidates(cfg, sourceID, videoID, raw)
	if err != nil {
		return nil, err
	}
	segment := func(id int) (Segment, bool) {
		var found Segment
		n := 0
		for _, s := range t.Segments {
			if s.ID == id {
				found = s
				n++
			}
		}
		return found, n == 1
	}
	resolved := make([]Candidate, 0, len(candidates))
	for i, item := range bounds.Clips {
		if item.StartSegment == nil || item.EndSegment == nil {
			log.Printf("selector: compact candidate %d missing segment IDs", i)
			continue
		}
		startSeg, okStart := segment(*item.StartSegment)
		endSeg, okEnd := segment(*item.EndSegment)
		if !okStart || !okEnd || startSeg.Start > endSeg.Start {
			log.Printf("selector: compact candidate %d has invalid segment IDs", i)
			continue
		}
		start, _, okStart := uniqueQuoteInSegment(item.StartQuote, startSeg, t.Words)
		_, end, okEnd := uniqueQuoteInSegment(item.EndQuote, endSeg, t.Words)
		if !okStart || !okEnd || end <= start {
			log.Printf("selector: compact candidate %d has invalid or ambiguous boundary quote", i)
			continue
		}
		candidates[i].Start, candidates[i].End = start, end
		resolved = append(resolved, candidates[i])
	}
	return resolved, nil
}

func uniqueQuoteInSegment(quote string, segment Segment, words []Word) (float64, float64, bool) {
	phrase := normalizedWords(quote)
	if len(phrase) == 0 || len(phrase) > 15 {
		return 0, 0, false
	}
	var tokens []string
	var starts, ends []float64
	for _, word := range words {
		if word.End <= segment.Start || word.Start >= segment.End {
			continue
		}
		for _, token := range normalizedWords(word.Word) {
			tokens = append(tokens, token)
			starts = append(starts, word.Start)
			ends = append(ends, word.End)
		}
	}
	count := 0
	var start, end float64
	for i := 0; i+len(phrase) <= len(tokens); i++ {
		match := true
		for j := range phrase {
			if tokens[i+j] != phrase[j] {
				match = false
				break
			}
		}
		if match {
			count++
			start = starts[i]
			end = ends[i+len(phrase)-1]
		}
	}
	return start, end, count == 1
}

// ChatProvider is anything that can return a chat completion for a single user prompt.
type ChatProvider interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// ProviderHTTPError preserves status codes so callers can distinguish account
// or quota failures from an error specific to one episode.
type ProviderHTTPError struct {
	StatusCode int
	Body       string
}

func (e *ProviderHTTPError) Error() string {
	return fmt.Sprintf("chat status %d: %s", e.StatusCode, e.Body)
}

// OpenAICompatChat calls an OpenAI-compatible chat completions endpoint.
type OpenAICompatChat struct {
	APIKey          string
	Model           string
	BaseURL         string
	SessionID       string
	Client          *http.Client
	ReasoningEffort string
	MaxTokens       int
}

func (g *OpenAICompatChat) Complete(ctx context.Context, system, user string) (string, error) {
	if g.APIKey == "" {
		return "", fmt.Errorf("selector API key is missing")
	}
	client := g.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	payload := map[string]any{
		"model": g.Model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"temperature":     0.4,
		"response_format": map[string]string{"type": "json_object"},
	}
	if g.ReasoningEffort != "" {
		payload["reasoning"] = map[string]string{"effort": g.ReasoningEffort}
	}
	if g.MaxTokens > 0 {
		payload["max_tokens"] = g.MaxTokens
	}
	body, _ := json.Marshal(payload)
	baseURL := strings.TrimRight(g.BaseURL, "/")
	if baseURL == "" {
		return "", fmt.Errorf("selector.base_url is required")
	}
	if !strings.HasSuffix(baseURL, "/chat/completions") {
		baseURL += "/chat/completions"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+g.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "yt-short/1.0")
	endpoint, _ := url.Parse(baseURL)
	if g.SessionID != "" && endpoint != nil && strings.EqualFold(endpoint.Hostname(), "opencode.ai") {
		req.Header.Set("x-opencode-session", g.SessionID)
	}
	var observedUsage *ProviderUsage
	defer func() { ReportProviderUsage(ctx, observedUsage) }()
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", &ProviderHTTPError{StatusCode: resp.StatusCode, Body: string(raw)}
	}
	var out struct {
		Model    string `json:"model"`
		Provider string `json:"provider"`
		Usage    *struct {
			PromptTokens     int      `json:"prompt_tokens"`
			CompletionTokens int      `json:"completion_tokens"`
			Cost             *float64 `json:"cost"`
			PromptDetails    struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		detail := strings.TrimSpace(string(raw))
		if len(detail) > 500 {
			detail = detail[:500]
		}
		return "", fmt.Errorf("parse chat response: %w (body: %s)", err, detail)
	}
	if out.Usage != nil {
		usage := ProviderUsage{PromptTokens: out.Usage.PromptTokens, CompletionTokens: out.Usage.CompletionTokens, ReasoningTokens: out.Usage.CompletionDetails.ReasoningTokens, CachedTokens: out.Usage.PromptDetails.CachedTokens, Cost: out.Usage.Cost, Model: out.Model, Provider: out.Provider}
		observedUsage = &usage
		StartTiming(ctx, TimingEvent{Stage: "provider_usage", Usage: &usage})(nil)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("no choices")
	}
	if out.Choices[0].FinishReason == "length" {
		return "", fmt.Errorf("provider response truncated at output token limit")
	}
	return out.Choices[0].Message.Content, nil
}

// ParseCandidates extracts the JSON shape we asked the LLM for.
func ParseCandidates(cfg Config, sourceID string, videoID string, raw string) ([]Candidate, error) {
	var resp struct {
		Clips []struct {
			Start            float64       `json:"start"`
			End              float64       `json:"end"`
			Score            float32       `json:"score"`
			HookQuote        string        `json:"hook_quote"`
			AttentionSignals []string      `json:"attention_signals"`
			ViralityReason   string        `json:"virality_reason"`
			Title            string        `json:"title"`
			Description      string        `json:"description"`
			Hashtags         []string      `json:"hashtags"`
			Rationale        string        `json:"rationale"`
			Story            StoryEvidence `json:"story"`
		} `json:"clips"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		detail := strings.TrimSpace(raw)
		if len(detail) > 200 {
			detail = detail[:200]
		}
		return nil, fmt.Errorf("selector returned invalid JSON: %w (%s)", err, detail)
	}
	if resp.Clips == nil {
		return nil, fmt.Errorf("selector response requires a clips array")
	}
	out := make([]Candidate, 0, len(resp.Clips))
	for _, c := range resp.Clips {
		out = append(out, Candidate{
			VideoID:          videoID,
			SourceID:         sourceID,
			Start:            c.Start,
			End:              c.End,
			Score:            c.Score,
			HookQuote:        strings.TrimSpace(c.HookQuote),
			AttentionSignals: normalizeAttentionSignals(c.AttentionSignals),
			ViralityReason:   strings.TrimSpace(c.ViralityReason),
			Title:            strings.TrimSpace(c.Title),
			Description:      strings.TrimSpace(c.Description),
			Hashtags:         c.Hashtags,
			Rationale:        c.Rationale,
			Story:            c.Story,
		})
	}
	return out, nil
}

// Select is the top-level entry point used by main.go.
func Select(ctx context.Context, cfg Config, ep Episode, t Transcript) ([]Candidate, error) {
	if cfg.Selector.MinScore < 0 || cfg.Selector.MinScore > 1 || math.IsNaN(float64(cfg.Selector.MinScore)) {
		return nil, fmt.Errorf("minimum score must be between 0 and 1")
	}
	if len(t.Segments) == 0 {
		return nil, fmt.Errorf("transcript %s: no segments", ep.VideoID)
	}
	if len(t.Words) == 0 {
		return nil, fmt.Errorf("transcript %s: no word timestamps", ep.VideoID)
	}
	if cfg.Selector.Provider != "openai_compat" {
		return nil, fmt.Errorf("unknown selector provider %q", cfg.Selector.Provider)
	}
	keyEnv := cfg.Selector.APIKeyEnv
	if keyEnv == "" {
		return nil, fmt.Errorf("selector.api_key_env is required")
	}
	provider := &OpenAICompatChat{
		APIKey: os.Getenv(keyEnv), Model: cfg.Selector.ChatModel,
		BaseURL: cfg.Selector.BaseURL, SessionID: "ses_ytshort_" + ep.VideoID,
		ReasoningEffort: cfg.Selector.ReasoningEffort, MaxTokens: cfg.Selector.MaxTokens,
	}

	system := "Você é um assistente que seleciona cortes para o YouTube Shorts."
	windowSeconds := 180.0
	if cfg.Clip.LongForm {
		// A window is 1.5× the maximum cut, with half a maximum of overlap.
		// That guarantees a candidate up to the configured maximum fits inside
		// at least one window while keeping calls far apart.
		windowSeconds = float64(cfg.Clip.TargetSecondsMax) * 1.5
	}
	if cfg.Selector.PromptFormat != "" && cfg.Selector.PromptFormat != "legacy" && cfg.Selector.PromptFormat != "compact" {
		return nil, fmt.Errorf("invalid selector prompt format %q", cfg.Selector.PromptFormat)
	}
	if cfg.Selector.WindowSeconds != 0 {
		windowSeconds = float64(cfg.Selector.WindowSeconds)
	}
	overlap := float64(cfg.Clip.TargetSecondsMax)
	if overlap <= 0 || overlap >= windowSeconds {
		return nil, fmt.Errorf("invalid maximum clip duration for selector windows")
	}
	// Bind a checkpoint to the exact transcript, prompt, provider, and window settings.
	fingerprintInput, err := json.Marshal(struct {
		Transcript Transcript
		Prompt     string
		BaseURL    string
		Model      string
	}{t, SelectorPrompt(cfg, ep.SourceID, ep, t), cfg.Selector.BaseURL, cfg.Selector.ChatModel})
	if err != nil {
		return nil, err
	}
	if cfg.Selector.ReasoningEffort != "" || cfg.Selector.MaxTokens != 0 {
		fingerprintInput = append(fingerprintInput, []byte(fmt.Sprintf("\nreasoning=%s;max_tokens=%d", cfg.Selector.ReasoningEffort, cfg.Selector.MaxTokens))...)
	}
	if cfg.Selector.WindowSeconds != 0 {
		fingerprintInput = append(fingerprintInput, []byte(fmt.Sprintf("\nwindow=%.3f", windowSeconds))...)
	}
	// Rejected proposals were not stored in older checkpoints, so they cannot
	// be re-evaluated after changing the boundary-repair policy.
	fingerprintInput = append(fingerprintInput, []byte("\nboundary-repair=v1")...)
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(fingerprintInput))
	type checkpoint struct {
		Fingerprint string                 `json:"fingerprint"`
		Windows     map[string][]Candidate `json:"windows"`
		Reviews     map[string]bool        `json:"reviews,omitempty"`
	}
	state := checkpoint{Fingerprint: fingerprint, Windows: map[string][]Candidate{}}
	checkpointPath := filepath.Join(cfg.Storage.Transcripts, ep.SourceID+"__"+ep.VideoID+".selection-progress.json")
	if cfg.Storage.Transcripts != "" {
		if raw, err := os.ReadFile(checkpointPath); err == nil {
			var saved checkpoint
			if json.Unmarshal(raw, &saved) == nil && saved.Fingerprint == fingerprint && saved.Windows != nil {
				state = saved
			} else {
				// Preserve incompatible or damaged legacy checkpoints rather than overwriting them.
				checkpointPath = filepath.Join(cfg.Storage.Transcripts, ep.SourceID+"__"+ep.VideoID+".selection-"+fingerprint+".json")
				if alternate, readErr := os.ReadFile(checkpointPath); readErr == nil {
					var compatible checkpoint
					if json.Unmarshal(alternate, &compatible) == nil && compatible.Fingerprint == fingerprint && compatible.Windows != nil {
						state = compatible
					} else {
						return nil, fmt.Errorf("selector checkpoint for current configuration is invalid: %s", checkpointPath)
					}
				}
			}
		}
	}
	if state.Reviews == nil {
		state.Reviews = map[string]bool{}
	}
	reviewKey := func(c Candidate) string {
		data, _ := json.Marshal(struct {
			Version   string
			Candidate Candidate
		}{"story-review-v3-standalone", c})
		return fmt.Sprintf("%x", sha256.Sum256(data))
	}
	saveCheckpoint := func() error {
		if cfg.Storage.Transcripts == "" {
			return nil
		}
		if err := os.MkdirAll(cfg.Storage.Transcripts, 0o755); err != nil {
			return err
		}
		data, err := json.Marshal(state)
		if err != nil {
			return err
		}
		tmp, err := os.CreateTemp(cfg.Storage.Transcripts, ".selection-*")
		if err != nil {
			return err
		}
		name := tmp.Name()
		defer os.Remove(name)
		if _, err = tmp.Write(data); err != nil {
			_ = tmp.Close()
			return err
		}
		if err = tmp.Close(); err != nil {
			return err
		}
		return os.Rename(name, checkpointPath)
	}
	var valid []Candidate
	var approved []Candidate
	reviewedKeys := map[string]bool{}
	reviewFast := func() (bool, error) {
		if cfg.Selector.SelectionMode != "fast" {
			return false, nil
		}
		// Prefer the highest-scoring candidate among overlapping proposals in
		// the windows seen so far; fast mode cannot compare later windows.
		sort.SliceStable(valid, func(i, j int) bool { return valid[i].Score > valid[j].Score })
		for {
			var pending []Candidate
			for _, c := range valid {
				key := reviewKey(c)
				if reviewedKeys[key] {
					continue
				}
				duplicate := false
				for _, other := range append(append([]Candidate(nil), approved...), pending...) {
					intersection := minFloat(c.End, other.End) - maxFloat(c.Start, other.Start)
					if intersection > 0 && intersection >= 0.5*minFloat(c.End-c.Start, other.End-other.Start) {
						duplicate = true
						break
					}
				}
				if duplicate {
					continue
				}
				if passed, ok := state.Reviews[reviewKey(c)]; ok {
					reviewedKeys[key] = true
					if passed {
						approved = append(approved, c)
					}
				} else {
					pending = append(pending, c)
				}
			}
			target := cfg.Selector.TargetCuts
			if target <= 0 {
				target = 1
			}
			if len(approved) >= target {
				return true, nil
			}
			if len(pending) == 0 {
				return false, nil
			}
			finish := StartTiming(ctx, TimingEvent{Stage: "review", SourceID: ep.SourceID, VideoID: ep.VideoID})
			passed, err := reviewStories(ctx, provider, pending, t)
			finish(err)
			if err != nil {
				return false, fmt.Errorf("story review: %w", err)
			}
			for _, c := range pending {
				reviewedKeys[reviewKey(c)] = true
				state.Reviews[reviewKey(c)] = false
			}
			for _, c := range passed {
				state.Reviews[reviewKey(c)] = true
			}
			if err := saveCheckpoint(); err != nil {
				return false, fmt.Errorf("review checkpoint: %w", err)
			}
			approved = append(approved, passed...)
			if len(approved) >= target {
				return true, nil
			}
		}
	}

	type selectionWindow struct {
		start, end float64
		key        string
		part       Transcript
	}
	type selectionResult struct {
		index    int
		accepted []Candidate
		err      error
	}
	var windows []selectionWindow
	for start := 0.0; start < t.DurationS; start += windowSeconds - overlap {
		end := minFloat(start+windowSeconds, t.DurationS)
		part := t
		part.Segments = nil
		for _, segment := range t.Segments {
			if segment.End > start && segment.Start < end {
				part.Segments = append(part.Segments, segment)
			}
		}
		if len(part.Segments) > 0 {
			windows = append(windows, selectionWindow{start: start, end: end, key: fmt.Sprintf("%.3f-%.3f", start, end), part: part})
		}
		if end == t.DurationS {
			break
		}
	}
	windowTotal := len(windows)
	concurrency := cfg.Selector.Concurrency
	if concurrency == 0 {
		concurrency = 1
	}
	if concurrency < 1 || concurrency > 2 {
		return nil, fmt.Errorf("selector concurrency must be 1 or 2")
	}
	completedWindows := 0
	stopped := false
	for index := 0; index < len(windows) && !stopped; {
		// Review cached windows before scheduling more provider work: fast mode
		// may already have enough approved cuts.
		if saved, ok := state.Windows[windows[index].key]; ok {
			w := windows[index]
			ReportProgress(ctx, Progress{Stage: "select", Message: fmt.Sprintf("Analyzing transcript window %d of %d", completedWindows+1, windowTotal), Completed: completedWindows, Total: windowTotal})
			StartTiming(ctx, TimingEvent{Stage: "selection_window", SourceID: ep.SourceID, VideoID: ep.VideoID, Reused: true, WindowStart: &w.start, WindowEnd: &w.end})(nil)
			for _, c := range saved {
				if c.Score >= cfg.Selector.MinScore && validCandidate(c, t, cfg, w.start, w.end) {
					valid = append(valid, c)
				}
			}
			completedWindows++
			stop, err := reviewFast()
			if err != nil {
				return nil, err
			}
			stopped = stop
			index++
			continue
		}
		batchEnd := index
		for batchEnd < len(windows) && batchEnd-index < concurrency {
			if _, cached := state.Windows[windows[batchEnd].key]; cached {
				break
			}
			batchEnd++
		}
		ReportProgress(ctx, Progress{Stage: "select", Message: fmt.Sprintf("Analyzing transcript window %d of %d", completedWindows+1, windowTotal), Completed: completedWindows, Total: windowTotal})
		batchCtx, cancelBatch := context.WithCancel(ctx)
		results := make(chan selectionResult, batchEnd-index)
		for j := index; j < batchEnd; j++ {
			go func(i int, w selectionWindow) {
				log.Printf("selector: processing %.0f–%.0f seconds of %.0f", w.start, w.end, t.DurationS)
				user := SelectorPrompt(cfg, ep.SourceID, ep, w.part) + fmt.Sprintf("\nSelecione apenas entre %.1f e %.1f segundos do episódio. Use os timestamps originais, sem reiniciar em zero.\n", w.start, w.end)
				finish := StartTiming(batchCtx, TimingEvent{Stage: "selection_window", SourceID: ep.SourceID, VideoID: ep.VideoID, WindowStart: &w.start, WindowEnd: &w.end})
				raw, err := provider.Complete(batchCtx, system, user)
				finish(err)
				if err != nil {
					results <- selectionResult{index: i, err: err}
					return
				}
				var candidates []Candidate
				if cfg.Selector.PromptFormat == "compact" {
					candidates, err = ParseCompactCandidates(cfg, ep.SourceID, ep.VideoID, raw, w.part)
				} else {
					candidates, err = ParseCandidates(cfg, ep.SourceID, ep.VideoID, raw)
				}
				if err != nil {
					results <- selectionResult{index: i, err: err}
					return
				}
				var accepted []Candidate
				rejections := map[string]int{}
				for _, c := range candidates {
					c = alignWordBoundaries(c, t.Words)
					if !validCandidate(c, t, cfg, w.start, w.end) {
						original := c
						var repaired bool
						c, repaired = repairCandidateBoundaries(c, t, cfg, w.start, w.end)
						if !repaired {
							for _, reason := range candidateFailureReasons(original, t, cfg, w.start, w.end) {
								rejections[reason]++
							}
							continue
						}
						log.Printf("selector: repaired candidate boundaries using literal transcript evidence")
					}
					accepted = append(accepted, c)
				}
				log.Printf("selector: window %.0f–%.0f returned %d candidates, %d passed story and timing checks", w.start, w.end, len(candidates), len(accepted))
				if len(rejections) > 0 {
					log.Printf("selector: rejected candidate check counts (may overlap): %v", rejections)
				}
				results <- selectionResult{index: i, accepted: accepted}
			}(j, windows[j])
		}
		batch := make(map[int]selectionResult, batchEnd-index)
		var firstErr error
		for j := index; j < batchEnd; j++ {
			result := <-results
			batch[result.index] = result
			if result.err != nil && firstErr == nil {
				firstErr = fmt.Errorf("selector window %.0f–%.0f: %w", windows[result.index].start, windows[result.index].end, result.err)
				cancelBatch()
			}
		}
		cancelBatch()
		// A successful speculative window is durable even if its neighbor
		// failed, or if an earlier window satisfies fast mode.
		for j := index; j < batchEnd; j++ {
			result := batch[j]
			if result.err != nil {
				continue
			}
			state.Windows[windows[j].key] = result.accepted
			if err := saveCheckpoint(); err != nil {
				return nil, fmt.Errorf("selector checkpoint: %w", err)
			}
		}
		if firstErr != nil {
			return nil, firstErr
		}
		for index < batchEnd {
			w := windows[index]
			for _, c := range state.Windows[w.key] {
				if c.Score >= cfg.Selector.MinScore {
					valid = append(valid, c)
				}
			}
			completedWindows++
			stop, err := reviewFast()
			if err != nil {
				return nil, err
			}
			index++
			if stop {
				stopped = true
				break
			}
		}
	}
	if cfg.Selector.SelectionMode == "fast" {
		sort.SliceStable(approved, func(i, j int) bool { return approved[i].Score > approved[j].Score })
		target := cfg.Selector.TargetCuts
		if target <= 0 {
			target = 1
		}
		if len(approved) > target {
			approved = approved[:target]
		}
		return approved, nil
	}
	sort.SliceStable(valid, func(i, j int) bool { return valid[i].Score > valid[j].Score })
	result := make([]Candidate, 0, len(valid))
	for _, c := range valid {
		duplicate := false
		for _, prior := range result {
			intersection := minFloat(c.End, prior.End) - maxFloat(c.Start, prior.Start)
			if intersection > 0 && intersection >= 0.5*minFloat(c.End-c.Start, prior.End-prior.Start) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			result = append(result, c)
		}
	}
	if len(result) == 0 {
		return result, nil
	}
	ReportProgress(ctx, Progress{Stage: "select", Message: fmt.Sprintf("Reviewing %d candidate cuts", len(result)), Completed: windowTotal, Total: windowTotal})
	var pending, reviewed []Candidate
	for _, c := range result {
		if passed, ok := state.Reviews[reviewKey(c)]; ok {
			if passed {
				reviewed = append(reviewed, c)
			}
		} else {
			pending = append(pending, c)
		}
	}
	if len(pending) > 0 {
		finishReview := StartTiming(ctx, TimingEvent{Stage: "review", SourceID: ep.SourceID, VideoID: ep.VideoID})
		var passed []Candidate
		var err error
		for start := 0; start < len(pending); start += 10 {
			end := start + 10
			if end > len(pending) {
				end = len(pending)
			}
			var batch []Candidate
			batch, err = reviewStories(ctx, provider, pending[start:end], t)
			if err != nil {
				break
			}
			passed = append(passed, batch...)
		}
		finishReview(err)
		if err != nil {
			return nil, fmt.Errorf("story review: %w", err)
		}
		for _, c := range pending {
			state.Reviews[reviewKey(c)] = false
		}
		for _, c := range passed {
			state.Reviews[reviewKey(c)] = true
		}
		if err := saveCheckpoint(); err != nil {
			return nil, fmt.Errorf("review checkpoint: %w", err)
		}
		reviewed = reviewed[:0]
		for _, c := range result {
			if state.Reviews[reviewKey(c)] {
				reviewed = append(reviewed, c)
			}
		}
	}
	return reviewed, nil
}

func validCandidate(c Candidate, t Transcript, cfg Config, start, end float64) bool {
	return len(candidateFailureReasons(c, t, cfg, start, end)) == 0
}

// Reasons are fixed labels; transcript and model text must not be logged.
func candidateFailureReasons(c Candidate, t Transcript, cfg Config, start, end float64) []string {
	var reasons []string
	if !(c.Start >= start && c.End <= end && c.End > c.Start && c.End <= t.DurationS) {
		reasons = append(reasons, "bounds")
	}
	if !(c.End-c.Start >= float64(cfg.Clip.TargetSecondsMin) && c.End-c.Start <= float64(cfg.Clip.TargetSecondsMax)) {
		reasons = append(reasons, "duration")
	}
	if !(c.Score >= 0 && c.Score <= 1 && strings.TrimSpace(c.Title) != "" && strings.TrimSpace(c.ViralityReason) != "" && len(c.AttentionSignals) > 0) {
		reasons = append(reasons, "metadata")
	}
	if !hasHookEvidence(c, t.Words) {
		reasons = append(reasons, "hook")
	}
	if !hasStoryEvidence(c, t.Words) {
		reasons = append(reasons, "story")
	}
	return reasons
}

// repairCandidateBoundaries uses only unique literal matches close to the
// proposed boundaries, then applies the complete original validation again.
func repairCandidateBoundaries(c Candidate, t Transcript, cfg Config, windowStart, windowEnd float64) (Candidate, bool) {
	const maxShift = 8.0
	find := func(quote string, reference float64, useStart bool) (float64, bool) {
		phrase := normalizedWords(quote)
		if len(phrase) < 2 || len(phrase) > 60 {
			return 0, false
		}
		var tokens []string
		var starts, ends []float64
		for _, w := range t.Words {
			for _, token := range normalizedWords(w.Word) {
				tokens = append(tokens, token)
				starts = append(starts, w.Start)
				ends = append(ends, w.End)
			}
		}
		var found float64
		matches := 0
		for i := 0; i+len(phrase) <= len(tokens); i++ {
			equal := true
			for j := range phrase {
				if tokens[i+j] != phrase[j] {
					equal = false
					break
				}
			}
			if !equal {
				continue
			}
			v := starts[i]
			if !useStart {
				v = ends[i+len(phrase)-1]
			}
			if math.Abs(v-reference) <= maxShift {
				found = v
				matches++
			}
		}
		return found, matches == 1
	}
	if !hasHookEvidence(c, t.Words) {
		start, ok := find(c.HookQuote, c.Start, true)
		if !ok {
			return c, false
		}
		c.Start = start
	}
	if !hasStoryEvidence(c, t.Words) {
		end, ok := find(c.Story.ConclusionQuote, c.End, false)
		if !ok {
			return c, false
		}
		c.End = end
	}
	return c, validCandidate(c, t, cfg, windowStart, windowEnd)
}

// reviewStories asks a separate editorial pass to check the actual cut and
// surrounding context; a positive review is required for every candidate.
func reviewStories(ctx context.Context, provider ChatProvider, candidates []Candidate, t Transcript) ([]Candidate, error) {
	var prompt strings.Builder
	prompt.WriteString("Review from the perspective of a new viewer who has not seen the full source. Reject a clip if its spoken content does not establish the necessary subject and setup, or if an important pronoun, person, event, or claim has an unclear referent. The title, description and outside context cannot supply missing context for the clip itself.\n")
	prompt.WriteString("Avalie cada corte de forma rigorosa. Aceite apenas se o próprio corte tiver contexto, desenvolvimento e conclusão clara, E se a fala literal indicada como gancho realmente abrir o corte nos primeiros 3 segundos e tiver potencial concreto de capturar atenção sem distorcer o sentido. Considere curiosidade, surpresa/revelação, opinião forte, conflito, reação emocional, frase memorável, payoff ou valor prático. Rejeite frases cortadas, respostas/desfechos fora do corte, ganchos genéricos ou enganosos e trechos que precisem do contexto anterior. Não deduza conclusão de texto fora do corte. O CONTEXTO é apenas para identificar cortes no meio do raciocínio. Retorne somente JSON {\"reviews\":[{\"index\":0,\"complete\":true,\"hook_strong\":true,\"reason\":\"...\"}]} com um item por índice.\n")
	for i, c := range candidates {
		fmt.Fprintf(&prompt, "\nCORTE %d (%.2f–%.2f):\n", i, c.Start, c.End)
		fmt.Fprintf(&prompt, "GANCHO PROPOSTO: %q\nSINAIS: %s\nMOTIVO DE RETENÇÃO: %s\n", c.HookQuote, strings.Join(c.AttentionSignals, ", "), c.ViralityReason)
		var clip, before, after strings.Builder
		for _, w := range t.Words {
			switch {
			case w.Start >= c.Start && w.End <= c.End:
				fmt.Fprintf(&clip, "%s ", w.Word)
			case w.End <= c.Start && w.End >= c.Start-15:
				fmt.Fprintf(&before, "%s ", w.Word)
			case w.Start >= c.End && w.Start <= c.End+15:
				fmt.Fprintf(&after, "%s ", w.Word)
			}
		}
		fmt.Fprintf(&prompt, "ANTES: %s\nCORTE: %s\nDEPOIS: %s\n", before.String(), clip.String(), after.String())
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		user := prompt.String()
		if attempt > 0 {
			user += fmt.Sprintf("\nA resposta anterior foi inválida. Retorne exatamente %d avaliações, uma para cada índice, sem omissões ou duplicatas.\n", len(candidates))
		}
		raw, err := provider.Complete(ctx, "Você revisa cortes para garantir histórias completas e fiéis ao áudio.", user)
		if err != nil {
			return nil, err
		}
		result, err := parseStoryReviews(raw, candidates)
		if err == nil {
			log.Printf("selector: story review accepted %d of %d candidates", len(result), len(candidates))
			return result, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("story review invalid after two attempts: %w", lastErr)
}

func parseStoryReviews(raw string, candidates []Candidate) ([]Candidate, error) {
	var response struct {
		Reviews []struct {
			Index      *int   `json:"index"`
			Complete   *bool  `json:"complete"`
			HookStrong *bool  `json:"hook_strong"`
			Reason     string `json:"reason"`
		} `json:"reviews"`
	}
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		return nil, fmt.Errorf("invalid review JSON: %w", err)
	}
	if len(response.Reviews) != len(candidates) {
		return nil, fmt.Errorf("expected %d reviews, got %d", len(candidates), len(response.Reviews))
	}
	approved := make([]bool, len(candidates))
	seen := make([]bool, len(candidates))
	for _, review := range response.Reviews {
		if review.Index == nil || review.Complete == nil || review.HookStrong == nil || *review.Index < 0 || *review.Index >= len(candidates) || seen[*review.Index] {
			return nil, fmt.Errorf("invalid or duplicate review index")
		}
		seen[*review.Index] = true
		approved[*review.Index] = *review.Complete && *review.HookStrong
	}
	var result []Candidate
	for i, candidate := range candidates {
		if approved[i] {
			result = append(result, candidate)
		}
	}
	return result, nil
}

var allowedAttentionSignals = map[string]bool{
	"curiosity": true, "surprise": true, "strong_opinion": true, "conflict": true,
	"emotional_reaction": true, "revelation": true, "quotable_line": true,
	"story_payoff": true, "practical_value": true,
}

func normalizeAttentionSignals(signals []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, 3)
	for _, signal := range signals {
		signal = strings.ToLower(strings.TrimSpace(signal))
		if allowedAttentionSignals[signal] && !seen[signal] && len(out) < 3 {
			seen[signal] = true
			out = append(out, signal)
		}
	}
	return out
}

func hasHookEvidence(c Candidate, words []Word) bool {
	phrase := normalizedWords(c.HookQuote)
	if len(phrase) < 2 || len(phrase) > 60 {
		return false
	}
	deadline := minFloat(c.End, c.Start+3)
	var tokens []string
	var starts, ends []float64
	for _, word := range words {
		if word.Start < c.Start || word.End > c.End {
			continue
		}
		for _, token := range normalizedWords(word.Word) {
			tokens = append(tokens, token)
			starts = append(starts, word.Start)
			ends = append(ends, word.End)
		}
	}
	for i := 0; i+len(phrase) <= len(tokens); i++ {
		matches := true
		for j := range phrase {
			if tokens[i+j] != phrase[j] {
				matches = false
				break
			}
		}
		if matches && starts[i] >= c.Start && starts[i] < deadline && ends[i+len(phrase)-1] <= c.End {
			return true
		}
	}
	return false
}

// alignWordBoundaries includes whole words when the selector cuts through one.
func alignWordBoundaries(c Candidate, words []Word) Candidate {
	for _, w := range words {
		if w.Start < c.Start && c.Start < w.End {
			c.Start = w.Start
		}
		if w.Start < c.End && c.End < w.End {
			c.End = w.End
		}
	}
	return c
}

func normalizedWords(text string) []string {
	return strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return unicode.ToLower(r)
		}
		return ' '
	}, text))
}

// hasStoryEvidence verifies literal, ordered quotations in the clip, including
// a conclusion near its end. It cannot independently prove narrative quality.
func hasStoryEvidence(c Candidate, words []Word) bool {
	var tokens []string
	var starts, ends []float64
	var lastWord Word
	for _, w := range words {
		if w.Start < c.Start || w.End > c.End {
			continue
		}
		if strings.TrimSpace(w.Word) != "" {
			lastWord = w
		}
		for _, token := range normalizedWords(w.Word) {
			tokens = append(tokens, token)
			starts = append(starts, w.Start)
			ends = append(ends, w.End)
		}
	}
	if strings.TrimSpace(lastWord.Word) == "" || c.End-lastWord.End > 3 {
		return false
	}
	quotes := []string{c.Story.SetupQuote, c.Story.DevelopmentQuote, c.Story.ConclusionQuote}
	next := 0
	for i, quote := range quotes {
		phrase := normalizedWords(quote)
		if len(phrase) < 2 || len(phrase) > 60 {
			return false
		}
		found := -1
		for j := next; j+len(phrase) <= len(tokens); j++ {
			matches := true
			for k := range phrase {
				if tokens[j+k] != phrase[k] {
					matches = false
					break
				}
			}
			if matches {
				found = j
				break
			}
		}
		if found < 0 {
			return false
		}
		last := found + len(phrase) - 1
		if i == 0 && starts[found]-c.Start > 15 {
			return false
		}
		if i == 2 && (c.End-ends[last] > 3 || lastWord.End-ends[last] > 1.5) {
			return false
		}
		next = last + 1
	}
	return true
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
