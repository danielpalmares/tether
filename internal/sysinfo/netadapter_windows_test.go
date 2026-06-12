//go:build windows

package sysinfo

import (
	"context"
	"testing"
)

func TestParseLinkSpeedMbps(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"100 Mbps", 100},
		{"1 Gbps", 1000},
		{"866.7 Mbps", 866},
		{"866,7 Mbps", 866},
		{"", 0},
		{"Unknown", 0},
	}
	for _, c := range cases {
		if got := parseLinkSpeedMbps(c.in); got != c.want {
			t.Errorf("parseLinkSpeedMbps(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestDetectAdapterSmoke roda a detecção real (requer rede ativa na máquina).
func TestDetectAdapterSmoke(t *testing.T) {
	info, err := DetectAdapter(context.Background())
	if err != nil {
		t.Fatalf("DetectAdapter: %v", err)
	}
	if info.Name == "" || info.Type == "unknown" && info.LinkMbps == 0 {
		t.Errorf("adaptador suspeito: %+v", info)
	}
	t.Logf("adaptador: %+v", info)
}
