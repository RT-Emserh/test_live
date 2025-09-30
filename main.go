package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
)

type SignalingMessage struct {
	Type     string                     `json:"type"`
	From     string                     `json:"from"`
	To       string                     `json:"to"`
	RoomID   string                     `json:"room_id"`
	Offer    *webrtc.SessionDescription `json:"offer,omitempty"`
	Answer   *webrtc.SessionDescription `json:"answer,omitempty"`
	ICE      *webrtc.ICECandidateInit   `json:"ice,omitempty"`
	UserList []UserInfo                 `json:"user_list,omitempty"`
	UserInfo *UserInfo                  `json:"user_info,omitempty"`
	Message  string                     `json:"message,omitempty"`
	Data     map[string]interface{}     `json:"data,omitempty"`
}

type UserInfo struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	HasVideo    bool      `json:"has_video"`
	HasAudio    bool      `json:"has_audio"`
	IsStreaming bool      `json:"is_streaming"`
	JoinedAt    time.Time `json:"joined_at"`
}

type VideoClient struct {
	ID          string
	Name        string
	Conn        *websocket.Conn
	RoomID      string
	PeerConns   map[string]*webrtc.PeerConnection
	HasVideo    bool
	HasAudio    bool
	IsStreaming bool
	JoinedAt    time.Time
	mutex       sync.RWMutex
}

type VideoRoom struct {
	ID      string
	Clients map[string]*VideoClient
	Created time.Time
	mutex   sync.RWMutex
}

