package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
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
	fmt.Fprintf(&b, "Você é um editor de cortes virais para um canal de política brasileira (%s).\n", cfg.Channel.Name)
	if cfg.Clip.LongForm {
		fmt.Fprintf(&b, "Produza um vídeo longo para o YouTube, não um Short. A saída é horizontal e deve funcionar como um vídeo autônomo.\n")
	} else {
		fmt.Fprintf(&b, "O canal é NEUTRO, sem alinhamento partidário. Títulos curtos e provocativos em português (max 90 chars).\n")
	}
	fmt.Fprintf(&b, "O canal é NEUTRO, sem alinhamento partidário.\n\n")
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
	fmt.Fprintf(&b, "   - hook_quote (citação literal curta do gancho, que ocorre nos primeiros 3 segundos)\n")
	fmt.Fprintf(&b, "   - attention_signals (1-3 itens entre curiosity, surprise, strong_opinion, conflict, emotional_reaction, revelation, quotable_line, story_payoff, practical_value)\n")
	fmt.Fprintf(&b, "   - virality_reason (PT-BR, uma frase específica explicando por que pode parar a rolagem/manter atenção; não prometa viralização)\n")
	fmt.Fprintf(&b, "   - title (PT-BR <= 90 chars, com 1 emoji no máximo)\n")
	fmt.Fprintf(&b, "   - description (PT-BR 1-2 frases)\n")
	fmt.Fprintf(&b, "   - hashtags (3-5 strings como #politica #brasil)\n")
	fmt.Fprintf(&b, "   - rationale (PT-BR, 1 frase curta resumindo o motivo editorial)\n\n")
	fmt.Fprintf(&b, "   - story: objeto com setup_quote, development_quote e conclusion_quote. Cada um deve ser uma citação literal curta e distinta, na ordem em que é falada dentro do corte. A primeira deve aparecer nos primeiros 15 segundos; a conclusão deve conter as últimas palavras do corte, sem começar outra frase que não termine.\n\n")
	fmt.Fprintf(&b, "TRANSCRIÇÃO COM TIMESTAMPS (formato: [start-end] texto):\n")
	// Combine ~10s chunks of segments so the LLM sees timing clearly.
	for _, s := range t.Segments {
		fmt.Fprintf(&b, "[%.1f-%.1f] %s\n", s.Start, s.End, strings.TrimSpace(s.Text))
	}
	fmt.Fprintf(&b, "\nDevolva APENAS um JSON com a chave \"clips\": [ {start, end, score, hook_quote, attention_signals, virality_reason, title, description, hashtags, rationale, story: {setup_quote, development_quote, conclusion_quote}}, ... ]\n")
	fmt.Fprintf(&b, "Não inclua markdown. Não inclua comentários fora do JSON.\n")
	return b.String()
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
	APIKey    string
	Model     string
	BaseURL   string
	SessionID string
	Client    *http.Client
}

