package pipeline

import "testing"

func TestFormatASSTimeCarriesRoundedSeconds(t *testing.T) {
	for _, tc := range []struct {
		seconds float64
		want    string
	}{
		{0, "0:00:00.00"}, {59.999, "0:01:00.00"}, {3599.999, "1:00:00.00"}, {-1, "0:00:00.00"},
	} {
		if got := formatASSTime(tc.seconds); got != tc.want {
			t.Errorf("formatASSTime(%v) = %q, want %q", tc.seconds, got, tc.want)
		}
	}
}
