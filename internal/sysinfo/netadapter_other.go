//go:build !windows

package sysinfo

import (
	"context"
	"errors"
)

// DetectAdapter não é suportado fora do Windows (host é Windows-only hoje).
func DetectAdapter(_ context.Context) (AdapterInfo, error) {
	return AdapterInfo{}, errors.New("detecção de adaptador disponível apenas no Windows")
}
