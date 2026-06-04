#!/usr/bin/env bash
# Build offline (usa vendor/).
#
# Por padrão CGO_ENABLED=0 -> compila com o stub de input (noop_other.go):
# o VÍDEO funciona normalmente, mas a injeção de gamepad fica desativada.
#
# Para o build COM input real de controle (ViGEm), são necessários DOIS itens:
#   1. CGO_ENABLED=1 e um toolchain C (ex.: gcc do MSYS2/mingw-w64 no Windows).
#   2. O SDK do ViGEmClient presente em internal/input/vigem/:
#        internal/input/vigem/include/ViGEm/Client.h
#        internal/input/vigem/lib/ViGEmClient.lib   (ou .a)
#      e o driver ViGEmBus instalado: https://github.com/ViGEm/ViGEmBus/releases
#
# Build SOMENTE-VÍDEO (multiplataforma):
#   GOOS=windows GOARCH=amd64 go build -mod=vendor -o tether-host.exe ./cmd/host
#
# Build COM input (no Windows, com o SDK ViGEm em internal/input/vigem/):
#   CGO_ENABLED=1 go build -mod=vendor -o tether-host.exe ./cmd/host
set -e
go build -mod=vendor -o tether-host ./cmd/host
echo "OK -> ./tether-host (input desativado se CGO_ENABLED=0)"
