package audio

import "fmt"

type pcmFormat struct {
	FFmpegFormat string
	SampleRate   int
	Channels     int
	BlockAlign   int
}

func (f pcmFormat) validate() error {
	if f.FFmpegFormat == "" {
		return fmt.Errorf("formato PCM não definido")
	}
	if f.SampleRate <= 0 {
		return fmt.Errorf("sample rate PCM inválido: %d", f.SampleRate)
	}
	if f.Channels <= 0 {
		return fmt.Errorf("número de canais PCM inválido: %d", f.Channels)
	}
	if f.BlockAlign <= 0 {
		return fmt.Errorf("block align PCM inválido: %d", f.BlockAlign)
	}
	return nil
}

type pcmLoopback interface {
	Name() string
	Format() pcmFormat
	StartWriting(writer writeCloser) error
	Close() error
}

type writeCloser interface {
	Write([]byte) (int, error)
	Close() error
}
