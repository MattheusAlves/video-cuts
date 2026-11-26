package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Word struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

type Segment struct {
	ID    int     `json:"id"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

type Transcript struct {
	VideoID   string    `json:"video_id"`
	Language  string    `json:"language"`
	DurationS float64   `json:"duration"`
	Text      string    `json:"text"`
	Segments  []Segment `json:"segments"`
	Words     []Word    `json:"words,omitempty"`
}

// whisperJSON matches whisper.cpp's full JSON output. Some builds omit token
// timestamps; in that case segments remain usable but Words is left empty.
type whisperJSON struct {
	Transcription []struct {
		Timestamps struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"timestamps"`
		Offsets struct {
			From int64 `json:"from"`
			To   int64 `json:"to"`
		} `json:"offsets"`
		Text   string `json:"text"`
		Tokens []struct {
			Text       string `json:"text"`
			Timestamps struct {
				From string `json:"from"`
				To   string `json:"to"`
			} `json:"timestamps"`
			Offsets struct {
				From int64 `json:"from"`
				To   int64 `json:"to"`
			} `json:"offsets"`
		} `json:"tokens"`
	} `json:"transcription"`
}

func Transcribe(ctx context.Context, cfg Config, ep Episode, _ string) (Transcript, error) {
	if cfg.Transcription.Provider != "local" && cfg.Transcription.Provider != "whisper.cpp" {
		return Transcript{}, fmt.Errorf("unsupported transcription provider %q (want local)", cfg.Transcription.Provider)
	}
	if ep.AudioPath == "" {
		return Transcript{}, fmt.Errorf("episode %s has no audio path", ep.VideoID)
	}
	if cfg.Transcription.WhisperModel == "" {
		return Transcript{}, fmt.Errorf("transcription.whisper_model is required")
	}
	bin := cfg.Transcription.WhisperBin
	if bin == "" {
		bin = "whisper-cli"
	}
	if _, err := os.Stat(cfg.Transcription.WhisperModel); err != nil {
		return Transcript{}, fmt.Errorf("whisper model: %w", err)
	}
	if _, err := os.Stat(ep.AudioPath); err != nil {
		return Transcript{}, fmt.Errorf("audio: %w", err)
	}

	tmp, err := os.MkdirTemp("", "yt-short-whisper-*")
	if err != nil {
		return Transcript{}, err
	}
	defer os.RemoveAll(tmp)
	wav := filepath.Join(tmp, "audio.wav")
	convert := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error", "-y", "-i", ep.AudioPath, "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", wav)
	finishConvert := StartTiming(ctx, TimingEvent{Stage: "audio_conversion", SourceID: ep.SourceID, VideoID: ep.VideoID})
	output, err := convert.CombinedOutput()
	finishConvert(err)
	if err != nil {
		return Transcript{}, fmt.Errorf("convert audio: %w: %s", err, strings.TrimSpace(string(output)))
	}
	outBase := filepath.Join(tmp, "transcript")
	args := []string{"-m", cfg.Transcription.WhisperModel, "-f", wav, "-ojf", "-of", outBase, "-np", "-pp"}
	if cfg.Transcription.Language != "" {
		args = append(args, "-l", cfg.Transcription.Language)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	finishInference := StartTiming(ctx, TimingEvent{Stage: "transcription", SourceID: ep.SourceID, VideoID: ep.VideoID})
	err = cmd.Run()
	finishInference(err)
	if err != nil {
		return Transcript{}, fmt.Errorf("whisper.cpp: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	raw, err := os.ReadFile(outBase + ".json")
	if err != nil {
		return Transcript{}, fmt.Errorf("read whisper JSON: %w (output: %s)", err, strings.TrimSpace(stderr.String()))
	}
	var decoded whisperJSON
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Transcript{}, fmt.Errorf("parse whisper JSON: %w", err)
	}
	out := Transcript{VideoID: ep.VideoID, Language: cfg.Transcription.Language}
	for i, s := range decoded.Transcription {
		start, end := whisperTime(s.Timestamps.From, s.Offsets.From), whisperTime(s.Timestamps.To, s.Offsets.To)
		if end > out.DurationS {
			out.DurationS = end
		}
		text := strings.TrimSpace(s.Text)
		out.Segments = append(out.Segments, Segment{ID: i, Start: start, End: end, Text: text})
		if out.Text != "" {
			out.Text += " "
		}
		out.Text += text
		for _, token := range s.Tokens {
			ws := whisperTime(token.Timestamps.From, token.Offsets.From)
			we := whisperTime(token.Timestamps.To, token.Offsets.To)
			word := strings.TrimSpace(token.Text)
			if word == "" || strings.HasPrefix(word, "[_") || we < ws || we <= 0 {
				continue
			}
			if len(out.Words) > 0 && !strings.HasPrefix(token.Text, " ") {
				last := &out.Words[len(out.Words)-1]
				last.Word += word
				if we > last.End {
					last.End = we
				}
			} else {
				out.Words = append(out.Words, Word{Word: word, Start: ws, End: we})
			}
		}
	}
	if len(out.Segments) == 0 || len(out.Words) == 0 || strings.TrimSpace(out.Text) == "" || out.DurationS <= 0 {
		return Transcript{}, fmt.Errorf("whisper returned incomplete transcript (text=%t segments=%d words=%d duration=%.3f)", strings.TrimSpace(out.Text) != "", len(out.Segments), len(out.Words), out.DurationS)
	}
	if out.Language == "" {
		out.Language = "pt"
	}
	return out, nil
}

func whisperTime(ts string, offset int64) float64 {
	return float64(offset) / 1000
}

func SaveTranscript(cfg Config, sourceID string, t Transcript) (string, error) {
	if err := os.MkdirAll(cfg.Storage.Transcripts, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(cfg.Storage.Transcripts, sourceID+"__"+t.VideoID+".json")
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return "", err
	}
	return path, nil
}
