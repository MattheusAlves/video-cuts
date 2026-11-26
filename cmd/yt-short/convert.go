package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	pl "github.com/mitnick/yt-short/pipeline"
)

func (s *uiServer) conversionClip(name string) (pl.PendingClip, error) {
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\`) || !strings.HasSuffix(strings.ToLower(name), ".mp4") {
		return pl.PendingClip{}, fmt.Errorf("invalid saved clip")
	}
	clips, err := pl.ListPending(s.cfg)
	if err != nil {
		return pl.PendingClip{}, err
	}
	for _, c := range clips {
		if filepath.Base(c.ClipPath) != name || filepath.Clean(filepath.Dir(c.ClipPath)) != filepath.Clean(s.cfg.Storage.ClipsPending) {
			continue
		}
		if c.SourceID == "" || c.VideoID == "" || strings.ContainsAny(c.SourceID+c.VideoID, `/\`) || c.SourceID == ".." || c.VideoID == ".." {
			return pl.PendingClip{}, fmt.Errorf("invalid source identifiers")
		}
		if err := pl.ValidateYouTubeVideoURL(c.SourceURL); err != nil {
			return pl.PendingClip{}, fmt.Errorf("saved clip has no valid source URL")
		}
		return c, nil
	}
	return pl.PendingClip{}, fmt.Errorf("saved clip not found")
}

func (s *uiServer) convertSavedClip(ctx context.Context, cfg pl.Config, request jobRequest) error {
	clip, err := s.conversionClip(request.ConvertClip)
	if err != nil {
		return err
	}
	if err := pipelinePreflight(ctx, cfg); err != nil {
		return err
	}
	ep := pl.Episode{SourceID: clip.SourceID, VideoID: clip.VideoID, Title: clip.SourceTitle, URL: clip.SourceURL}
	base := filepath.Join(cfg.Storage.SourcesDir, ep.SourceID+"__"+ep.VideoID)
	transPath := filepath.Join(cfg.Storage.Transcripts, ep.SourceID+"__"+ep.VideoID+".json")
	var tr pl.Transcript
	haveTranscript := loadJSON(transPath, &tr) == nil && tr.VideoID == ep.VideoID && tr.DurationS > 0 && len(tr.Segments) > 0 && len(tr.Words) > 0
	if nonemptyFile(base+".mp4") && (haveTranscript || nonemptyFile(base+".audio.m4a")) {
		ep.VideoPath, ep.AudioPath = base+".mp4", base+".audio.m4a"
		pl.StartTiming(ctx, pl.TimingEvent{Stage: "download", SourceID: ep.SourceID, VideoID: ep.VideoID, Reused: true})(nil)
	} else {
		pl.ReportProgress(ctx, pl.Progress{Stage: "download", Message: "Downloading the original video"})
		inspected, err := pl.InspectYouTubeURL(ctx, ep.URL)
		if err != nil {
			return fmt.Errorf("inspect original video: %w", err)
		}
		if inspected.VideoID != ep.VideoID {
			return fmt.Errorf("saved clip source URL does not match its video ID")
		}
		finish := pl.StartTiming(ctx, pl.TimingEvent{Stage: "download", SourceID: ep.SourceID, VideoID: ep.VideoID})
		ep, err = pl.DownloadEpisode(ctx, ep, cfg)
		finish(err)
		if err != nil {
			return fmt.Errorf("download original video: %w", err)
		}
	}
	if !haveTranscript {
		pl.ReportProgress(ctx, pl.Progress{Stage: "transcribe", Message: "Transcribing the original video"})
		tr, err = pl.Transcribe(ctx, cfg, ep, "")
		if err != nil {
			return err
		}
		if _, err = pl.SaveTranscript(cfg, ep.SourceID, tr); err != nil {
			return err
		}
	} else {
		pl.StartTiming(ctx, pl.TimingEvent{Stage: "transcription", SourceID: ep.SourceID, VideoID: ep.VideoID, Reused: true})(nil)
	}
	ep.DurationS = tr.DurationS
	pl.ReportProgress(ctx, pl.Progress{Stage: "select", Message: "Finding a self-contained version of the selected cut"})
	finish := pl.StartTiming(ctx, pl.TimingEvent{Stage: "selection", SourceID: ep.SourceID, VideoID: ep.VideoID})
	candidate, err := pl.SelectConversion(ctx, cfg, ep, tr, clip.Candidate, request.Mode == "video")
	finish(err)
	if err != nil {
		return err
	}
	path := pl.CutOutputPath(cfg, ep, candidate)
	markerPath := path + ".rendering.json"
	fingerprint := pl.RenderFingerprint(cfg, candidate, tr)
	meta := pl.PendingClip{Candidate: candidate, SourceTitle: ep.Title, SourceURL: ep.URL, ClipPath: path, CreatedAt: pl.NowUTC(), RenderFingerprint: fingerprint}
	if filepath.Clean(path) == filepath.Clean(clip.ClipPath) {
		return fmt.Errorf("conversion would replace the original cut")
	}
	if nonemptyFile(path) {
		var existing pl.PendingClip
		if loadJSON(strings.TrimSuffix(path, ".mp4")+".json", &existing) == nil && existing.RenderFingerprint == fingerprint && pl.VerifyRenderedClip(ctx, cfg, path, candidate.End-candidate.Start) == nil {
			if err := ensureThumbnail(ctx, path, candidate); err != nil {
				return err
			}
			return nil
		}
		var marker pl.PendingClip
		if loadJSON(markerPath, &marker) != nil || marker.RenderFingerprint != fingerprint || marker.ClipPath != path || pl.VerifyRenderedClip(ctx, cfg, path, candidate.End-candidate.Start) != nil {
			return fmt.Errorf("conversion output already exists and cannot be safely reused: %s", path)
		}
		if err := ensureThumbnail(ctx, path, candidate); err != nil {
			return err
		}
		if err := pl.WritePendingSidecar(path, marker); err != nil {
			return err
		}
		return os.Remove(markerPath)
	}
	if _, err := os.Stat(markerPath); err == nil {
		var marker pl.PendingClip
		if loadJSON(markerPath, &marker) != nil || marker.RenderFingerprint != fingerprint || marker.ClipPath != path {
			return fmt.Errorf("incompatible conversion recovery marker: %s", markerPath)
		}
	} else if !os.IsNotExist(err) {
		return err
	} else if err := saveJSONAtomic(markerPath, meta); err != nil {
		return err
	}
	pl.ReportProgress(ctx, pl.Progress{Stage: "render", Message: "Rendering the converted cut"})
	finish = pl.StartTiming(ctx, pl.TimingEvent{Stage: "render", SourceID: ep.SourceID, VideoID: ep.VideoID})
	_, err = pl.Cut(ctx, cfg, ep, tr, candidate)
	finish(err)
	if err != nil {
		return err
	}
	if err = ensureThumbnail(ctx, path, candidate); err != nil {
		return err
	}
	if err = pl.WritePendingSidecar(path, meta); err != nil {
		return err
	}
	if err = os.Remove(markerPath); err != nil {
		return err
	}
	pl.ReportProgress(ctx, pl.Progress{Stage: "finalize", Message: "Converted cut ready for review"})
	return nil
}
