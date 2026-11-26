package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	pl "github.com/mitnick/yt-short/pipeline"
)

// loadEnv supplies values from a simple .env file without overriding the shell.
func loadEnv(path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || key == "" {
			return fmt.Errorf("invalid .env assignment")
		}
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("invalid .env variable name")
			}
		}
	}
	return s.Err()
}

func loadConfig(path string) (pl.Config, error) {
	var cfg pl.Config
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

func requireBin(name string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("%s not found in PATH", name)
	}
	return nil
}

func main() {
	var (
		cfgPath  = flag.String("config", "config.yaml", "path to config.yaml")
		sourceID = flag.String("source", "", "limit to one source id")
		doList   = flag.Bool("list", false, "list recent episodes and exit (no download)")
		doFetch  = flag.Bool("fetch", false, "download new episodes")
		videoID  = flag.String("video", "", "fetch or operate on a single YouTube video ID")
		videoURL = flag.String("url", "", "process a single YouTube video URL with --pipeline")

		doTranscribe = flag.Bool("transcribe", false, "transcribe a downloaded episode via local whisper.cpp")
		audioPath    = flag.String("audio", "", "explicit audio path for --transcribe")

		doSelect  = flag.Bool("select", false, "run the selector over a transcript")
		transPath = flag.String("transcript", "", "transcript JSON path for --select")

		doCut = flag.Bool("cut", false, "render candidates; requires --transcript and --candidates")
		cand  = flag.String("candidates", "", "candidates JSON path for --cut")
		idx   = flag.Int("idx", -1, "render only this candidate index (-1 = all)")

		doPipeline = flag.Bool("pipeline", false, "fetch + transcribe + select + cut top candidate per unseen episode")
		longForm   = flag.Bool("longform", false, "use the 4–28 minute landscape video profile with --pipeline")

		doReview = flag.Bool("review", false, "regenerate the HTML review page from current pending clips")
		doServe  = flag.Bool("serve", false, "regenerate the page and serve clips/pending at http://localhost:8765/")
		doUI     = flag.Bool("ui", false, "open the local React video studio at http://127.0.0.1:8765/")
	)
	flag.Parse()
	if *videoURL != "" && !*doPipeline {
		log.Fatal("--url requires --pipeline")
	}
	if *longForm && !*doPipeline {
		log.Fatal("--longform requires --pipeline")
	}

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("yt-short ")
	if err := loadEnv(".env"); err != nil {
		log.Fatalf("load .env: %v", err)
	}

	if err := requireBin("yt-dlp"); err != nil {
		log.Fatalf("%v — install via 'pipx install yt-dlp' or 'sudo apt install yt-dlp'", err)
	}
	if err := requireBin("ffmpeg"); err != nil {
		log.Fatal(err)
	}
	if *doCut || *doPipeline || *doUI {
		if err := requireBin("ffprobe"); err != nil {
			log.Fatal(err)
		}
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	if *longForm {
		if err := useLongFormProfile(&cfg); err != nil {
			log.Fatal(err)
		}
	}
	st, err := pl.LoadState(cfg.Storage.StateFile)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := st.Save(); err != nil {
			log.Printf("warn: saving state: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	defer cancel()

	switch {
	case *doList:
		for _, src := range cfg.Sources {
			if *sourceID != "" && src.ID != *sourceID {
				continue
			}
			eps, err := pl.ListRecentEpisodes(ctx, src, cfg, 600)
			if err != nil {
				log.Printf("list %s: %v", src.ID, err)
				continue
			}
			fmt.Printf("\n== %s (%s) — %d recent episodes\n", src.ID, src.ChannelURL, len(eps))
			for _, e := range eps {
				seen := ""
				if st.IsSeen(e.VideoID) {
					seen = "(seen)"
				}
				fmt.Printf("  %s  %6.0fs  %s  %s\n", e.VideoID, e.DurationS, e.Title, seen)
			}
		}
		return

	case *doTranscribe:
		if *audioPath == "" {
			log.Fatal("--audio is required for --transcribe")
		}
		if *videoID == "" {
			log.Fatal("--video is required for --transcribe (used as the video_id in the transcript)")
		}
		if *sourceID == "" {
			log.Fatal("--source is required for --transcribe")
		}
		ep := pl.Episode{VideoID: *videoID, SourceID: *sourceID, AudioPath: *audioPath}
		tr, err := pl.Transcribe(ctx, cfg, ep, "")
		if err != nil {
			log.Fatal(err)
		}
		path, err := pl.SaveTranscript(cfg, *sourceID, tr)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("OK transcript=%s (lang=%s dur=%.1fs words=%d segments=%d)\n",
			path, tr.Language, tr.DurationS, len(tr.Words), len(tr.Segments))
		return

	case *doSelect:
		if *transPath == "" || *videoID == "" || *sourceID == "" {
			log.Fatal("--transcript, --video, --source are required for --select")
		}
		tr, ep, err := loadTranscriptAndEpisode(*transPath)
		if err != nil {
			log.Fatal(err)
		}
		ep.SourceID = *sourceID
		cands, err := pl.Select(ctx, cfg, ep, tr)
		if err != nil {
			log.Fatal(err)
		}
		if len(cands) == 0 {
			log.Fatal("selector returned 0 candidates")
		}
		outPath := strings.TrimSuffix(*transPath, ".json") + ".candidates.json"
		if err := saveJSON(outPath, cands); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("OK candidates=%s (%d clips)\n", outPath, len(cands))
		for i, c := range cands {
			fmt.Printf("  [%d] %.1f-%0.1f  (%.0fs) score=%.2f  %s\n", i, c.Start, c.End, c.End-c.Start, c.Score, c.Title)
		}
		return

	case *doCut:
		if *transPath == "" || *cand == "" {
			log.Fatal("--transcript and --candidates are required for --cut")
		}
		tr, ep, err := loadTranscriptAndEpisode(*transPath)
		if err != nil {
			log.Fatal(err)
		}
		var cands []pl.Candidate
		if err := loadJSON(*cand, &cands); err != nil {
			log.Fatal(err)
		}
		if *sourceID != "" {
			ep.SourceID = *sourceID
		}
		if ep.URL == "" {
			ep.URL = "https://www.youtube.com/watch?v=" + ep.VideoID
		}
		if ep.Title == "" {
			ep.Title = ep.SourceID
		}
		if ep.VideoPath == "" {
			// Try the conventional filename pattern, including manual --video downloads.
			for _, prefix := range []string{ep.SourceID, "adhoc"} {
				if prefix == "" {
					continue
				}
				matches, _ := filepath.Glob(filepath.Join(cfg.Storage.SourcesDir, prefix+"__"+ep.VideoID+".mp4"))
				if len(matches) > 0 {
					ep.VideoPath = matches[0]
					break
				}
			}
			if ep.VideoPath == "" {
				log.Fatalf("could not infer video path for %s; download it first with --fetch --video %s", ep.VideoID, ep.VideoID)
			}
		}
		rendered := 0
		failed := false
		for i, c := range cands {
			if *idx >= 0 && *idx != i {
				continue
			}
			if c.VideoID != ep.VideoID || c.Start < 0 || c.End > tr.DurationS || c.End-c.Start < float64(cfg.Clip.TargetSecondsMin) || c.End-c.Start > float64(cfg.Clip.TargetSecondsMax) {
				log.Printf("cut [%d]: candidate has invalid episode or timestamps", i)
				failed = true
				continue
			}
			out, err := pl.Cut(ctx, cfg, ep, tr, c)
			if err != nil {
				log.Printf("cut [%d]: %v", i, err)
				failed = true
				continue
			}
			meta := pl.PendingClip{Candidate: c, SourceTitle: ep.Title, SourceURL: ep.URL, ClipPath: out, CreatedAt: pl.NowUTC()}
			if err := pl.WritePendingSidecar(out, meta); err != nil {
				log.Printf("cut [%d] sidecar: %v", i, err)
				failed = true
				continue
			}
			rendered++
			fmt.Printf("OK [%d] %s (%s)\n", i, out, c.Title)
		}
		if rendered > 0 {
			if err := pl.WriteReviewPage(cfg); err != nil {
				log.Fatal(err)
			}
		}
		if rendered == 0 || failed {
			log.Fatal("one or more clips could not be rendered")
		}
		return

	case *doPipeline:
		if *videoURL != "" {
			if *sourceID != "" || *videoID != "" {
				log.Fatal("--url cannot be combined with --source or --video")
			}
			if err := runPipelineURL(ctx, cfg, st, *videoURL); err != nil {
				log.Fatal(err)
			}
			return
		}
		if err := runPipeline(ctx, cfg, st, *sourceID, *videoID); err != nil {
			log.Fatal(err)
		}
		return

	case *doReview:
		if err := pl.WriteReviewPage(cfg); err != nil {
			log.Fatal(err)
		}
		abs, _ := filepath.Abs(cfg.Storage.ClipsPending)
		fmt.Printf("OK %s\n", filepath.Join(abs, "index.html"))
		return

	case *doServe:
		if err := pl.WriteReviewPage(cfg); err != nil {
			log.Fatal(err)
		}
		abs, _ := filepath.Abs(cfg.Storage.ClipsPending)
		fmt.Printf("Serving %s at http://localhost:8765/\n", abs)
		if err := http.ListenAndServe("127.0.0.1:8765", http.FileServer(http.Dir(abs))); err != nil {
			log.Fatal(err)
		}
		return

	case *doUI:
		if err := serveUI(ctx, cfg, st); err != nil {
			log.Fatal(err)
		}
		return

	case *doFetch:
		if *videoID != "" {
			// Ad-hoc single video fetch using the default episode layout
			ep := pl.Episode{
				VideoID:  *videoID,
				SourceID: "adhoc",
				URL:      "https://www.youtube.com/watch?v=" + *videoID,
			}
			ep, err = pl.DownloadEpisode(ctx, ep, cfg)
			if err != nil {
				log.Fatal(err)
			}
			fmt.Printf("OK audio=%s video=%s\n", ep.AudioPath, ep.VideoPath)
			if err := saveEpisodeMeta(ep, cfg); err != nil {
				log.Fatalf("save metadata: %v", err)
			}
			return
		}

		for _, src := range cfg.Sources {
			if *sourceID != "" && src.ID != *sourceID {
				continue
			}
			eps, err := pl.ListRecentEpisodes(ctx, src, cfg, 600)
			if err != nil {
				log.Printf("list %s: %v", src.ID, err)
				continue
			}
			for _, e := range eps {
				if st.IsSeen(e.VideoID) {
					continue
				}
				log.Printf("fetching %s [%s] %q", e.VideoID, src.ID, e.Title)
				dl, err := pl.DownloadEpisode(ctx, e, cfg)
				if err != nil {
					log.Printf("download %s: %v", e.VideoID, err)
					st.Fails[e.VideoID] = err.Error()
					continue
				}
				// Persist episode metadata next to the media
				if err := saveEpisodeMeta(dl, cfg); err != nil {
					log.Printf("save meta %s: %v", dl.VideoID, err)
				}
			}
		}
		return

	default:
		flag.Usage()
		os.Exit(2)
	}
}

func saveJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func saveJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".marker-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func loadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// loadTranscriptAndEpisode reads a transcript file and reconstructs a minimal
// Episode from the matching sources/*.json (if present) and the video files
// in sources/. DurationS is taken from the transcript.
func loadTranscriptAndEpisode(transPath string) (tr pl.Transcript, ep pl.Episode, err error) {
	if err := loadJSON(transPath, &tr); err != nil {
		return tr, ep, fmt.Errorf("read transcript: %w", err)
	}
	// Infer sourceID from filename "<src>__<vid>.json"
	base := filepath.Base(transPath)
	parts := strings.SplitN(strings.TrimSuffix(base, ".json"), "__", 2)
	if len(parts) == 2 {
		ep.SourceID = parts[0]
	}
	ep.VideoID = tr.VideoID
	ep.DurationS = tr.DurationS

	// Try to load episode metadata if available.
	metaPath := filepath.Join("sources", ep.SourceID+"__"+ep.VideoID+".json")
	var meta pl.Episode
	if err := loadJSON(metaPath, &meta); err == nil {
		meta.VideoID = tr.VideoID
		meta.DurationS = tr.DurationS
		ep = meta
	}
	return tr, ep, nil
}

func saveEpisodeMeta(ep pl.Episode, cfg pl.Config) error {
	if err := os.MkdirAll(cfg.Storage.SourcesDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(cfg.Storage.SourcesDir, ep.SourceID+"__"+ep.VideoID+".json")
	b, err := json.MarshalIndent(ep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
