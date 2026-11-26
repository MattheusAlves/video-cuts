# Candidate validation investigation

Diagnostic: three 180-second windows from `KXuTmIJmEAg`, using the legacy prompt, low reasoning effort and an 8,192-token output cap. One prior GLM request at 4,096 tokens was truncated. Results are stochastic, use one source, and were not manually judged against the audio.

| Model | Proposed | Accepted | Failed duration | Failed hook | Failed story |
| --- | ---: | ---: | ---: | ---: | ---: |
| GLM 5.3 Flash | 4 | 0 | 1 | 4 | 4 |
| Gemini 3.8 Flash | 3 | 0 | 1 | 2 | 3 |

Failure categories can overlap. No proposal failed window bounds or required metadata in this sample.

## Findings

- `hasStoryEvidence` rejects any setup, development or conclusion quote over 15 normalized words. The prompt asks for short quotes but does not specify this numeric limit. Every one of the seven diagnostic candidates included at least one story quote over 15 words. The selected words may still be present and verifiable.
- `hasHookEvidence` requires the complete quoted phrase to end within three seconds, although the selection instruction says the clip should open with the hook in the first three seconds. This is stronger than requiring the hook to start in that interval. Some model quotes also exceed the unadvertised 15-word hook limit.
- Other failures remain significant: one GLM candidate had no matching conclusion inside the clip; another conclusion ended 6.1 seconds before the cut. One GLM hook did not match the transcript at all. One Gemini setup quote did not match the clip, and another hook started over three seconds into the clip. One candidate per model was shorter than the requested 45-second minimum.
- Therefore removing the quote-length limits or changing the three-second rule alone would not establish that any of these seven cuts is valid. Some fail multiple independent checks. The checks are not equivalent to human review of story quality.

## Proposed next experiment

Make the prompt and validation rules explicit and consistent, while retaining literal-word, ordering, boundary, duration and context safeguards. Evaluate whether requiring a hook to *start* within three seconds better matches the intended behavior, and whether longer exact story quotes should be accepted. Build regression cases for long literal quotes, hook start versus completion, fabricated quotes, missing endings and out-of-range duration. Replay the saved candidates offline, then have a human listen to source context and review any newly accepted candidates. Compare a second set of representative videos before changing defaults.

The diagnostic candidate files and usage reports are in the ignored local `state/comparison/validation-diagnostic/` directory. They contain transcript-derived content; do not commit them. The diagnostic's provider-reported costs were approximately $0.00356 for four GLM calls (including the truncated call) and $0.00737 for three Gemini calls.

## Offline replay after aligning the rules

The validator now requires a literal hook to start before three seconds and finish within the cut, and accepts literal hook and story quotes of 2–60 words. The prompt states those limits. Source-relative boundaries, story order, conclusion proximity, duration limits, and the separate editorial review remain in place. Changing the prompt changes the selection checkpoint fingerprint.

Replaying the seven saved proposals under the revised validator accepted zero: four GLM and three Gemini proposals still failed at least one independent safeguard. No new API calls were made for this replay. This does not establish that the video lacks good cuts; it shows that these particular proposals still need better boundaries or evidence. Further prompt or boundary refinement should be measured on this and other sources, with human audio review.
