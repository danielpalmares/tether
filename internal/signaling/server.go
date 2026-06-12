package signaling

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	pionwebrtc "github.com/pion/webrtc/v4"

	"tether/internal/config"
	"tether/internal/input"
	"tether/internal/sysinfo"
	"tether/internal/webrtc"
)

var upgrader = websocket.Upgrader{
	// LAN: aceita qualquer origem.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Server mantém o estado do host e a config corrente.
type Server struct {
	mu      sync.Mutex
	cfg     config.StreamConfig
	cfgPath string
	active  *webrtc.Session
	hostNm  string

	// netInfo é o adaptador de rede do host, detectado uma vez no boot (lado do
	// enlace que o client não enxerga no teste de capacidade da LAN).
	netInfo        atomic.Pointer[sysinfo.AdapterInfo]
	throughputBusy atomic.Bool
}

func NewServer(hostName string) *Server {
	path := config.Path()
	cfg, err := config.Load(path)
	if err != nil {
		// arquivo ausente/inválido no primeiro boot: usa o default e tenta
		// persistir já, para a próxima execução carregar do disco.
		cfg = config.Default()
		if saveErr := config.Save(path, cfg); saveErr != nil {
			log.Printf("[config] não foi possível criar %s: %v", path, saveErr)
		} else {
			log.Printf("[config] criado %s com perfil padrão", path)
		}
	} else {
		log.Printf("[config] carregada de %s: %dx%d@%dfps %dkbps", path, cfg.Width, cfg.Height, cfg.FPS, cfg.Bitrate)
	}
	s := &Server{cfg: cfg, cfgPath: path, hostNm: hostName}

	// Detecção do adaptador em background: custa ~1s de PowerShell e não pode
	// atrasar o boot do host.
	go func() {
		info, err := sysinfo.DetectAdapter(context.Background())
		if err != nil {
			log.Printf("[sysinfo] adaptador de rede indisponível: %v", err)
			return
		}
		s.netInfo.Store(&info)
		log.Printf("[sysinfo] rede do host: %s (%s, %d Mbps)", info.Name, info.Type, info.LinkMbps)
	}()

	return s
}

type wsMsg struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// HandleSignal é o endpoint WebSocket de signaling do client.
func (s *Server) HandleSignal(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var msg wsMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "offer":
			log.Println("[signal] offer recebida do client")
			var offer pionwebrtc.SessionDescription
			if err := json.Unmarshal(msg.Data, &offer); err != nil {
				log.Printf("[signal] erro ao ler offer: %v", err)
				continue
			}
			s.handleOffer(conn, offer)
		}
	}
}

func (s *Server) handleOffer(conn *websocket.Conn, offer pionwebrtc.SessionDescription) {
	s.mu.Lock()
	old := s.active
	s.active = nil
	cfg := s.cfg
	s.mu.Unlock()
	if old != nil {
		old.Close()
	}

	injector, err := input.NewInjector()
	if err != nil {
		log.Printf("[signal] injector: %v", err)
		return
	}

	var sess *webrtc.Session
	sess, err = webrtc.NewSession(cfg, injector, func() {
		_ = injector.Close()
		s.clearActive(sess)
	}, s.persistConfig)
	if err != nil {
		log.Printf("[signal] nova sessão: %v", err)
		return
	}

	s.mu.Lock()
	s.active = sess
	s.mu.Unlock()

	answer, err := sess.HandleOffer(offer)
	if err != nil {
		log.Printf("[signal] handle offer: %v", err)
		sess.Close()
		return
	}

	data, _ := json.Marshal(answer)
	_ = conn.WriteJSON(wsMsg{Type: "answer", Data: data})
}

func (s *Server) clearActive(sess *webrtc.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == sess {
		s.active = nil
	}
}

// persistConfig é o callback injetado na sessão: propaga ao estado do server e
// ao disco uma config aplicada com sucesso via hot-swap (pedido do client).
func (s *Server) persistConfig(c config.StreamConfig) {
	s.mu.Lock()
	s.cfg = c
	path := s.cfgPath
	s.mu.Unlock()
	if err := config.Save(path, c); err != nil {
		log.Printf("[config] falha ao salvar em %s: %v", path, err)
	} else {
		log.Printf("[config] persistida: %dx%d@%dfps %dkbps", c.Width, c.Height, c.FPS, c.Bitrate)
	}
}

// HandleConfig expõe GET/POST da config de streaming (usado pelo painel host).
func (s *Server) HandleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		defer s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(s.cfg)
	case http.MethodPost:
		var c config.StreamConfig
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c = c.Normalize()
		s.mu.Lock()
		active := s.active
		s.cfg = c
		path := s.cfgPath
		s.mu.Unlock()

		// persiste no disco para sobreviver a reinícios/rebuilds do host
		if err := config.Save(path, c); err != nil {
			log.Printf("[config] falha ao salvar em %s: %v", path, err)
		}

		switch {
		case active == nil:
			log.Printf("[config] alterada para %dx%d@%dfps %dkbps", c.Width, c.Height, c.FPS, c.Bitrate)
		default:
			// Tenta o hot-swap do encoder (sem derrubar a sessão WebRTC); só
			// reinicia a live quando a mudança exige renegociação (ex.: codec).
			if err := active.Reconfigure(c); err != nil {
				log.Printf("[config] hot-swap indisponível (%v); reiniciando live ativa", err)
				s.mu.Lock()
				if s.active == active {
					s.active = nil
				}
				s.mu.Unlock()
				active.Close()
			} else {
				log.Printf("[config] hot-swap aplicado: %dx%d@%dfps %dkbps", c.Width, c.Height, c.FPS, c.Bitrate)
			}
		}
		_ = json.NewEncoder(w).Encode(c)
	default:
		http.Error(w, "método não permitido", http.StatusMethodNotAllowed)
	}
}

// HandleInfo devolve metadados do host (nome, status) para descoberta do client.
func (s *Server) HandleInfo(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	streaming := s.active != nil
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	payload := map[string]any{
		"app":       "Tether",
		"host":      s.hostNm,
		"streaming": streaming,
	}
	if info := s.netInfo.Load(); info != nil {
		payload["network"] = info
	}
	_ = json.NewEncoder(w).Encode(payload)
}

// HandleThroughput envia bytes aleatórios por N segundos para o client medir a
// capacidade de transferência host->client da LAN — NÃO é teste de internet; o
// gargalo medido é o enlace local (tipicamente o Wi-Fi da TV). TCP subestima um
// pouco o teto UDP do RTP; o client compensa na heurística de sugestão.
func (s *Server) HandleThroughput(w http.ResponseWriter, r *http.Request) {
	const (
		defaultSeconds = 5
		maxSeconds     = 10
		chunkSize      = 256 * 1024
	)
	seconds := defaultSeconds
	if v := r.URL.Query().Get("seconds"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= maxSeconds {
			seconds = n
		}
	}

	// Um teste por vez: medições concorrentes dividem o enlace e mentem.
	if !s.throughputBusy.CompareAndSwap(false, true) {
		http.Error(w, "teste de capacidade já em andamento", http.StatusConflict)
		return
	}
	defer s.throughputBusy.Store(false)

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Bloco aleatório (incompressível) reusado: o custo de CPU fica no rand uma
	// única vez; o loop só empurra bytes até o deadline.
	chunk := make([]byte, chunkSize)
	if _, err := rand.Read(chunk); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	for time.Now().Before(deadline) {
		if _, err := w.Write(chunk); err != nil {
			return // client abortou o teste
		}
	}
}
