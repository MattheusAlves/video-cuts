# SHOT BR

Downloads recent episodes, transcribes them locally with whisper.cpp, selects
candidate moments through OpenCode Go, and renders captioned vertical clips.
Review and upload are manual.

## Setup

Run commands from this directory. The project needs Go, yt-dlp, ffmpeg,
ffprobe, whisper.cpp and a multilingual Whisper model. Configure their paths in
`config.yaml`. Put `OPENCODE_API_KEY=...` in `.env` and restrict access to the
file (`chmod 600 .env`). Do not put the key in `config.yaml`.
The Go command loads simple `KEY=value` assignments from `.env`; variables
already exported in the shell take precedence. Restart the backend after edits.

The selector currently uses Kimi K3 through OpenCode Go. Selection sends
episode transcripts to that service. The Whisper small model runs locally and
may take close to the duration of the source audio on this laptop.

The selector ranks attention signals such as surprise, strong opinions,
conflict, revelations and quotable lines. Each candidate must open with a
literal spoken hook in its first three seconds; the hook is checked against
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
`go run ./cmd/yt-short --ui`. In a second terminal, run `npm install` and
`npm run dev` from `web/`, then open the address printed by Vite (normally
`http://127.0.0.1:5173/`). Vite proxies API and clip requests to the backend.
Choose a configured channel and recent video, or paste
a YouTube video link. The form selects Short or regular-video output, duration,
frame, framing, and captions. Regular videos default to 4–28 minutes and
landscape output; the 28-minute maximum is enforced by the pipeline. Renders
appear in the review queue in the studio and in `clips/pending/`. To rebuild the
embedded UI after editing it, run `npm run build:embedded` from `web/`, then
rebuild the Go binary. The embedded app is served at `http://127.0.0.1:8765/`.

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
