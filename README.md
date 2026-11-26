# SHOT BR

Downloads recent episodes, transcribes them locally with whisper.cpp, selects
candidate moments through OpenRouter, and renders captioned vertical clips.
Review and upload are manual.

## Setup

Run commands from this directory. The project needs Go, yt-dlp, ffmpeg,
ffprobe, whisper.cpp and a multilingual Whisper model. Configure their paths in
`config.yaml`. Copy `.env.example` to `.env`, set `OPENROUTER_API_KEY`, and restrict access to the
file (`chmod 600 .env`). Do not put the key in `config.yaml`.
The Go command loads simple `KEY=value` assignments from `.env`; variables
already exported in the shell take precedence. Restart the backend after edits.

UI jobs record stage timings as JSON Lines in the `timings` directory next to
the configured state file (normally `state/timings/`). Events include duration,
outcome and cache reuse, without transcript text or provider credentials. The
pipeline panel shows elapsed time for the active stage.
When the provider returns usage, the report also records input, output,
reasoning and cached tokens, the provider/model, and cost when available.
No usage is inferred when the provider omits it.

For controlled selector experiments, set `selector.prompt_format` to
`compact` and `selector.window_seconds` to `360` in `config.yaml`. The
defaults (`legacy` and `0`) retain the existing prompt and window sizing.
Compact mode maps segment IDs and literal boundary quotes to local word
timestamps and rejects ambiguous or missing matches. Compare resulting cuts
and actual provider usage before adopting it for production.

Selection uses one provider request at a time by default. Set
`selector.concurrency: 2` to allow two window requests concurrently.
Completed windows are checkpointed and provider errors stop further scheduling.

CPU rendering uses `clip.video_encoder: libx264`. To opt in to NVIDIA GPU
encoding, set it to `h264_nvenc` and restart the backend. FFmpeg must have
NVENC support and access to a compatible GPU; if initialization fails, select
`libx264` to use the CPU. Verify video quality on your own footage.

The selector currently uses GLM 5.3 Flash through OpenRouter. Selection sends
episode transcripts to that service. The Whisper small model runs locally and
may take close to the duration of the source audio on this laptop.
The default selector requests low reasoning effort and limits output to 4,096
tokens; a truncated response is reported as an error rather than used as a cut.

The selector ranks attention signals such as surprise, strong opinions,
conflict, revelations and quotable lines. Each candidate must open with a
literal spoken hook that starts in its first three seconds; the complete quote is checked against
word timestamps, gets larger opening captions, and is reviewed along with the
complete story. Selection uses the transcript, so it cannot identify silent
visual reactions. These checks can reject every candidate and do not guarantee
retention; review the whole clip and its context. For wide shots, `fit_blur`
framing preserves the source frame over a blurred vertical background.

## Run and review

Run `./run-daily.sh` for all configured sources, or
`./run-daily.sh --source flowpodcast` to restrict a run to one source. Logs
are written to `logs/YYYY-MM-DD.log`. The script loads `.env`, prevents
overlapping daily runs, checks memory and free disk space, and returns an error
when a source fails. It does not upload clips.

To process one YouTube video on demand, run
`go run ./cmd/yt-short --pipeline --url 'https://www.youtube.com/watch?v=VIDEO_ID'`.
The URL must point to a single YouTube video; playlist pages and other sites
are not accepted. The result is labeled `adhoc` in the review page.

For frontend development, start the Go backend from the project root with
`go run ./cmd/yt-short --ui`. The Recent runs panel shows the estimated LLM cost in USD using provider-reported charges for completed calls. The amount is partial when a call does not report a cost; unavailable means no cost was reported. Job history is kept for the current studio session. In a second terminal, run `npm install` and
`npm run dev` from `web/`, then open the address printed by Vite (normally
`http://127.0.0.1:5173/`). Vite proxies API and clip requests to the backend.
Choose a configured channel and recent video, or paste
a YouTube video link. The form selects Short or regular-video output, duration,
frame, framing, captions, and a minimum editorial score from 0 to 100. The
studio scans the full transcript and renders distinct candidates that meet the
threshold and pass story and timing checks. The score is a subjective AI
estimate of attention potential, not a prediction of views or virality. The
default threshold comes from `selector.min_score` (0.0–1.0) in `config.yaml`;
`candidates_per_episode` limits proposals per transcript window, so the scan
can still miss moments. Regular videos default to 4–28 minutes and
landscape output; the 28-minute maximum is enforced by the pipeline. Renders
appear in the review queue in the studio and in `clips/pending/`. To rebuild the
embedded UI after editing it, run `npm run build:embedded` from `web/`, then
rebuild the Go binary. The embedded app is served at `http://127.0.0.1:8765/`.

Each proposed clip must explain the essential subject and context in the spoken
cut itself. Suggested titles are intended to make sense to new viewers and
create honest curiosity; descriptions add context and link to the full source
when its URL is available. These are model-generated suggestions and should be
reviewed against the source. Existing titles and descriptions are not rewritten.

Open `clips/pending/index.html` to review the video, captions and suggested
text. Confirm claims and context against the source, then upload the MP4 with
YouTube Studio if approved. `clips/pending/review.md` contains the file paths
and descriptions. The HTML page does not move, approve or publish files.

## Recovery

An episode is marked processed only after its clip and review metadata are
saved. Retrying a failed run reuses completed media, transcripts and selector
window checkpoints where available. Keep the original source media if you need
to recut without downloading it again: set `storage.keep_sources: true`.
Otherwise media is deleted after successful rendering.

If the memory guard stops a run, close unused applications and try again once
memory is available. Guard thresholds can be adjusted with
`YT_SHORT_MIN_AVAILABLE_KIB`, `YT_SHORT_MAX_SWAP_PERCENT` and
`YT_SHORT_MIN_DISK_KIB`; lowering them can make the machine unresponsive.

The existing 22:00 America/Sao_Paulo Hermes job stays paused until the real
production test and manual review pass. The laptop and scheduler need to be
running at the scheduled time.
