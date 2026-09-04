package signaling

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	pionwebrtc "github.com/pion/webrtc/v4"

	"tether/internal/config"
	"tether/internal/input"
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
	lanAddr string
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
	return &Server{cfg: cfg, cfgPath: path, hostNm: hostName}
}

// SetLANAddress informa o IP pelo qual a TV alcança este host, para o painel
// exibir o endereço certo (ver localIP no main: a rota padrão pode ser a VPN).
func (s *Server) SetLANAddress(ip string) {
	s.mu.Lock()
	s.lanAddr = ip
	s.mu.Unlock()
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
	})
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
		s.active = nil
		s.cfg = c
		path := s.cfgPath
		s.mu.Unlock()

		// persiste no disco para sobreviver a reinícios/rebuilds do host
		if err := config.Save(path, c); err != nil {
			log.Printf("[config] falha ao salvar em %s: %v", path, err)
		}

		if active != nil {
			log.Printf("[config] alterada para %dx%d@%dfps %dkbps; reiniciando live ativa", c.Width, c.Height, c.FPS, c.Bitrate)
			active.Close()
		} else {
			log.Printf("[config] alterada para %dx%d@%dfps %dkbps", c.Width, c.Height, c.FPS, c.Bitrate)
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
	lan := s.lanAddr
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"app":       "Tether",
		"host":      s.hostNm,
		"streaming": streaming,
		// lan é o endereço que a TV deve usar; o painel do PC roda em localhost
		// e não tem como descobrir isso sozinho.
		"lan": lan,
	})
}
