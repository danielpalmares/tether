package config

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// DetectCodec escolhe o encoder H.264 sozinho, testando o que o FFmpeg desta
// máquina realmente oferece.
//
// Isso deixou de ser opção do painel de propósito: "h264_nvenc" vs "h264_x264"
// não significa nada para quem só quer jogar na TV, e escolher errado resulta em
// tela preta (NVENC sem placa NVIDIA) ou em CPU a 100% (libx264 num PC com
// NVIDIA disponível). A máquina sabe responder isso melhor que o usuário.
//
// Ordem de preferência: encoders de GPU primeiro (não roubam CPU do jogo, que é
// o ponto de um host de streaming), libx264 como último recurso universal.
func DetectCodec() string {
	codecDetectOnce.Do(func() {
		detectedCodec = detectCodec()
	})
	return detectedCodec
}

var (
	codecDetectOnce sync.Once
	detectedCodec   string
)

// candidatos em ordem de preferência.
var codecCandidates = []string{
	CodecH264NVENC, // NVIDIA
	CodecH264AMF,   // AMD
	CodecH264QSV,   // Intel QuickSync
	CodecH264X264,  // CPU, sempre disponível
}

func detectCodec() string {
	available := availableEncoders()
	for _, c := range codecCandidates {
		if available[ffmpegEncoderName(c)] {
			return c
		}
	}
	// Sem informação utilizável: libx264 acompanha qualquer build de FFmpeg.
	return CodecH264X264
}

// availableEncoders lê a lista de encoders do FFmpeg uma única vez.
func availableEncoders() map[string]bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-encoders").Output()
	if err != nil {
		return nil
	}

	found := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		// formato: " V....D h264_nvenc  NVIDIA NVENC H.264 encoder"
		if len(fields) >= 2 && strings.HasPrefix(fields[0], "V") {
			found[fields[1]] = true
		}
	}
	return found
}

// ffmpegEncoderName traduz o identificador interno no nome do encoder no FFmpeg.
func ffmpegEncoderName(codec string) string {
	switch codec {
	case CodecH264X264:
		return "libx264"
	default:
		return codec
	}
}

// CodecLabel descreve o encoder em uso para exibição no painel (informativo, já
// que a escolha é automática).
func CodecLabel(codec string) string {
	switch codec {
	case CodecH264NVENC:
		return "NVIDIA NVENC (GPU)"
	case CodecH264AMF:
		return "AMD AMF (GPU)"
	case CodecH264QSV:
		return "Intel QuickSync (GPU)"
	case CodecH264X264:
		return "CPU (libx264)"
	default:
		return codec
	}
}
