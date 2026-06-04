package webrtc

import (
	"testing"
	"time"
)

func TestRTPTimestampSamplesRoundsCommonFrameRates(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		want     uint32
	}{
		{name: "30fps", duration: time.Second / 30, want: 3000},
		{name: "60fps", duration: time.Second / 60, want: 1500},
		{name: "120fps", duration: time.Second / 120, want: 750},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rtpTimestampSamples(tt.duration, 90000); got != tt.want {
				t.Fatalf("rtpTimestampSamples(%s) = %d, want %d", tt.duration, got, tt.want)
			}
		})
	}
}
