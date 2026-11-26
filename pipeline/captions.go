package pipeline

import (
	"fmt"
	"math"
	"strings"
)

// BuildASS renders an Advanced SubStation Alpha subtitle file covering [start, end).
// The ASS timestamps are rebased so that 0:00:00.00 = start.
// Style: bold DejaVu Sans, 72pt, primary white, active word yellow (word-by-word \k effect).
func BuildASS(cfg Config, t Transcript, start, end float64) (string, error) {
	return buildASS(cfg, t, start, end, false)
}

// BuildASSWithHook gives the opening hook captions a larger, heavier treatment.
func BuildASSWithHook(cfg Config, t Transcript, start, end float64) (string, error) {
	return buildASS(cfg, t, start, end, true)
}

func buildASS(cfg Config, t Transcript, start, end float64, emphasizeHook bool) (string, error) {
	if cfg.Clip.Captions.Style != "" && cfg.Clip.Captions.Style != "word_highlight" && cfg.Clip.Captions.Style != "plain" {
		return "", fmt.Errorf("unknown caption style %q", cfg.Clip.Captions.Style)
	}
	width, height, err := parseResolution(cfg.Clip.Resolution)
	if err != nil {
		return "", err
	}
	if len(t.Words) == 0 {
		return "", fmt.Errorf("transcript %s has no word-level data", t.VideoID)
	}
	// Filter words to the window
	var ws []Word
	for _, w := range t.Words {
		if w.End <= start {
			continue
		}
		if w.Start >= end {
			continue
		}
		// Clamp words that cross the window boundary
		ws = append(ws, Word{
			Word:  w.Word,
			Start: maxF(w.Start, start),
			End:   minF(w.End, end),
		})
	}
	if len(ws) == 0 {
		return "", fmt.Errorf("no words within window %.1f-%.1f", start, end)
	}

	header := `[Script Info]
ScriptType: v4.00+
PlayResX: %d
PlayResY: %d
ScaledBorderAndShadow: yes

[V4+ Styles]
;            Name             Font        Sz   1c        2c        3c  4c  B I U S   OL SH ALI M V T Encoding
Format: Name,Fontname,Fontsize,PrimaryColour,SecondaryColour,OutlineColour,BackColour,Bold,Italic,Underline,StrikeOut,ScaleX,ScaleY,Spacing,Angle,BorderStyle,Outline,Shadow,Alignment,MarginL,MarginR,MarginV,Encoding
Style: Cap,%s,72,&H0000FFFF,&H00FFFFFF,&H00000000,&H80000000,-1,0,0,0,100,100,0,0,1,4,2,2,40,40,200,1
Style: HookCap,%s,88,&H0000FFFF,&H00FFFFFF,&H00000000,&H80000000,-1,0,0,0,100,100,0,0,1,6,2,2,32,32,200,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
`
	if cfg.Clip.Captions.Style == "plain" {
		header = strings.ReplaceAll(header, "&H0000FFFF", "&H00FFFFFF")
	}
	header = fmt.Sprintf(header, width, height, cfg.Clip.Captions.Font, cfg.Clip.Captions.Font)

	var b strings.Builder
	b.WriteString(header)

	// Group words into ~3-word chunks so each caption is short and punchy.
	const perChunk = 3
	for i := 0; i < len(ws); i += perChunk {
		j := i + perChunk
		if j > len(ws) {
			j = len(ws)
		}
		chunk := ws[i:j]
		startAt := chunk[0].Start - start
		endAt := chunk[len(chunk)-1].End - start

		// Build text with karaoke highlighting of each word at its moment.
		var text strings.Builder
		for k, w := range chunk {
			// Keep the karaoke cursor aligned with the next word's start.
			endTime := w.End
			if k+1 < len(chunk) && chunk[k+1].Start > endTime {
				endTime = chunk[k+1].Start
			}
			durCS := int((endTime - w.Start) * 100)
			if durCS <= 0 {
				durCS = 1
			}
			word := strings.TrimSpace(w.Word)
			if k > 0 {
				text.WriteString(" ")
			}
			if cfg.Clip.Captions.Style == "plain" {
				text.WriteString(escapeASSText(word))
			} else {
				fmt.Fprintf(&text, "{\\kf%d}%s", durCS, escapeASSText(word))
			}
		}

		style := "Cap"
		if emphasizeHook && startAt < 3 {
			style = "HookCap"
		}
		b.WriteString(fmt.Sprintf("Dialogue: 0,%s,%s,%s,,0,0,0,,%s\n",
			formatASSTime(startAt), formatASSTime(endAt), style, text.String()))
	}
	return b.String(), nil
}

// escapeASSText neutralises braces and backslashes in captions.
func escapeASSText(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `{`, `\{`)
	s = strings.ReplaceAll(s, `}`, `\}`)
	// Drop soft newlines inside a token
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// formatASSTime renders h:mm:ss.cc for ASS.
func formatASSTime(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	centiseconds := int64(math.Round(sec * 100))
	h := centiseconds / 360000
	m := (centiseconds % 360000) / 6000
	s := (centiseconds % 6000) / 100
	cs := centiseconds % 100
	return fmt.Sprintf("%d:%02d:%02d.%02d", h, m, s, cs)
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
