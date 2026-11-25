package pipeline

import "context"

// Progress describes work underway. Window counts apply only to selection.
type Progress struct {
	Stage     string `json:"stage"`
	Message   string `json:"message"`
	Completed int    `json:"completed,omitempty"`
	Total     int    `json:"total,omitempty"`
}

type progressKey struct{}

func WithProgress(ctx context.Context, report func(Progress)) context.Context {
	return context.WithValue(ctx, progressKey{}, report)
}

func ReportProgress(ctx context.Context, progress Progress) {
	if report, ok := ctx.Value(progressKey{}).(func(Progress)); ok && report != nil {
		report(progress)
	}
}
