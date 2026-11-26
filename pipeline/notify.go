package pipeline

import (
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// PendingClip is the sidecar metadata used by the human review page.
type PendingClip struct {
	Candidate
	SourceTitle       string    `json:"source_title"`
	SourceURL         string    `json:"source_url"`
	ClipPath          string    `json:"clip_path"`
	CreatedAt         time.Time `json:"created_at"`
	RenderFingerprint string    `json:"render_fingerprint,omitempty"`
	UploadDescription string    `json:"upload_description,omitempty"`
	ThumbnailPath     string    `json:"thumbnail_path,omitempty"`
}

// NowUTC is a small indirection so tests can stub the clock later.
func NowUTC() time.Time { return time.Now().UTC() }

// WritePendingSidecar persists clip metadata next to the mp4.
func WritePendingSidecar(clipPath string, meta PendingClip) error {
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	path := strings.TrimSuffix(clipPath, ".mp4") + ".json"
	f, err := os.CreateTemp(filepath.Dir(path), ".sidecar-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// UploadDescription assembles the suggested copy and a link to the full source.
func UploadDescription(clip PendingClip) string {
	var parts []string
	if text := strings.TrimSpace(clip.Description); text != "" {
		parts = append(parts, text)
	}
	if source := strings.TrimSpace(clip.SourceURL); source != "" {
		parts = append(parts, "Assista ao vídeo completo: "+source)
	}
	if tags := strings.TrimSpace(strings.Join(clip.Hashtags, " ")); tags != "" {
		parts = append(parts, tags)
	}
	return strings.Join(parts, "\n\n")
}

// ListPending reads all pending clip sidecars, newest first.
func ListPending(cfg Config) ([]PendingClip, error) {
	paths, err := filepath.Glob(filepath.Join(cfg.Storage.ClipsPending, "*.json"))
	if err != nil {
		return nil, err
	}
	var clips []PendingClip
	for _, p := range paths {
		if strings.HasSuffix(p, ".rendering.json") {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var c PendingClip
		if err := json.Unmarshal(b, &c); err != nil || c.ClipPath == "" {
			continue
		}
		info, err := os.Stat(c.ClipPath)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			continue
		}
		if info, err := os.Stat(ThumbnailPath(c.ClipPath)); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
			c.ThumbnailPath = ThumbnailPath(c.ClipPath)
		}
		c.UploadDescription = UploadDescription(c)
		clips = append(clips, c)
	}
	sort.SliceStable(clips, func(i, j int) bool {
		return clips[i].CreatedAt.After(clips[j].CreatedAt)
	})
	return clips, nil
}

// WriteReviewPage creates a local HTML review app and a Markdown digest.
// The HTML plays videos directly and exposes upload metadata for copy/paste.
func WriteReviewPage(cfg Config) error {
	clips, err := ListPending(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.Storage.ClipsPending, 0o755); err != nil {
		return err
	}
	var h strings.Builder
	h.WriteString(`<!doctype html><html lang="pt-BR"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>SHOT BR — Revisão</title>
<style>
body{font:16px system-ui,sans-serif;background:#111;color:#eee;margin:0;padding:24px}
h1{margin-top:0}.clip{display:flex;gap:24px;border-top:1px solid #444;padding:24px 0}
video{width:270px;height:480px;background:#000;max-width:45vw}.meta{flex:1;min-width:280px}
textarea{width:100%;box-sizing:border-box;background:#1d1d1d;color:#eee;border:1px solid #555;padding:8px}
button{margin:5px 5px 5px 0;padding:8px 12px;cursor:pointer}.small{color:#aaa;font-size:.9em}
code{user-select:all}.score{font-size:1.2em}
</style></head><body><h1>SHOT BR — Revisão diária</h1>
<p>Assista, aprove ou rejeite. Para publicar, arraste o MP4 aprovado para o YouTube Studio e copie o título/descrição.</p>`)
	if len(clips) == 0 {
		h.WriteString("<p>Nenhum corte pendente.</p>")
	}
	var md strings.Builder
	md.WriteString("# SHOT BR — cortes pendentes\n\n")
	for _, c := range clips {
		base := filepath.Base(c.ClipPath)
		title := html.EscapeString(c.Title)
		desc := html.EscapeString(c.UploadDescription)
		h.WriteString(`<section class="clip"><video controls preload="metadata" src="` + html.EscapeString(base) + `"></video>`)
		if c.ThumbnailPath != "" {
			h.WriteString(`<a href="` + html.EscapeString(filepath.Base(c.ThumbnailPath)) + `" download>Baixar capa</a>`)
		}
		h.WriteString(`<div class="meta"><div class="score">Score ` + fmt.Sprintf("%.2f", c.Score) + `</div>`)
		h.WriteString("<h2>" + title + "</h2>")
		h.WriteString(`<p class="small">Fonte: <a style="color:#9cf" href="` + html.EscapeString(c.SourceURL) + `">` + html.EscapeString(c.SourceTitle) + `</a><br>`)
		h.WriteString(fmt.Sprintf("Janela: %.1fs–%.1fs (%.1fs) · Arquivo: <code>%s</code></p>", c.Start, c.End, c.End-c.Start, html.EscapeString(c.ClipPath)))
		h.WriteString("<p>" + html.EscapeString(c.Rationale) + "</p>")
		if c.HookQuote != "" {
			h.WriteString("<p><strong>Gancho (primeiros 3s):</strong> “" + html.EscapeString(c.HookQuote) + "”</p>")
		}
		if c.ViralityReason != "" {
			h.WriteString("<p><strong>Potencial de retenção:</strong> " + html.EscapeString(c.ViralityReason) + "</p>")
		}
		if len(c.AttentionSignals) > 0 {
			h.WriteString("<p class=\"small\"><strong>Sinais:</strong> " + html.EscapeString(strings.Join(c.AttentionSignals, ", ")) + "</p>")
		}
		if c.Story.SetupQuote != "" && c.Story.DevelopmentQuote != "" && c.Story.ConclusionQuote != "" {
			h.WriteString("<details><summary>Estrutura da história</summary><p><strong>Início:</strong> " + html.EscapeString(c.Story.SetupQuote) + "</p><p><strong>Desenvolvimento:</strong> " + html.EscapeString(c.Story.DevelopmentQuote) + "</p><p><strong>Conclusão:</strong> " + html.EscapeString(c.Story.ConclusionQuote) + "</p></details>")
		}
		h.WriteString("<label>Título</label><textarea rows=2 readonly>" + title + `</textarea>`)
		h.WriteString("<label>Descrição</label><textarea rows=6 readonly>" + desc + "</textarea></div></section>")

		abs, _ := filepath.Abs(c.ClipPath)
		md.WriteString(fmt.Sprintf("## %s\n\n- Arquivo: `%s`\n- Fonte: %s — %s\n- Score: %.2f\n- Janela: %.1fs–%.1fs\n- Gancho (primeiros 3s): %s\n- Sinais: %s\n- Potencial de retenção: %s\n- Motivo: %s\n\n%s\n\n%s\n\n",
			c.Title, abs, c.SourceTitle, c.SourceURL, c.Score, c.Start, c.End, c.HookQuote, strings.Join(c.AttentionSignals, ", "), c.ViralityReason, c.Rationale, c.Description, strings.Join(c.Hashtags, " ")))
		if c.Story.SetupQuote != "" && c.Story.DevelopmentQuote != "" && c.Story.ConclusionQuote != "" {
			md.WriteString(fmt.Sprintf("- Início: %s\n- Desenvolvimento: %s\n- Conclusão: %s\n\n", c.Story.SetupQuote, c.Story.DevelopmentQuote, c.Story.ConclusionQuote))
		}
	}
	h.WriteString("</body></html>")
	if err := os.WriteFile(filepath.Join(cfg.Storage.ClipsPending, "index.html"), []byte(h.String()), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cfg.Storage.ClipsPending, "review.md"), []byte(md.String()), 0o644)
}
