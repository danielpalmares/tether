package main

import (
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"tether/internal/process"
	"tether/internal/signaling"
)

//go:embed web/*
var webFiles embed.FS

const port = "8787"

func main() {
	if err := process.BoostCurrent(); err != nil {
		log.Printf("[host] aviso: não foi possível elevar prioridade: %v", err)
	} else {
		log.Println("[host] prioridade alta ativa")
	}

	hostName, _ := os.Hostname()
	srv := signaling.NewServer(hostName)

	webFS, err := fs.Sub(webFiles, "web")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	// no-cache nos assets estáticos: a TV Samsung Tizen cacheia HTML/JS de forma
	// agressiva. Sem isto, mudanças no client.html (jitter buffer, playout-delay)
	// não chegam ao firmware — ele segue rodando a versão antiga em cache, o que
	// mascara qualquer correção de latência aplicada no front. EXCEÇÃO: os assets
	// PWA (manifest, service worker, ícones) precisam de um cache curto — não
	// no-store — senão o navegador não consegue instalar/registrar o app.
	mux.Handle("/", cacheControl(http.FileServer(http.FS(webFS))))
	mux.HandleFunc("/api/info", srv.HandleInfo)
	mux.HandleFunc("/api/config", srv.HandleConfig)
	// Painel único (no cliente): opções, saúde do host e teste de banda da LAN.
	mux.HandleFunc("/api/options", srv.HandleOptions)
	mux.HandleFunc("/api/stats", srv.HandleStats)
	mux.HandleFunc("/api/bandwidth", srv.HandleBandwidth)
	mux.HandleFunc("/api/bandwidth/up", srv.HandleBandwidthUp)
	mux.HandleFunc("/api/recommend", srv.HandleRecommend)
	mux.HandleFunc("/ws", srv.HandleSignal)

	ip := localIP()
	srv.SetLANAddress(ip)
	fmt.Println("╔══════════════════════════════════════════╗")
	fmt.Println("║              TETHER  ·  host               ║")
	fmt.Println("╠══════════════════════════════════════════╣")
	fmt.Printf("║  Painel host : http://localhost:%s        \n", port)
	fmt.Printf("║  Client LAN  : http://%s:%s/client.html\n", ip, port)
	fmt.Println("╚══════════════════════════════════════════╝")

	go openBrowser("http://localhost:" + port + "/host.html")

	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}

// cacheControl aplica no-store ao HTML/CSS/JS (para que correções cheguem à TV
// sem cache preso) mas um cache curto aos assets PWA, necessário para o app
// instalar/registrar o service worker.
func cacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPWAAsset(r.URL.Path) {
			// cache curto: permite instalação, mas atualiza rápido em LAN.
			w.Header().Set("Cache-Control", "public, max-age=300")
			if strings.HasSuffix(r.URL.Path, ".js") {
				w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
			} else if strings.HasSuffix(r.URL.Path, ".webmanifest") {
				w.Header().Set("Content-Type", "application/manifest+json; charset=utf-8")
			}
		} else {
			w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Expires", "0")
		}
		next.ServeHTTP(w, r)
	})
}

func isPWAAsset(path string) bool {
	return path == "/sw.js" ||
		path == "/manifest.webmanifest" ||
		strings.HasPrefix(path, "/icons/")
}

// localIP descobre o endereço que a TV deve usar para alcançar o host.
//
// A abordagem ingênua — abrir um socket para 8.8.8.8 e ler o endereço local —
// devolve o IP da rota padrão, que numa máquina com VPN ativa é o IP da VPN
// (medido: 10.5.0.2 do NordLynx). A TV está na LAN e não alcança esse endereço,
// então o painel anunciava um endereço inútil. Aqui varremos as interfaces e
// escolhemos um IP de rede privada em interface física ativa, ignorando
// túneis/VPN e adaptadores virtuais.
func localIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return fallbackLocalIP()
	}

	var best string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if isVirtualInterface(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil || !ip.IsPrivate() || ip.IsLinkLocalUnicast() {
				continue
			}
			// Preferimos 192.168.x.x — a faixa doméstica típica, onde a TV está.
			if strings.HasPrefix(ip.String(), "192.168.") {
				return ip.String()
			}
			if best == "" {
				best = ip.String()
			}
		}
	}
	if best != "" {
		return best
	}
	return fallbackLocalIP()
}

// isVirtualInterface filtra adaptadores que não levam à LAN: VPNs, túneis,
// hypervisors e pontes de contêiner.
func isVirtualInterface(name string) bool {
	n := strings.ToLower(name)
	for _, bad := range []string{
		"vpn", "nordlynx", "wireguard", "tailscale", "zerotier", "tap", "tun",
		"virtualbox", "vmware", "hyper-v", "vethernet", "docker", "wsl", "loopback",
	} {
		if strings.Contains(n, bad) {
			return true
		}
	}
	return false
}

func fallbackLocalIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

func openBrowser(url string) {
	time.Sleep(400 * time.Millisecond)
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
