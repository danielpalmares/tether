package capture

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"tether/internal/config"
)

// TestLiveCaptureSmoke roda o pipeline REAL (ddagrab + encoder) por ~2s e
// confere que bytes H.264 saem. Gated por TETHER_LIVE_TEST=1: exige GPU/display
// da máquina, não roda no CI.
func TestLiveCaptureSmoke(t *testing.T) {
	if os.Getenv("TETHER_LIVE_TEST") != "1" {
		t.Skip("defina TETHER_LIVE_TEST=1 para rodar o smoke do pipeline real")
	}

	c := New(config.Default())
	stream, err := c.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Stop()
	defer stream.Close()

	deadline := time.Now().Add(2 * time.Second)
	buf := make([]byte, 64*1024)
	var total int64
	for time.Now().Before(deadline) {
		n, err := stream.Read(buf)
		total += int64(n)
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("read: %v", err)
		}
	}
	if total == 0 {
		t.Fatal("pipeline real não produziu bytes H.264")
	}
	t.Logf("pipeline real ok: %d KB em ~2s", total/1024)
}
