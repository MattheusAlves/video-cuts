package pipeline

import "testing"

func TestValidateYouTubeVideoURLAcceptsSingleVideoLinksAndRejectsOthers(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		ok   bool
	}{
		{name: "watch URL", url: "https://www.youtube.com/watch?v=video123", ok: true},
		{name: "short URL", url: "https://youtu.be/video123", ok: true},
		{name: "shorts URL", url: "https://m.youtube.com/shorts/video123", ok: true},
		{name: "playlist page", url: "https://www.youtube.com/playlist?list=playlist123", ok: false},
		{name: "other host", url: "https://example.com/watch?v=video123", ok: false},
		{name: "youtube lookalike host", url: "https://youtube.com.evil.example/watch?v=video123", ok: false},
		{name: "missing video ID", url: "https://youtube.com/watch?v=", ok: false},
		{name: "unsupported scheme", url: "file://youtube.com/watch?v=video123", ok: false},
		{name: "empty URL", url: "", ok: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateYouTubeVideoURL(tc.url)
			if (err == nil) != tc.ok {
				t.Fatalf("ValidateYouTubeVideoURL(%q) error = %v, want success=%t", tc.url, err, tc.ok)
			}
		})
	}
}