func (g *OpenAICompatChat) Complete(ctx context.Context, system, user string) (string, error) {
	if g.APIKey == "" {
		return "", fmt.Errorf("selector API key is missing")
	}
	if g.Client == nil {
		g.Client = &http.Client{Timeout: 10 * time.Minute}
	}
	body, _ := json.Marshal(map[string]any{
		"model": g.Model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"temperature":     0.4,
		"response_format": map[string]string{"type": "json_object"},
	})
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
	if g.SessionID != "" {
		req.Header.Set("x-opencode-session", g.SessionID)
	}
	resp, err := g.Client.Do(req)
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
		Choices []struct {
			Message struct {
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
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("no choices")
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
	}

	system := "Você é um assistente que seleciona cortes para o YouTube Shorts."
	windowSeconds := 180.0
	if cfg.Clip.LongForm {
		// A window is 1.5× the maximum cut, with half a maximum of overlap.
		// That guarantees a candidate up to the configured maximum fits inside
		// at least one window while keeping calls far apart.
		windowSeconds = float64(cfg.Clip.TargetSecondsMax) * 1.5
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
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(fingerprintInput))
	type checkpoint struct {
		Fingerprint string                 `json:"fingerprint"`
		Windows     map[string][]Candidate `json:"windows"`
	}
	state := checkpoint{Fingerprint: fingerprint, Windows: map[string][]Candidate{}}
	checkpointPath := filepath.Join(cfg.Storage.Transcripts, ep.SourceID+"__"+ep.VideoID+".selection-progress.json")
	if cfg.Storage.Transcripts != "" {
		if raw, err := os.ReadFile(checkpointPath); err == nil {
			var saved checkpoint
			if json.Unmarshal(raw, &saved) == nil && saved.Fingerprint == fingerprint && saved.Windows != nil {
				state = saved
			}
		}
	}
	var valid []Candidate
	windowTotal := 0
	for start := 0.0; start < t.DurationS; start += windowSeconds - overlap {
		end := minFloat(start+windowSeconds, t.DurationS)
		for _, segment := range t.Segments {
			if segment.End > start && segment.Start < end {
				windowTotal++
				break
			}
		}
		if end == t.DurationS {
			break
		}
	}
	completedWindows := 0
	for start := 0.0; start < t.DurationS; start += windowSeconds - overlap {
		end := start + windowSeconds
		if end > t.DurationS {
			end = t.DurationS
		}
		part := t
		part.Segments = nil
		for _, segment := range t.Segments {
			if segment.End > start && segment.Start < end {
				part.Segments = append(part.Segments, segment)
			}
		}
		if len(part.Segments) == 0 {
			continue
		}
		windowKey := fmt.Sprintf("%.3f-%.3f", start, end)
		ReportProgress(ctx, Progress{Stage: "select", Message: fmt.Sprintf("Analyzing transcript window %d of %d", completedWindows+1, windowTotal), Completed: completedWindows, Total: windowTotal})
		if saved, ok := state.Windows[windowKey]; ok {
			for _, c := range saved {
				if validCandidate(c, t, cfg, start, end) {
					valid = append(valid, c)
				}
			}
			completedWindows++
			if end == t.DurationS {
				break
			}
			continue
		}
		log.Printf("selector: processing %.0f–%.0f seconds of %.0f", start, end, t.DurationS)
		user := SelectorPrompt(cfg, ep.SourceID, ep, part) + fmt.Sprintf("\nSelecione apenas entre %.1f e %.1f segundos do episódio. Use os timestamps originais, sem reiniciar em zero.\n", start, end)
		raw, err := provider.Complete(ctx, system, user)
		if err != nil {
			return nil, fmt.Errorf("selector window %.0f–%.0f: %w", start, end, err)
		}
		candidates, err := ParseCandidates(cfg, ep.SourceID, ep.VideoID, raw)
		if err != nil {
			return nil, fmt.Errorf("selector window %.0f–%.0f: %w", start, end, err)
		}
		var accepted []Candidate
		for _, c := range candidates {
			c = alignWordBoundaries(c, t.Words)
			if !validCandidate(c, t, cfg, start, end) {
				continue
			}
			accepted = append(accepted, c)
		}
		log.Printf("selector: window %.0f–%.0f returned %d candidates, %d passed story and timing checks", start, end, len(candidates), len(accepted))
		valid = append(valid, accepted...)
		state.Windows[windowKey] = accepted
		completedWindows++
		if cfg.Storage.Transcripts != "" {
			if err := os.MkdirAll(cfg.Storage.Transcripts, 0o755); err != nil {
				return nil, fmt.Errorf("selector checkpoint: %w", err)
			}
			data, err := json.Marshal(state)
			if err != nil {
				return nil, err
			}
			tmp, err := os.CreateTemp(cfg.Storage.Transcripts, ".selection-*")
			if err != nil {
				return nil, fmt.Errorf("selector checkpoint: %w", err)
			}
			name := tmp.Name()
			if _, err = tmp.Write(data); err == nil {
				err = tmp.Close()
			} else {
				_ = tmp.Close()
			}
			if err == nil {
				err = os.Rename(name, checkpointPath)
			}
			if err != nil {
				_ = os.Remove(name)
				return nil, fmt.Errorf("selector checkpoint: %w", err)
			}
		}
		if end == t.DurationS {
			break
		}
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
		if len(result) >= 10 {
			break
		}
	}
	if len(result) == 0 {
		return result, nil
	}
	ReportProgress(ctx, Progress{Stage: "select", Message: fmt.Sprintf("Reviewing %d candidate cuts", len(result)), Completed: windowTotal, Total: windowTotal})
	reviewed, err := reviewStories(ctx, provider, result, t)
	if err != nil {
		return nil, fmt.Errorf("story review: %w", err)
	}
	if cfg.Selector.CandidatesPerEpisode > 0 && len(reviewed) > cfg.Selector.CandidatesPerEpisode {
		reviewed = reviewed[:cfg.Selector.CandidatesPerEpisode]
	}
	return reviewed, nil
}

func validCandidate(c Candidate, t Transcript, cfg Config, start, end float64) bool {
	return c.Start >= start && c.End <= end && c.End > c.Start && c.End <= t.DurationS &&
		c.End-c.Start >= float64(cfg.Clip.TargetSecondsMin) && c.End-c.Start <= float64(cfg.Clip.TargetSecondsMax) &&
		c.Score >= 0 && c.Score <= 1 && strings.TrimSpace(c.Title) != "" &&
		strings.TrimSpace(c.ViralityReason) != "" && len(c.AttentionSignals) > 0 &&
		hasHookEvidence(c, t.Words) && hasStoryEvidence(c, t.Words)
}

// reviewStories asks a separate editorial pass to check the actual cut and
// surrounding context; a positive review is required for every candidate.
func reviewStories(ctx context.Context, provider ChatProvider, candidates []Candidate, t Transcript) ([]Candidate, error) {
	var prompt strings.Builder
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
	raw, err := provider.Complete(ctx, "Você revisa cortes para garantir histórias completas e fiéis ao áudio.", prompt.String())
	if err != nil {
		return nil, err
	}
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
		if review.Index == nil || review.Complete == nil || *review.Index < 0 || *review.Index >= len(candidates) || seen[*review.Index] {
			return nil, fmt.Errorf("invalid or duplicate review index")
		}
		seen[*review.Index] = true
		hookStrong := review.HookStrong == nil || *review.HookStrong
		approved[*review.Index] = *review.Complete && hookStrong
	}
	var result []Candidate
	for i, candidate := range candidates {
		if approved[i] {
			result = append(result, candidate)
		}
	}
	log.Printf("selector: story review accepted %d of %d candidates", len(result), len(candidates))
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
	if len(phrase) < 2 || len(phrase) > 15 {
		return false
	}
	deadline := minFloat(c.End, c.Start+3)
	var tokens []string
	var starts, ends []float64
	for _, word := range words {
		if word.Start < c.Start || word.End > deadline {
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
		if matches && starts[i] >= c.Start && ends[i+len(phrase)-1] <= deadline {
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
		if len(phrase) < 2 || len(phrase) > 15 {
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
