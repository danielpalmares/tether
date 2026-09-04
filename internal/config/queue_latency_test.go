package config

import "testing"

// A fila de vídeo é LATÊNCIA: cada frame parado nela custa ~16.7ms a 60fps.
// O perfil "smooth" tinha fila de 100ms (6 frames) e vivia cheio — medido 5-6
// de 6 em sessão real — somando ~100ms fixos de atraso ao que a TV já
// bufferiza. "Suave" tem de vir do hold do pacer, não de empilhar frames.
func TestQueueDepthStaysLowEnoughForLatency(t *testing.T) {
	const fps = 60
	// Teto: 3 frames = 50ms. Acima disso a fila deixa de ser absorvedor de
	// jitter e vira acumulador de atraso perceptível.
	const maxFrames = 3

	for _, p := range []LatencyProfile{LatencyUltra, LatencyBalanced, LatencySmooth} {
		c := StreamConfig{Width: 1920, Height: 1080, FPS: fps, Bitrate: 20000, Latency: p}
		depth := c.Tuning().FrameQueueDepth
		ms := float64(depth) * 1000 / fps
		t.Logf("%-9s fila=%d frames (~%.0fms)", p, depth, ms)
		if depth > maxFrames {
			t.Errorf("perfil %s: fila de %d frames (~%.0fms) acumula latência demais", p, depth, ms)
		}
	}
}
