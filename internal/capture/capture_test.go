package capture

import (
	"testing"

	"tether/internal/config"
)

func TestPipelineOrderForX264ForcesCPU(t *testing.T) {
	t.Setenv("TETHER_FFMPEG_PIPELINE", "d3d11")

	got := pipelineOrder(config.CodecH264X264)
	if len(got) != 1 || got[0] != pipelineCPU {
		t.Fatalf("pipeline order = %v, want [%s]", got, pipelineCPU)
	}
}

func TestPipelineOrderForNVENCDefaultsToD3D11Only(t *testing.T) {
	t.Setenv("TETHER_FFMPEG_PIPELINE", "")

	got := pipelineOrder(config.CodecH264NVENC)
	if len(got) != 1 || got[0] != pipelineD3D11 {
		t.Fatalf("pipeline order = %v, want [%s]", got, pipelineD3D11)
	}
}

func TestPipelineOrderForNVENCAutoAllowsCPUFallback(t *testing.T) {
	t.Setenv("TETHER_FFMPEG_PIPELINE", "auto")

	got := pipelineOrder(config.CodecH264NVENC)
	if len(got) != 2 || got[0] != pipelineD3D11 || got[1] != pipelineCPU {
		t.Fatalf("pipeline order = %v, want [%s %s]", got, pipelineD3D11, pipelineCPU)
	}
}

func TestPipelineOrderForNVENCCanForceCPUFallback(t *testing.T) {
	t.Setenv("TETHER_FFMPEG_PIPELINE", "cpu")

	got := pipelineOrder(config.CodecH264NVENC)
	if len(got) != 1 || got[0] != pipelineCPU {
		t.Fatalf("pipeline order = %v, want [%s]", got, pipelineCPU)
	}
}

func TestFFmpegArgsUseNVENCByDefault(t *testing.T) {
	c := New(config.StreamConfig{
		Width:   1920,
		Height:  1080,
		FPS:     60,
		Bitrate: 8000,
		Codec:   config.CodecH264NVENC,
	})

	args := c.ffmpegArgs(pipelineD3D11)
	requireArgValue(t, args, "-c:v", "h264_nvenc")
	requireArgValue(t, args, "-bufsize", "240k")
	requireArg(t, args, "-surfaces")
	requireNoArgValue(t, args, "-c:v", "libx264")
	requireNoArg(t, args, "-vf")
}

func TestFFmpegArgsUseX264ForUniversalCPU(t *testing.T) {
	c := New(config.StreamConfig{
		Width:   1920,
		Height:  1080,
		FPS:     60,
		Bitrate: 8000,
		Codec:   config.CodecH264X264,
	})

	args := c.ffmpegArgs(pipelineCPU)
	requireArgValue(t, args, "-vf", "hwdownload,format=bgra,format=yuv420p")
	requireArgValue(t, args, "-c:v", "libx264")
	requireArgValue(t, args, "-preset", "ultrafast")
	requireArgValue(t, args, "-tune", "zerolatency")
	requireArgValue(t, args, "-bufsize", "160k")
	requireArgValue(t, args, "-x264-params", "repeat-headers=1:sliced-threads=1:sync-lookahead=0:rc-lookahead=0")
	requireNoArg(t, args, "-nal-hrd")
	requireNoArg(t, args, "-surfaces")
	requireNoArg(t, args, "-zerolatency")
}

func requireArg(t *testing.T, args []string, flag string) {
	t.Helper()
	for _, arg := range args {
		if arg == flag {
			return
		}
	}
	t.Fatalf("args missing %s: %v", flag, args)
}

func requireNoArg(t *testing.T, args []string, flag string) {
	t.Helper()
	for _, arg := range args {
		if arg == flag {
			t.Fatalf("args should not contain %s: %v", flag, args)
		}
	}
}

func requireArgValue(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return
		}
	}
	t.Fatalf("args missing %s %s: %v", flag, value, args)
}

func requireNoArgValue(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			t.Fatalf("args should not contain %s %s: %v", flag, value, args)
		}
	}
}
