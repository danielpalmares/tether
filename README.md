# Tether · Transmissão Steam para navegador (LAN)

[![Site](https://img.shields.io/badge/site-tether-b6ff3a)](https://tether-lan.netlify.app)
[![Release](https://img.shields.io/github/v/release/danielpalmares/tether?color=2ad4ff)](https://github.com/danielpalmares/tether/releases/latest)
[![License: AGPL v3](https://img.shields.io/badge/license-AGPL--3.0-blue.svg)](LICENSE)
![Platform](https://img.shields.io/badge/host-Windows%2010%2F11-informational)

Streaming de jogos pela rede local com **app host nativo** (Go) e **client 100% no navegador** (WebRTC + Gamepad API). O host abre o **Steam Big Picture** automaticamente quando um client conecta, transmite a tela com baixa latência e recebe o input do controle Xbox de volta.

> **O seu PC na TV da sala. Um clique e jogou.** Sem nuvem, sem mensalidade.
> Conheça o projeto em **[tether-lan.netlify.app](https://tether-lan.netlify.app)**.

## Instalação rápida (host)

Abra o **PowerShell** e cole — baixa o app, instala FFmpeg + ViGEmBus e cria os atalhos:

```powershell
irm https://raw.githubusercontent.com/danielpalmares/tether/main/install.ps1 | iex
```

Depois abra o endereço LAN exibido (ex.: `http://192.168.0.10:8787/client.html`) no
navegador da TV/celular e aperte **iniciar streaming**. Detalhes em
[docs/INSTALL.md](docs/INSTALL.md).

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
| Injeção Xbox 360 virtual | ✅ Windows sem cgo quando `ViGEmClient.dll` está disponível |
| Teclado/mouse | ✅ fallback Windows sem cgo via SendInput |
| Áudio | ✅ Opus/WebRTC via WASAPI loopback; DirectShow como fallback |
| Descoberta mDNS | ❌ v2 (hoje: IP manual) |

## Estrutura

```
cmd/host/main.go            entry point + embed das páginas web
cmd/host/web/               host.html, client.html, style.css (servidas embarcadas)
internal/config/            struct de configuração de streaming
internal/capture/           wrapper FFmpeg (ddagrab + h264_nvenc)
internal/webrtc/            sessão Pion: track de vídeo + data channel de input
internal/signaling/         servidor HTTP + WebSocket de signaling
internal/input/             GamepadState + injetor ViGEm/SendInput (win) / noop
internal/audio/             captura WASAPI/DirectShow -> Opus/RTP -> WebRTC
internal/steam/             dispara steam://open/bigpicture
```

## Notas técnicas

- **Annex-B → WebRTC:** o FFmpeg emite H.264 Annex-B; o `h264reader` do Pion
  fatia em NAL units enviadas como samples. GOP de 2s e `-bf 0` (sem B-frames)
  para minimizar latência.
- **Input não-confiável:** o data channel usa `ordered:false, maxRetransmits:0`
  (UDP-like) — perder um pacote de input é melhor que esperar retransmissão.
- **Input:** no build padrão (`CGO_ENABLED=0`), o host procura
  `ViGEmClient.dll` ao lado do executável, em `bin/`, em
  `internal/input/vigem/bin/`, ou no caminho definido por `TETHER_VIGEM_DLL`.
  Com a DLL presente, o gamepad do client vira um Xbox 360 virtual real via
  ViGEmBus. Sem a DLL, o host cai para SendInput e registra isso no log; esse
  fallback só serve para navegação/teclado, não para jogos que exigem XInput.
- **Áudio:** tenta WASAPI loopback nativo primeiro, capturando o dispositivo de
  reprodução padrão do Windows e enviando PCM para o FFmpeg codificar Opus/RTP.
  Para forçar DirectShow, defina `TETHER_AUDIO_BACKEND=dshow`; se a fonte não
  aparecer automaticamente, defina `TETHER_AUDIO_DEVICE` com o nome do
  dispositivo DirectShow.
- **Y invertido:** a Gamepad API usa +Y para baixo; o XInput espera o oposto —
  já tratado no injetor.

## Roadmap

1. Fluxo explícito de setup para baixar/posicionar `ViGEmClient.dll`
2. Descoberta mDNS (`_tether._tcp.local`) para listar hosts automaticamente
3. Captura/encode nativos (DXGI + NVENC via cgo) para cortar o overhead do FFmpeg
4. H.265/AV1 quando o browser do client suportar

## Licença

[GNU AGPL-3.0](LICENSE). Software livre e de código aberto: você pode usar,
estudar, modificar e redistribuir. Em contrapartida, qualquer trabalho derivado
— inclusive quando oferecido pela rede como serviço — **deve permanecer aberto
sob a mesma licença AGPL-3.0**. Não é permitido fechar o código nem revender
como produto proprietário.
