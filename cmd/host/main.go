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
	"time"

	"tether/internal/signaling"
)

//go:embed web/*
var webFiles embed.FS

const port = "8787"

func main() {
	hostName, _ := os.Hostname()
	srv := signaling.NewServer(hostName)

	webFS, err := fs.Sub(webFiles, "web")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(webFS)))
	mux.HandleFunc("/api/info", srv.HandleInfo)
	mux.HandleFunc("/api/config", srv.HandleConfig)
	mux.HandleFunc("/ws", srv.HandleSignal)

	ip := localIP()
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

func localIP() string {
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
