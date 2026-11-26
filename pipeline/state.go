package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Source is one YouTube channel we watch for new episodes.
type Source struct {
	ID         string `yaml:"id"`
	ChannelURL string `yaml:"channel_url"`
	Kind       string `yaml:"kind"`
	Polarity   string `yaml:"polarity"`
}

// Channel describes the clips channel itself.
type Channel struct {
	Name        string `yaml:"name"`
	Positioning string `yaml:"positioning"`
	Language    string `yaml:"language"`
}

// Clip config
type Clip struct {
	TargetSecondsMin   int      `yaml:"target_seconds_min"`
	TargetSecondsMax   int      `yaml:"target_seconds_max"`
	LongFormMinSeconds int      `yaml:"long_form_min_seconds"`
	LongFormMaxSeconds int      `yaml:"long_form_max_seconds"`
	LongForm           bool     `yaml:"-"`
	Resolution         string   `yaml:"resolution"`
	Framing            string   `yaml:"framing"`
	VideoEncoder       string   `yaml:"video_encoder"`
	Captions           Captions `yaml:"captions"`
}
type Captions struct {
	Enabled  bool   `yaml:"enabled"`
	Font     string `yaml:"font"`
	Style    string `yaml:"style"`
	Position string `yaml:"position"`
}

// Transcription
type Transcription struct {
	Provider     string `yaml:"provider"`
	Model        string `yaml:"model"`
	Language     string `yaml:"language"`
	WhisperBin   string `yaml:"whisper_bin"`
	WhisperModel string `yaml:"whisper_model"`
}

// Selector
type Selector struct {
	Provider             string  `yaml:"provider"`
	ChatModel            string  `yaml:"chat_model"`
	BaseURL              string  `yaml:"base_url"`
	APIKeyEnv            string  `yaml:"api_key_env"`
	CandidatesPerEpisode int     `yaml:"candidates_per_episode"`
	MinScore             float32 `yaml:"min_score"`
	SelectionMode        string  `yaml:"-"`
	TargetCuts           int     `yaml:"-"`
	PromptFormat         string  `yaml:"prompt_format"`
	WindowSeconds        int     `yaml:"window_seconds"`
	ReasoningEffort      string  `yaml:"reasoning_effort"`
	MaxTokens            int     `yaml:"max_tokens"`
	Concurrency          int     `yaml:"concurrency"`
}

// Storage
type Storage struct {
	SourcesDir        string `yaml:"sources_dir"`
	Transcripts       string `yaml:"transcripts"`
	ClipsPending      string `yaml:"clips_pending"`
	StateFile         string `yaml:"state_file"`
	KeepSources       bool   `yaml:"keep_sources"`
	MaxEpisodeAgeDays int    `yaml:"max_episode_age_days"`
}

// Schedule
type Schedule struct {
	FetchAt  string `yaml:"fetch_at"`
	Timezone string `yaml:"timezone"`
}

// Config is the YAML top-level shape.
type Config struct {
	Channel       Channel       `yaml:"channel"`
	Sources       []Source      `yaml:"sources"`
	Clip          Clip          `yaml:"clip"`
	Transcription Transcription `yaml:"transcription"`
	Selector      Selector      `yaml:"selector"`
	Storage       Storage       `yaml:"storage"`
	Schedule      Schedule      `yaml:"schedule"`
}

// State tracks which video IDs we've already downloaded and processed.
type State struct {
	mu    sync.Mutex
	path  string
	Seen  map[string]time.Time `json:"seen"`  // youtube video ID -> first seen at
	Cut   map[string]time.Time `json:"cut"`   // video ID -> last cut at
	Fails map[string]string    `json:"fails"` // video ID -> error reason
}

func LoadState(path string) (*State, error) {
	s := &State{
		path:  path,
		Seen:  map[string]time.Time{},
		Cut:   map[string]time.Time{},
		Fails: map[string]string{},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	s.path = path
	if s.Seen == nil {
		s.Seen = map[string]time.Time{}
	}
	if s.Cut == nil {
		s.Cut = map[string]time.Time{}
	}
	if s.Fails == nil {
		s.Fails = map[string]string{}
	}
	return s, nil
}

func (s *State) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	// json tags live on Seen/Cut/Fails but not on path/mu; project just what we need
	out := struct {
		Seen  map[string]time.Time `json:"seen"`
		Cut   map[string]time.Time `json:"cut"`
		Fails map[string]string    `json:"fails"`
	}{s.Seen, s.Cut, s.Fails}
	data, err = json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write state tmp: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}

func (s *State) MarkSeen(videoID string) {
	s.mu.Lock()
	if _, ok := s.Seen[videoID]; !ok {
		s.Seen[videoID] = time.Now().UTC()
	}
	s.mu.Unlock()
}

func (s *State) IsSeen(videoID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.Seen[videoID]
	return ok
}
