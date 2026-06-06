# Tether — Instalação & Distribuição

Guia de instalação para o **usuário final** e de empacotamento para o **mantenedor**.

---

## Para o usuário (instalar o host no PC com o jogo)

> Requisitos: **Windows 10/11**, GPU **NVIDIA** (NVENC) recomendada, rede local
> compartilhada com a TV/celular.

### Opção A — Instalador automático (recomendado)
Abra o **PowerShell** e cole:

```powershell
irm https://raw.githubusercontent.com/danielpalmares/tether/main/install.ps1 | iex
```

O instalador baixa o app do GitHub, instala o **FFmpeg** e o driver de gamepad
**ViGEmBus**, e cria atalhos no Menu Iniciar e na Área de Trabalho. Ao final,
abra **Tether** — o painel sobe em `http://localhost:8787`.

### Opção B — Baixar o .exe manualmente
1. Vá em **Releases**: `https://github.com/danielpalmares/tether/releases`
2. Baixe `tether-host-windows.zip` e extraia.
3. Rode `launch.ps1` (instala dependências e sobe o host) **ou** `tether-host.exe`
   direto se o FFmpeg já estiver no PATH.

> O Windows SmartScreen pode avisar que o app é de "editor desconhecido" — é
> esperado para executáveis sem assinatura digital. Clique em **Mais informações
> ▸ Executar assim mesmo**. (Ver "Assinatura de código" abaixo.)

---

## Como conectar a TV / celular

1. No PC host, anote o IP exibido no painel (ex.: `192.168.0.10:8787`).
2. No navegador da TV/celular, abra `http://<IP-do-host>:8787/client.html`.
3. O client descobre o host automaticamente; toque em **iniciar streaming**.

---

## Para o mantenedor (publicar um release)

O instalador espera, em cada release do GitHub, **um destes**:
- `tether-host-windows.zip` contendo `tether-host.exe`, `launch.ps1` e `libs/`
  (com `ViGEmBus_*.exe`) — **preferido**; ou
- `tether-host.exe` solto (sem ViGEmBus embutido).

### Passos para empacotar e publicar
```powershell
# 1. compila o host (Windows, com input de gamepad se o SDK ViGEm estiver presente)
go build -o tether-host.exe ./cmd/host

# 2. monta o pacote de distribuição
$pkg = "dist\tether-host-windows"
New-Item -ItemType Directory -Force $pkg | Out-Null
Copy-Item tether-host.exe, launch.ps1 $pkg
Copy-Item -Recurse libs $pkg
Compress-Archive -Path "$pkg\*" -DestinationPath "dist\tether-host-windows.zip" -Force

# 3. publica o release (precisa do GitHub CLI autenticado: gh auth login)
gh release create v1.0.0 dist\tether-host-windows.zip `
  --title "Tether v1.0.0" `
  --notes "Primeira versão de teste. Veja docs/PROJECT.md."
```

> O `tether_probe.h264` e o próprio `tether-host.exe` versionado no repo são
> artefatos de teste; o release oficial usa o zip de `dist/`.

---

## Pode distribuir o .exe direto? (resumo)

**Sim.** Um `.exe` Go é estático e roda sem runtime. Três níveis:

| Forma | Esforço | Experiência do usuário |
|---|---|---|
| **.exe solto** | baixo | precisa ter FFmpeg no PATH; sem gamepad sem ViGEmBus |
| **.zip (exe + launch.ps1 + libs)** | médio | `launch.ps1` instala deps e sobe; **recomendado** |
| **Instalador one-line (`install.ps1`)** | já pronto | um comando baixa do GitHub e instala tudo + atalhos |

### Limitações e próximos passos para "produto"
- **Assinatura de código**: sem um certificado de code signing, o SmartScreen
  alerta. Para uma landing page de produto, vale comprar um certificado (OV/EV)
  e assinar o `.exe` (`signtool`). Remove o aviso e passa confiança.
- **Instalador gráfico (.exe/.msi)**: para um clique duplo "instalar", empacotar
  com **Inno Setup** ou **WiX** (gera um instalador clássico com UI, em vez do
  one-liner PowerShell). É o próximo passo natural para a landing page.
- **HTTPS local**: necessário para o PWA instalar de verdade na TV/celular
  (cert auto-assinado) — ver `docs/PROJECT.md` §7.
- **Auto-update**: o `install.ps1` já busca o "latest"; dá para evoluir para um
  checador de versão dentro do app.
