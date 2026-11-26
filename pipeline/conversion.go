package pipeline

import (
	"context"
	"fmt"
	"os"
	"sort"
)

// SelectConversion chooses a new, validated range anchored to an existing cut.
func SelectConversion(ctx context.Context, cfg Config, ep Episode, tr Transcript, original Candidate, expand bool) (Candidate, error) {
	if cfg.Selector.Provider != "openai_compat" {
		return Candidate{}, fmt.Errorf("unknown selector provider %q", cfg.Selector.Provider)
	}
	provider := &OpenAICompatChat{APIKey: os.Getenv(cfg.Selector.APIKeyEnv), Model: cfg.Selector.ChatModel, BaseURL: cfg.Selector.BaseURL, ReasoningEffort: cfg.Selector.ReasoningEffort, MaxTokens: cfg.Selector.MaxTokens}
	return selectConversion(ctx, cfg, ep, tr, original, expand, provider)
}

func selectConversion(ctx context.Context, cfg Config, ep Episode, tr Transcript, original Candidate, expand bool, provider ChatProvider) (Candidate, error) {
	if original.Start < 0 || original.End <= original.Start || original.End > tr.DurationS {
		return Candidate{}, fmt.Errorf("original cut has invalid timestamps")
	}
	start, end := original.Start, original.End
	if expand {
		start = maxFloat(0, original.Start-float64(cfg.Clip.TargetSecondsMax))
		end = minFloat(tr.DurationS, original.End+float64(cfg.Clip.TargetSecondsMax))
	}
	part := tr
	part.Segments = nil
	for _, s := range tr.Segments {
		if s.End > start && s.Start < end {
			part.Segments = append(part.Segments, s)
		}
	}
	if len(part.Segments) == 0 {
		return Candidate{}, fmt.Errorf("no transcript in the conversion range")
	}
	direction := "a strictly shorter range entirely inside the original range"
	if expand {
		direction = "a strictly longer range containing the entire original range"
	}
	prompt := SelectorPrompt(cfg, ep.SourceID, ep, part) + fmt.Sprintf("\nConvert the existing cut at %.3f–%.3f seconds into %s. Select %s. Use the original timestamps. The result must be a self-contained, coherent story. Only use transcript material between %.3f and %.3f seconds. If no suitable result exists, return an empty clips array.\n", original.Start, original.End, map[bool]string{true: "a regular video", false: "a Short"}[expand], direction, start, end)
	finish := StartTiming(ctx, TimingEvent{Stage: "selection_window", SourceID: ep.SourceID, VideoID: ep.VideoID})
	raw, err := provider.Complete(ctx, "You edit self-contained YouTube videos faithfully from a transcript.", prompt)
	finish(err)
	if err != nil {
		return Candidate{}, fmt.Errorf("conversion selection: %w", err)
	}
	var candidates []Candidate
	if cfg.Selector.PromptFormat == "compact" {
		candidates, err = ParseCompactCandidates(cfg, ep.SourceID, ep.VideoID, raw, part)
	} else {
		candidates, err = ParseCandidates(cfg, ep.SourceID, ep.VideoID, raw)
	}
	if err != nil {
		return Candidate{}, err
	}
	var valid []Candidate
	for _, c := range candidates {
		c = alignWordBoundaries(c, tr.Words)
		if !validCandidate(c, tr, cfg, start, end) {
			var ok bool
			c, ok = repairCandidateBoundaries(c, tr, cfg, start, end)
			if !ok {
				continue
			}
		}
		if c.Score < cfg.Selector.MinScore {
			continue
		}
		if expand {
			if c.Start > original.Start || c.End < original.End || c.End-c.Start <= original.End-original.Start {
				continue
			}
		} else {
			if c.Start < original.Start || c.End > original.End || c.End-c.Start >= original.End-original.Start {
				continue
			}
		}
		valid = append(valid, c)
	}
	if len(valid) == 0 {
		return Candidate{}, fmt.Errorf("no suitable conversion met the requested duration, score and story requirements")
	}
	sort.SliceStable(valid, func(i, j int) bool { return valid[i].Score > valid[j].Score })
	finish = StartTiming(ctx, TimingEvent{Stage: "review", SourceID: ep.SourceID, VideoID: ep.VideoID})
	reviewed, err := reviewStories(ctx, provider, valid, tr)
	finish(err)
	if err != nil {
		return Candidate{}, fmt.Errorf("conversion review: %w", err)
	}
	if len(reviewed) == 0 {
		return Candidate{}, fmt.Errorf("no conversion passed editorial review")
	}
	return reviewed[0], nil
}
