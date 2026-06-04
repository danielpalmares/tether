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
	active  *webrtc.Session
	hostNm  string
}

func NewServer(hostName string) *Server {
	return &Server{cfg: config.Default(), hostNm: hostName}
}

type wsMsg struct {
	Type  string          `json:"type"`
	Data  json.RawMessage `json:"data,omitempty"`
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
	if s.active != nil {
		s.active.Close()
		s.active = nil
	}
	cfg := s.cfg
	s.mu.Unlock()

	injector, err := input.NewInjector()
	if err != nil {
		log.Printf("[signal] injector: %v", err)
		return
	}

	sess, err := webrtc.NewSession(cfg, injector, func() {
		_ = injector.Close()
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

// HandleConfig expõe GET/POST da config de streaming (usado pelo painel host).
func (s *Server) HandleConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		_ = json.NewEncoder(w).Encode(s.cfg)
	case http.MethodPost:
		var c config.StreamConfig
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.cfg = c
		_ = json.NewEncoder(w).Encode(s.cfg)
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
	_ = json.NewEncoder(w).Encode(map[string]any{
		"app":       "Tether",
		"host":      s.hostNm,
		"streaming": streaming,
	})
}
