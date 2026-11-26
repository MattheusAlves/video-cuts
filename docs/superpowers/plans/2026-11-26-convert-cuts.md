# Convert a saved cut to another format

1. Add a selector for an anchored conversion. For expansion, inspect nearby original transcript and require the selected range to contain and exceed the original cut. For shortening, require a strictly shorter range inside it. Validate bounds, duration, literal story evidence and editorial review; reject unsupported or invalid proposals. Exercise both directions and failure cases with mocked provider responses.
2. Add a conversion job that accepts a saved clip identifier, validates it against the pending queue, reuses or downloads the original media and transcript, runs the anchored selector, and renders a new sidecar and cover without replacing the original. Preserve recoverability on failures. Test request validation and job integration.
3. Add an action to each clip for converting to the other format, using existing job progress and cost reporting. Build the embedded studio and run Go race tests, frontend build, and diff checks.