type VideoHub struct {
	rooms      map[string]*VideoRoom
	register   chan *VideoClient
	unregister chan *VideoClient
	signal     chan *SignalingMessage
	roomStats  chan string
	mutex      sync.RWMutex
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

func NewVideoHub() *VideoHub {
	return &VideoHub{
		rooms:      make(map[string]*VideoRoom),
		register:   make(chan *VideoClient),
		unregister: make(chan *VideoClient),
		signal:     make(chan *SignalingMessage),
		roomStats:  make(chan string),
	}
}

func (h *VideoHub) Run() {
	// Cleanup ticker para remover salas vazias
	cleanupTicker := time.NewTicker(5 * time.Minute)
	defer cleanupTicker.Stop()

	for {
		select {
		case client := <-h.register:
			h.handleClientRegister(client)

		case client := <-h.unregister:
			h.handleClientUnregister(client)

		case msg := <-h.signal:
			h.handleSignalingMessage(msg)

		case roomID := <-h.roomStats:
			h.sendRoomStats(roomID)

		case <-cleanupTicker.C:
			h.cleanupEmptyRooms()
		}
	}
}

func (h *VideoHub) handleClientRegister(client *VideoClient) {
	h.mutex.Lock()
	room, exists := h.rooms[client.RoomID]
	if !exists {
		room = &VideoRoom{
			ID:      client.RoomID,
			Clients: make(map[string]*VideoClient),
			Created: time.Now(),
		}
		h.rooms[client.RoomID] = room
		log.Printf("Nova sala criada: %s", client.RoomID)
	}
	h.mutex.Unlock()

	room.mutex.Lock()
	room.Clients[client.ID] = client
	room.mutex.Unlock()

	log.Printf("Cliente %s (%s) conectado na sala %s", client.ID, client.Name, client.RoomID)

	// Enviar lista de usuários para o novo cliente
	h.sendUserListToClient(client)

	// Notificar outros clientes sobre novo participante
	h.broadcastUserJoined(room, client)
}

func (h *VideoHub) handleClientUnregister(client *VideoClient) {
	h.mutex.RLock()
	room, exists := h.rooms[client.RoomID]
	h.mutex.RUnlock()

	if !exists {
		return
	}

	room.mutex.Lock()
	if _, ok := room.Clients[client.ID]; ok {
		delete(room.Clients, client.ID)

		// Fechar todas as conexões peer
		client.mutex.Lock()
		for peerID, pc := range client.PeerConns {
			log.Printf("Fechando conexão peer entre %s e %s", client.ID, peerID)
			pc.Close()
		}
		client.PeerConns = make(map[string]*webrtc.PeerConnection)
		client.mutex.Unlock()

		client.Conn.Close()

		// Notificar outros clientes sobre a saída
		for _, otherClient := range room.Clients {
			msg := &SignalingMessage{
				Type:   "user-left",
				From:   client.ID,
				RoomID: client.RoomID,
			}
			otherClient.Conn.WriteJSON(msg)

			// Remover conexões peer nos outros clientes
			otherClient.mutex.Lock()
			if pc, exists := otherClient.PeerConns[client.ID]; exists {
				pc.Close()
				delete(otherClient.PeerConns, client.ID)
			}
			otherClient.mutex.Unlock()
		}

		log.Printf("Cliente %s saiu da sala %s", client.ID, client.RoomID)
	}
	room.mutex.Unlock()
}

func (h *VideoHub) handleSignalingMessage(msg *SignalingMessage) {
	h.mutex.RLock()
	room, exists := h.rooms[msg.RoomID]
	h.mutex.RUnlock()

	if !exists {
		return
	}

	room.mutex.RLock()
	defer room.mutex.RUnlock()

	switch msg.Type {
	case "offer", "answer", "ice-candidate":
		if targetClient, ok := room.Clients[msg.To]; ok {
			err := targetClient.Conn.WriteJSON(msg)
			if err != nil {
				log.Printf("Erro ao enviar sinal para %s: %v", msg.To, err)
			}
		}

	case "media-state-changed":
		if sourceClient, ok := room.Clients[msg.From]; ok {
			if msg.Data != nil {
				if hasVideo, ok := msg.Data["hasVideo"].(bool); ok {
					sourceClient.HasVideo = hasVideo
				}
				if hasAudio, ok := msg.Data["hasAudio"].(bool); ok {
					sourceClient.HasAudio = hasAudio
				}
			}

			// Broadcast para outros clientes
			for _, client := range room.Clients {
				if client.ID != msg.From {
					updateMsg := &SignalingMessage{
						Type:   "user-media-changed",
						From:   msg.From,
						RoomID: msg.RoomID,
						Data: map[string]interface{}{
							"hasVideo": sourceClient.HasVideo,
							"hasAudio": sourceClient.HasAudio,
						},
					}
					client.Conn.WriteJSON(updateMsg)
				}
			}
		}

	case "chat-message":
		// Broadcast mensagem de chat
		for _, client := range room.Clients {
			if client.ID != msg.From {
				client.Conn.WriteJSON(msg)
			}
		}
	}
}

func (h *VideoHub) sendUserListToClient(client *VideoClient) {
	h.mutex.RLock()
	room := h.rooms[client.RoomID]
	h.mutex.RUnlock()

	room.mutex.RLock()
	defer room.mutex.RUnlock()

	var userList []UserInfo
	for _, otherClient := range room.Clients {
		if otherClient.ID != client.ID {
			userList = append(userList, UserInfo{
				ID:          otherClient.ID,
				Name:        otherClient.Name,
				HasVideo:    otherClient.HasVideo,
				HasAudio:    otherClient.HasAudio,
				IsStreaming: otherClient.IsStreaming,
				JoinedAt:    otherClient.JoinedAt,
			})
		}
	}

	msg := &SignalingMessage{
		Type:     "user-list",
		UserList: userList,
		RoomID:   client.RoomID,
	}
	client.Conn.WriteJSON(msg)
}

func (h *VideoHub) broadcastUserJoined(room *VideoRoom, newClient *VideoClient) {
	room.mutex.RLock()
	defer room.mutex.RUnlock()

	userInfo := UserInfo{
		ID:          newClient.ID,
		Name:        newClient.Name,
		HasVideo:    newClient.HasVideo,
		HasAudio:    newClient.HasAudio,
		IsStreaming: newClient.IsStreaming,
		JoinedAt:    newClient.JoinedAt,
	}

	for _, client := range room.Clients {
		if client.ID != newClient.ID {
			msg := &SignalingMessage{
				Type:     "user-joined",
				From:     newClient.ID,
				RoomID:   newClient.RoomID,
				UserInfo: &userInfo,
			}
			client.Conn.WriteJSON(msg)
		}
	}
}

func (h *VideoHub) sendRoomStats(roomID string) {
	h.mutex.RLock()
	room, exists := h.rooms[roomID]
	h.mutex.RUnlock()

	if !exists {
		return
	}

	room.mutex.RLock()
	defer room.mutex.RUnlock()

	stats := map[string]interface{}{
		"room_id":      roomID,
		"user_count":   len(room.Clients),
		"created_at":   room.Created,
		"active_since": time.Since(room.Created),
	}

	for _, client := range room.Clients {
		msg := &SignalingMessage{
			Type: "room-stats",
			Data: stats,
		}
		client.Conn.WriteJSON(msg)
	}
}

func (h *VideoHub) cleanupEmptyRooms() {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	for roomID, room := range h.rooms {
		room.mutex.RLock()
		isEmpty := len(room.Clients) == 0
		room.mutex.RUnlock()

		if isEmpty {
			delete(h.rooms, roomID)
			log.Printf("Sala vazia removida: %s", roomID)
		}
	}
}

func createPeerConnection() (*webrtc.PeerConnection, error) {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{
					"stun:stun.l.google.com:19302",
					"stun:stun1.l.google.com:19302",
					"stun:stun2.l.google.com:19302",
				},
			},
		},
		ICECandidatePoolSize: 10,
	}

	// Configurações para melhor qualidade de vídeo
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))
	return api.NewPeerConnection(config)
}

