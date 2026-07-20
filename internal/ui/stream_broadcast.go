package ui

import (
	"sync"
	"time"
)

const streamSSEReliableSendTimeout = 2 * time.Second

func (s *Server) broadcastChatSSE(streamID, frame string) {
	broadcastStreamSSE(&s.chatSSEClients, streamID, frame)
}

func (s *Server) broadcastTaskSSE(streamID, frame string) {
	broadcastStreamSSE(&s.taskSSEClients, streamID, frame)
}

// broadcastStreamSSE sends an SSE frame to matching stream subscribers. The
// frame must be complete (event + data lines with a trailing blank line).
// Delivery is bounded so slow clients cannot stall a stream forever, but frames
// are not silently dropped under ordinary backpressure.
func broadcastStreamSSE(clients *sync.Map, streamID, frame string) {
	clients.Range(func(key, value any) bool {
		ch, ok := key.(chan string)
		if !ok {
			return true
		}
		clientStreamID, _ := value.(string)
		if streamID != "" && clientStreamID != streamID {
			return true
		}
		select {
		case ch <- frame:
		case <-time.After(streamSSEReliableSendTimeout):
			clients.Delete(ch)
		}
		return true
	})
}
