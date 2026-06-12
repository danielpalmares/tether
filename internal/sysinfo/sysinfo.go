// Package sysinfo expõe informações do sistema do host relevantes para o
// client dimensionar a transmissão (ex.: link de rede do host).
package sysinfo

// AdapterInfo descreve o adaptador de rede ativo do host — o lado do enlace que
// o client não consegue medir sozinho no teste de capacidade da LAN.
type AdapterInfo struct {
	Name     string `json:"adapter"`
	LinkMbps int    `json:"linkMbps"` // 0 = desconhecido
	Type     string `json:"type"`     // "wifi" | "ethernet" | "unknown"
}
