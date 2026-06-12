//go:build windows

package sysinfo

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// detectScript resolve o adaptador físico que possui a rota default (o que
// realmente fala com a LAN) e devolve nome, velocidade de link e tipo de mídia.
const detectScript = `$idx = (Get-NetRoute -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue | Sort-Object RouteMetric | Select-Object -First 1).ifIndex; if ($idx) { Get-NetAdapter -InterfaceIndex $idx | Select-Object Name, LinkSpeed, PhysicalMediaType | ConvertTo-Json -Compress }`

type rawAdapter struct {
	Name              string `json:"Name"`
	LinkSpeed         string `json:"LinkSpeed"`
	PhysicalMediaType string `json:"PhysicalMediaType"`
}

// DetectAdapter consulta o adaptador de rede ativo via PowerShell. Chamado uma
// única vez no boot do host, em background. Timeout generoso: o cold start do
// PowerShell durante o boot do host (sistema ocupado) passa fácil de 10s, e o
// kill por contexto viraria um falso "exit status 1".
func DetectAdapter(ctx context.Context) (AdapterInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx,
		"powershell", "-NoProfile", "-NonInteractive", "-Command", detectScript,
	).Output()
	if err != nil {
		return AdapterInfo{}, err
	}

	var raw rawAdapter
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &raw); err != nil {
		return AdapterInfo{}, err
	}

	return AdapterInfo{
		Name:     raw.Name,
		LinkMbps: parseLinkSpeedMbps(raw.LinkSpeed),
		Type:     mediaType(raw.PhysicalMediaType),
	}, nil
}

// parseLinkSpeedMbps converte o LinkSpeed do Get-NetAdapter ("866.7 Mbps",
// "1 Gbps") para Mbps inteiro. 0 = desconhecido.
func parseLinkSpeedMbps(s string) int {
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) < 2 {
		return 0
	}
	value, err := strconv.ParseFloat(strings.ReplaceAll(fields[0], ",", "."), 64)
	if err != nil {
		return 0
	}
	switch strings.ToLower(fields[1]) {
	case "gbps":
		return int(value * 1000)
	case "mbps":
		return int(value)
	case "kbps":
		return int(value / 1000)
	default:
		return 0
	}
}

func mediaType(physical string) string {
	switch {
	case strings.Contains(physical, "802.11"):
		return "wifi"
	case strings.Contains(physical, "802.3"):
		return "ethernet"
	default:
		return "unknown"
	}
}
