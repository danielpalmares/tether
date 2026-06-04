# Tether · Steam Link para navegador (LAN)

Streaming de jogos pela rede local com **app host nativo** (Go) e **client 100% no navegador** (WebRTC + Gamepad API). O host abre o **Steam Big Picture** automaticamente quando um client conecta, transmite a tela com baixa latência e recebe o input do controle Xbox de volta.

```
HOST (Go, nativo)                          CLIENT (browser puro)
 ├─ serve painel web (localhost:8787)       ├─ abre client.html
 ├─ captura tela (DXGI/ddagrab)             ├─ recebe vídeo H.264 (WebRTC)
 ├─ encode H.264 (NVENC, low-latency)  ───► ├─ lê controle (Gamepad API)
 ├─ servidor WebRTC (Pion)             ◄─── └─ envia input @60Hz (data channel)
 └─ injeta input (ViGEmBus → Xbox 360 virtual)
```

## Requisitos (host)

- **Windows 10/11**
- **GPU NVIDIA** (encoder NVENC)
- **FFmpeg** no PATH, com suporte a `ddagrab` e `h264_nvenc` (builds gyan.dev/BtbN têm)
- **ViGEmBus** instalado: https://github.com/ViGEm/ViGEmBus/releases
- **Go 1.22+** (para compilar)
- **Steam** instalado e logado

## Requisitos (client)

- Qualquer navegador moderno (Chrome/Edge recomendados) na **mesma rede**
- Controle Xbox conectado

## Build

```bash
# já vem com vendor/ — não precisa de internet
go build -o tether-host.exe ./cmd/host
```

> O projeto usa `replace` no go.mod para resolver módulos via GitHub e inclui
> `vendor/`, então o build é offline.

## Uso

1. No **PC host**, rode `tether-host.exe`. O painel abre no navegador
   (`http://localhost:8787/host.html`). Ajuste resolução, FPS e bitrate, salve.
   O terminal mostra o endereço LAN do client (ex.: `http://192.168.0.10:8787/client.html`).
2. No **PC client**, abra esse endereço no navegador.
3. Em **NET · 01**, digite o `IP:porta` do host e clique **vincular host**.
4. Conecte o controle Xbox e pressione um botão — o painel **HID · 02** mostra
   os inputs ao vivo.
5. Clique **iniciar streaming**. O Big Picture abre no host e o vídeo aparece
   em tela cheia no client.

## Estado do MVP

| Componente | Status |
|---|---|
| Painel host (config) | ✅ funcional |
| Client web (UI/UX) | ✅ funcional |
| Signaling WebSocket | ✅ funcional |
| WebRTC vídeo (Pion) | ✅ pipeline pronto |
| Captura+encode (FFmpeg) | ✅ implementado (precisa de GPU/Windows p/ rodar) |
| Gamepad → data channel | ✅ funcional |
| Injeção ViGEmBus | ⚙️ requer SDK ViGEmClient (ver `internal/input/vigem_windows.go`) |
| Áudio | ❌ fora do MVP (próximo passo) |
| Descoberta mDNS | ❌ v2 (hoje: IP manual) |

## Estrutura

```
cmd/host/main.go            entry point + embed das páginas web
cmd/host/web/               host.html, client.html, style.css (servidas embarcadas)
internal/config/            struct de configuração de streaming
internal/capture/           wrapper FFmpeg (ddagrab + h264_nvenc)
internal/webrtc/            sessão Pion: track de vídeo + data channel de input
internal/signaling/         servidor HTTP + WebSocket de signaling
internal/input/             GamepadState + injetor ViGEm (win) / noop (outros)
internal/steam/             dispara steam://open/bigpicture
```

## Notas técnicas

- **Annex-B → WebRTC:** o FFmpeg emite H.264 Annex-B; o `h264reader` do Pion
  fatia em NAL units enviadas como samples. GOP de 2s e `-bf 0` (sem B-frames)
  para minimizar latência.
- **Input não-confiável:** o data channel usa `ordered:false, maxRetransmits:0`
  (UDP-like) — perder um pacote de input é melhor que esperar retransmissão.
- **ViGEmBus:** `vigem_windows.go` espera o SDK ViGEmClient (header + lib) em
  `internal/input/vigem/`. Sem ele, compile o stub ajustando a build tag, ou
  adapte para chamar um helper externo.
- **Y invertido:** a Gamepad API usa +Y para baixo; o XInput espera o oposto —
  já tratado no injetor.

## Roadmap

1. Áudio do sistema (WASAPI loopback → Opus no WebRTC)
2. Descoberta mDNS (`_tether._tcp.local`) para listar hosts automaticamente
3. Captura/encode nativos (DXGI + NVENC via cgo) para cortar o overhead do FFmpeg
4. Teclado + mouse (jogos não-gamepad)
5. H.265/AV1 quando o browser do client suportar