func handleVideoSignaling(hub *VideoHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Erro ao fazer upgrade da conexão: %v", err)
		return
	}

	userID := r.URL.Query().Get("user_id")
	userName := r.URL.Query().Get("user_name")
	roomID := r.URL.Query().Get("room")

	if userID == "" || roomID == "" {
		conn.WriteJSON(map[string]string{"error": "user_id e room são obrigatórios"})
		conn.Close()
		return
	}

	if userName == "" {
		userName = "Usuário " + userID[:8]
	}

	client := &VideoClient{
		ID:        userID,
		Name:      userName,
		Conn:      conn,
		RoomID:    roomID,
		PeerConns: make(map[string]*webrtc.PeerConnection),
		HasVideo:  false,
		HasAudio:  false,
		JoinedAt:  time.Now(),
	}

	// Configurar ping/pong para manter conexão
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	// Goroutine para ping periódico
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			}
		}
	}()

	hub.register <- client

	go func() {
		defer func() {
			hub.unregister <- client
		}()

		for {
			var msg SignalingMessage
			err := conn.ReadJSON(&msg)
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Printf("Erro ao ler mensagem de %s: %v", client.ID, err)
				}
				break
			}

			msg.From = client.ID
			msg.RoomID = client.RoomID

			// Processar mensagens WebRTC
			switch msg.Type {
			case "offer":
				// BUG FIX: NÃO processar localmente, apenas repassar
				log.Printf("Offer de %s para %s", msg.From, msg.To)
				hub.signal <- &msg
			case "answer":
				// BUG FIX: NÃO processar localmente, apenas repassar
				log.Printf("Answer de %s para %s", msg.From, msg.To)
				hub.signal <- &msg
			case "ice-candidate":
				// BUG FIX: NÃO processar localmente, apenas repassar
				log.Printf("ICE de %s para %s", msg.From, msg.To)
				hub.signal <- &msg
			case "media-state-changed", "chat-message":
				hub.signal <- &msg
			default:
				hub.signal <- &msg
			}
		}
	}()
}

func handleRoomInfo(hub *VideoHub, w http.ResponseWriter, r *http.Request) {
	roomID := r.URL.Query().Get("room")
	if roomID == "" {
		http.Error(w, "room parameter required", http.StatusBadRequest)
		return
	}

	hub.mutex.RLock()
	room, exists := hub.rooms[roomID]
	hub.mutex.RUnlock()

	if !exists {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"exists":     false,
			"user_count": 0,
		})
		return
	}

	room.mutex.RLock()
	userCount := len(room.Clients)
	var users []UserInfo
	for _, client := range room.Clients {
		users = append(users, UserInfo{
			ID:          client.ID,
			Name:        client.Name,
			HasVideo:    client.HasVideo,
			HasAudio:    client.HasAudio,
			IsStreaming: client.IsStreaming,
			JoinedAt:    client.JoinedAt,
		})
	}
	room.mutex.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"exists":     true,
		"room_id":    roomID,
		"user_count": userCount,
		"users":      users,
		"created_at": room.Created,
	})
}

func main() {
	hub := NewVideoHub()
	go hub.Run()

	// Rotas da API
	http.HandleFunc("/video-signaling", func(w http.ResponseWriter, r *http.Request) {
		handleVideoSignaling(hub, w, r)
	})

	http.HandleFunc("/room-info", func(w http.ResponseWriter, r *http.Request) {
		handleRoomInfo(hub, w, r)
	})

	// Servir arquivos estáticos
	http.Handle("/", http.FileServer(http.Dir("./static/")))

	log.Println("🚀 Servidor de videoconferência iniciado na porta 8084")
	log.Println("📹 Acesse: http://localhost:8084")
	log.Fatal(http.ListenAndServe(":8084", nil))
}
