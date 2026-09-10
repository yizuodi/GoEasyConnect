package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

const (
	webSocketTicketLifetime = 30 * time.Second
	maxWebSocketTickets     = 1024
)

type wsClient struct {
	connection *websocket.Conn
	send       chan wsOutbound
	done       chan struct{}
	closeOnce  sync.Once
}

type wsOutbound struct {
	payload    []byte
	closeAfter bool
}

type wsMessage struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	Data      string `json:"data"`
	Cols      uint16 `json:"cols"`
	Rows      uint16 `json:"rows"`
}

func (a *App) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if !a.consumeWebSocketTicket(r.URL.Query().Get("ticket")) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		a.logError("accept WebSocket", err)
		return
	}
	client := &wsClient{connection: connection, send: make(chan wsOutbound, 64), done: make(chan struct{})}
	a.wsMu.Lock()
	a.wsClients[client] = struct{}{}
	a.wsMu.Unlock()
	go client.writeLoop()
	defer func() {
		a.wsMu.Lock()
		delete(a.wsClients, client)
		a.wsMu.Unlock()
		client.close(websocket.StatusNormalClosure, "connection closed")
	}()

	var subscribed *TerminalSession
	defer func() {
		if subscribed != nil {
			subscribed.removeClient(client)
		}
	}()
	for {
		_, payload, err := connection.Read(r.Context())
		if err != nil {
			return
		}
		var message wsMessage
		if json.Unmarshal(payload, &message) != nil {
			continue
		}
		switch message.Type {
		case "subscribe":
			if subscribed != nil {
				subscribed.removeClient(client)
			}
			subscribed = a.sessions.get(message.SessionID)
			if subscribed != nil {
				subscribed.addClient(client)
			}
		case "unsubscribe":
			if subscribed != nil {
				subscribed.removeClient(client)
				subscribed = nil
			}
		case "input":
			if terminal := a.sessions.get(message.SessionID); terminal != nil {
				_, _ = terminal.Write([]byte(message.Data))
			}
		case "resize":
			if terminal := a.sessions.get(message.SessionID); terminal != nil {
				_ = terminal.Resize(message.Cols, message.Rows)
			}
		}
	}
}

func (a *App) handleWebSocketTicket(w http.ResponseWriter, _ *http.Request) {
	ticket := uuid.NewString()
	now := time.Now()
	a.wsTicketMu.Lock()
	for value, expiresAt := range a.wsTickets {
		if !expiresAt.After(now) {
			delete(a.wsTickets, value)
		}
	}
	if len(a.wsTickets) >= maxWebSocketTickets {
		a.wsTicketMu.Unlock()
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "Too many pending WebSocket connections"})
		return
	}
	a.wsTickets[ticket] = now.Add(webSocketTicketLifetime)
	a.wsTicketMu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"ticket": ticket})
}

func (a *App) consumeWebSocketTicket(ticket string) bool {
	if ticket == "" {
		return false
	}
	now := time.Now()
	a.wsTicketMu.Lock()
	expiresAt, exists := a.wsTickets[ticket]
	delete(a.wsTickets, ticket)
	a.wsTicketMu.Unlock()
	return exists && expiresAt.After(now)
}

func (c *wsClient) enqueue(payload []byte) {
	c.enqueueMessage(payload, false)
}

func (c *wsClient) enqueueFinal(payload []byte) {
	c.enqueueMessage(payload, true)
}

func (c *wsClient) enqueueMessage(payload []byte, closeAfter bool) {
	message := wsOutbound{payload: append([]byte(nil), payload...), closeAfter: closeAfter}
	select {
	case <-c.done:
		return
	case c.send <- message:
	default:
		c.close(websocket.StatusPolicyViolation, "client is too slow")
	}
}

func (c *wsClient) writeLoop() {
	for {
		select {
		case <-c.done:
			return
		case message := <-c.send:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := c.connection.Write(ctx, websocket.MessageText, message.payload)
			cancel()
			if err != nil {
				c.close(websocket.StatusInternalError, "write failed")
				return
			}
			if message.closeAfter {
				c.close(websocket.StatusNormalClosure, "session exited")
				return
			}
		}
	}
}

func (c *wsClient) close(_ websocket.StatusCode, _ string) {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.connection.CloseNow()
	})
}

func (a *App) closeWebSockets() {
	a.wsMu.Lock()
	clients := make([]*wsClient, 0, len(a.wsClients))
	for client := range a.wsClients {
		clients = append(clients, client)
	}
	a.wsMu.Unlock()
	for _, client := range clients {
		client.close(websocket.StatusGoingAway, "server shutting down")
	}
}
