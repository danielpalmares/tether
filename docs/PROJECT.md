# Tether — Documentação do Projeto

> Streaming de jogos em **baixa latência** na rede local (LAN). Host em Windows
> (captura + encode H.264 por NVENC ou libx264), client em navegador (foco: TV
> Samsung Tizen 6.5+ e celular). Transporte WebRTC. Estado atual: **v1 de teste**.

---

## 1. Arquitetura

```
┌───────────────────────────── HOST (Windows) ──────────────────────────────┐
│                                                                            │
│  ddagrab (DXGI) ──► H.264 encoder ─► Annex-B ─► pumpVideo ─► WebRTC track   │
│  WASAPI loopback ─► Opus (FFmpeg) ─────────► pumpAudio ──► WebRTC track     │
│                                                                            │
│  HTTP/WS signaling (porta 8787)  ·  painel host.html  ·  client.html (PWA) │
│  DataChannel "input" ◄── teclado/mouse/gamepad do client                   │
└────────────────────────────────────────────────────────────────────────────┘
```

### Componentes (Go)
| Pacote | Papel |
|---|---|
| `cmd/host` | entrypoint, servidor HTTP, embed dos assets web, headers de cache/PWA |
| `internal/config` | `StreamConfig` + **`Tuning()`** (algoritmo adaptativo de encoder) |
| `internal/capture` | FFmpeg ddagrab→NVENC/libx264; consome `TuningProfile` |
| `internal/webrtc` | `Session`, negociação SDP, `pumpVideo`/`pumpAudio`, pacer anti-jitter |
| `internal/audio` | captura WASAPI, re-pacing RTP de Opus |
| `internal/input` | injeção SendInput / ViGEm (gamepad virtual) |
| `internal/signaling` | WebSocket de offer/answer, GET/POST de config |
| `internal/steam` | abre Big Picture ao conectar |

### Client (`cmd/host/web/`)
- `client.html` — app do jogador (TV/celular): descoberta, controle, streaming, PWA.
- `host.html` — painel de configuração no PC host.
- `style.css`, `manifest.webmanifest`, `sw.js`, `icons/`.

---

## 2. Algoritmo de tuning adaptativo (`internal/config/tuning.go`)

**Princípio (inspirado no Steam Link):** os parâmetros do encoder e do
transporte são **derivados de `bitrate × resolução × fps`**, nunca constantes.
Fonte única de verdade: `StreamConfig.Tuning() TuningProfile`.

| Parâmetro | Regra | Porquê |
|---|---|---|
| **GOP** | `3s × fps` | GOP longo amortiza a frequência dos picos de IDR. Recupera perda via NACK/PLI. |
| **VBV** | ms de bitrate: 1080p=30, 2K=55, 4K=75 | janela temporal p/ o rate control achatar o IDR em vez de despejá-lo em lote. Frame maior → janela maior. |
| **surfaces** | por megapixels: 3 / 4 / 6 | fila interna do NVENC; 2 fixo estrangulava bitrate alto → rajada de saída. |
| **spatial-aq / temporal-aq** | ligados | redistribui bits dentro e entre frames → achata picos de tamanho. |
| **FrameQueueDepth** | `65ms × fps / 1000` (~3 a 60fps) | fila curta, absorve rajada breve do writer sem acumular latência. |
| **PacerMaxHold** | `frameDur × 50%/75%/100%` (1080p/2K/4K) | teto de espera do pacer; frame grande chega mais irregular, precisa de mais folga. |

### Pacer de vídeo (`internal/webrtc/session.go`, `writeVideoFrames`)
- **Regra de ouro:** o pacer só **apara frames adiantados** (até `PacerMaxHold`).
  NUNCA impõe cadência fixa.
- Frame atrasado ou em rajada (`backlog>0`) → envia imediato e realinha o relógio.
- Garante latência adicionada **< 1 frame**, sem o lag de backlog.

> ⚠️ **Não repetir erros já cometidos** (vide histórico abaixo): impor cadência
> fixa no pacer, ou fila grande (250ms), PIORARAM o engasgo. A fila real raramente
> passa de 2-4 frames.

---

## 3. Decisões críticas de compatibilidade (TV Samsung Tizen)

Documentadas em detalhe na memória do agente; resumo:

1. **Codec H264 único forçado** com `SetCodecPreferences` antes do `CreateAnswer`.
   Sem isso o Pion ecoa perfis 720p na answer e a TV configura o decoder errado → congela.
2. **`profile-level-id` dinâmico** por resolução (`42c02a`/`42c033`/`42c034`),
   com `profile-iop 0xc0` casando os constraint flags reais do SPS H.264 Baseline.
3. **Baseline profile** (sem CABAC/B-frames): denominador comum de decoders de TV.
4. **playout-delay extension** com `max=1` (não 0): a Tizen interpreta `max=0`
   como "sem limite" e infla o jitter buffer p/ ~160ms.
5. **`jitterBufferTarget` em ms** (não segundos) no client.
6. **Re-pacing do áudio** a 20ms no host: o muxer RTP do FFmpeg emite Opus em
   rajada (gap ~90ms) que arrasta o vídeo via lip-sync.
7. **`Cache-Control: no-store`** no HTML/CSS/JS: a Tizen cacheia agressivo e
   ignora fixes do front sem isso.

---

## 4. UX do client

- **Descoberta automática**: varre a sub-rede (`/api/info`), com banner de status
  visual em 3 estados — `procurando` (pulsa) → `✓ encontrado/vinculado` → `● CONECTADO`.
