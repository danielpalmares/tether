//go:build !windows

package audio

import (
	"context"
	"fmt"
)

func newWASAPILoopback(context.Context) (pcmLoopback, error) {
	return nil, fmt.Errorf("WASAPI disponível apenas no Windows")
}
