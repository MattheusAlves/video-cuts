# Performance options for review

Evaluate these after moving to faster hardware. The recorded 44-minute Flow episode took about 32 minutes to transcribe on the previous laptop; selector calls took roughly 53–89 seconds per window before the provider quota stopped the run. These are historical measurements, not predictions for other hardware.

- **Fast selection:** stop once a strong, complete cut passes review. This makes fewer selector calls, but can miss better moments later in the source. Offer a full-scan option.
- **Transcription model:** benchmark a smaller or accelerated Whisper model against the current `small` model using Portuguese speech, names, and word timestamps. Compare speed and cut quality before changing the default.
- **Time range:** let users select a section of the source to download and process when they know where to look. Preserve source-relative timestamps.
- **Progress and timing:** show the active stage and elapsed time for download, transcription, selection, and rendering. Record timings so bottlenecks can be measured on the new machine.
- **Hardware acceleration:** evaluate supported GPU transcription and video encoding on the new hardware. Check quality and memory use before enabling it by default.
- **Parallelism:** consider parallel selector requests or pipeline stages only after measuring provider rate limits and CPU, memory, and disk capacity; preserve quota handling and avoid duplicate work.

Reuse existing media, transcripts, and selection checkpoints when possible. Measure end-to-end time and resulting cut quality on the same representative videos before and after changes.
