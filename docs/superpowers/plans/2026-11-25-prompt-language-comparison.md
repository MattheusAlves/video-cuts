# Prompt-language comparison

The same three 180-second windows from `KXuTmIJmEAg` were sent to `z-ai/glm-5.3-flash` with Portuguese instructions and an English translation. Both used the same Portuguese transcript, source-relative timestamps, requested schema, low reasoning effort, 8,192-token output cap, local validation, and Portuguese editorial review. The two prompt versions have equivalent intent but are not mechanically identical; the Portuguese run was performed first. One sample per version is not statistically conclusive.

| Instructions | Selection calls | Proposed | Passed local checks | Passed review | Elapsed | Reported cost |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Portuguese | 3 | 6 | 1 | 0 | 16 s | $0.001415 including one review |
| English | 3 | 3 | 0 | 0 | 10 s | $0.001436; no review was invoked |

All six selection responses were parseable. The editorial review returned a valid rejection of the single locally valid Portuguese candidate. English did not receive a review because no proposal passed the local gates. This experiment does not establish that either instruction language is superior or that the source lacks suitable clips; the failure to produce a reviewable candidate remains unresolved. Review time and one extra provider call are included only in the Portuguese elapsed time and cost.

Provider usage events and the (empty) approved-candidate files are stored under the ignored local `state/comparison/prompt-language/` directory. No prompt-language change was made to production configuration.