- **Acessibilidade por controle**: na tela de setup, D-pad/stick movem o foco
  entre campos/botões com auto-scroll; A/Start ativam. Realce `gp-focus`.
- **Fullscreen preservado no reconnect**: ao salvar config a stream reinicia sem
  sair do fullscreen (reentrar exigiria novo gesto do usuário).
- **Cursor auto-hide** durante o streaming; **pointer lock** para mouse relativo.

---

## 5. PWA (instalável)

- `manifest.webmanifest` — `display: fullscreen`, ícones 192/512/maskable, tema escuro.
- `sw.js` — app shell offline-first; **nunca** cacheia `/api/*` nem `/ws`.
- Ícones gerados em `icons/` (anel acid + elo ciano).
- `main.go` serve assets PWA com cache curto (300s) e MIME correto; resto fica `no-store`.
- Instalar: abrir `http://IP:8787/client.html` na TV/celular → "Adicionar à tela inicial".

> ⚠️ Service worker exige contexto seguro (https **ou** localhost). Em HTTP LAN,
> alguns firmwares de TV aceitam; outros não registram o SW (o app funciona, só
> não instala offline). Avaliar HTTPS local (cert auto-assinado) se for requisito.

---

## 6. Problemas conhecidos

| # | Sintoma | Status / hipótese |
|---|---|---|
| P1 | **2K/4K ainda sem leveza visível** | tuning ajustado não resolveu na percepção. Provável limite do decoder/painel da TV ou pico de `writeMax` na escrita RTP (não mais o encoder). |
| P2 | **Stats mostram 1080p em 4K** | o HUD exibe `inbound-rtp.frameWidth/Height` (real). Indica que a **TV decodifica/escala em 1080p** de fato, não bug do client. Confirmar capacidade de decode 4K do firmware. |
| P3 | `writeMax` pico isolado ~610ms em 4K | raro; provável renegociação/reattach de stream. |
| P4 | 4K 50Mbps trava | config extrema; aceito como limite atual. |

---

## 7. Próximos passos

### Curto prazo (pós-v1)
1. **Diagnóstico de resolução (P2)**: o client buscar `/api/config` e exibir no
   HUD **"alvo AxB · decode CxD"** — distingue "host envia 4K / TV escala 1080p"
   de "host envia 1080p". Confirma se o gargalo é o decoder da TV.
2. **Investigar `writeMax` (P1/P3)**: profilar `WriteSample` —
   packetização de keyframe grande? lock em `writePacketToBindings`? backpressure SRTP?
3. **mDNS / descoberta automática** real (hoje é varredura de sub-rede).

### Médio prazo
4. **Presets estilo Steam Link** no painel (Suave/Equilibrado/Rápido) como
   multiplicador sobre o `TuningProfile`.
5. **HEVC / AV1** se o decoder da TV suportar (melhor qualidade/bitrate).
6. **HTTPS local** (cert auto-assinado) p/ habilitar SW em todos os firmwares.
7. **Adaptação dinâmica de bitrate** por feedback de `packetsLost`/`nack` (REMB/TWCC).

### Longo prazo
8. Suporte a múltiplos clients / seleção de monitor melhorada.
9. Telemetria de latência fim-a-fim (glass-to-glass).

---

## 8. Build & Run

> **Instalação para usuário final e empacotamento de release:** ver
> [`docs/INSTALL.md`](INSTALL.md) — inclui o instalador one-line, distribuição
> do `.exe`, assinatura de código e passos para a landing page.

### Launcher automatizado (recomendado para o usuário final) — `launch.ps1`
Valida e instala as dependências do sistema, depois sobe o host:
```powershell
powershell -ExecutionPolicy Bypass -File .\launch.ps1
```
O que ele faz:
- **FFmpeg**: usa `winget` (Gyan.FFmpeg); se não houver, baixa o build de
  gyan.dev (.7z), extrai para `%LOCALAPPDATA%\Tether\ffmpeg` e adiciona ao PATH
  do usuário. (precisa de 7-Zip; instala via winget se faltar).
- **ViGEmBus**: se o driver não estiver presente, roda `libs/ViGEmBus_*.exe`
  com elevação UAC (gamepad virtual). Sem ele, o vídeo funciona; só o gamepad fica off.
- **GPU**: NVENC segue como padrão; sem NVIDIA, selecione
  **H.264 · Universal CPU (TESTE)** no painel.
- **Binário**: usa `tether-host.exe` se existir; senão compila (precisa de Go).
- Flags: `-SkipChecks` (pula validação), `-Reinstall` (força reinstalar FFmpeg).

> Distribuição: compilar o `tether-host.exe` uma vez e empacotar junto de
> `launch.ps1` + `libs/` (ViGEmBus). O usuário final só roda o launcher.

### Build manual (dev)
```powershell
go build -o tether-host.exe ./cmd/host   # compila o host
./tether-host.exe                        # sobe na porta 8787, abre o painel
go test ./...                            # testes (config + webrtc)
```

- Painel host: `http://localhost:8787/host.html`
- Client LAN: `http://<IP-do-host>:8787/client.html`
- Variáveis: `TETHER_VIDEO_NACK=0` desliga NACK; `TETHER_FFMPEG_PIPELINE=cpu|d3d11`.
- Codec no painel host: **H.264 · NVENC** ou **H.264 · Universal CPU (TESTE)**.

### Instalar o PWA no client
- Automático: navegadores que suportam disparam o prompt; o client também tem um
  **botão "instalar app na tela inicial"** (painel 01) para quando o navegador
  não avisa.
- iOS/Safari: o botão mostra a instrução manual (Compartilhar ▸ Adicionar à Tela de Início).
