package demo

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/example-git/crux/internal/ui/model"
	"github.com/google/uuid"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type controlClient struct {
	events chan []byte
	done   chan struct{}
}
type controlReply struct {
	Result   map[string]any `json:"result"`
	Error    string         `json:"error"`
	ID       string         `json:"id"`
	ClientID string         `json:"clientId"`
}
type pendingControl struct {
	client string
	reply  chan controlReply
}

func registerControl(mux *http.ServeMux, render func(model.PreviewOptions) (model.PreviewFrame, error)) {
	var headless headlessRenderer
	var mu sync.Mutex
	clients := map[string]*controlClient{}
	pending := map[string]pendingControl{}
	images := map[string][]byte{}
	imageOrder := []string{}
	send := func(w http.ResponseWriter, code int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(value)
	}
	validOrigin := func(w http.ResponseWriter, r *http.Request) bool {
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host {
				send(w, 403, map[string]string{"error": "Cross-origin control is not allowed"})
				return false
			}
		}
		return true
	}
	mux.HandleFunc("GET /api/control/events", func(w http.ResponseWriter, r *http.Request) {
		if !validOrigin(w, r) {
			return
		}
		flush, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming unavailable", 500)
			return
		}
		id := uuid.NewString()
		client := &controlClient{events: make(chan []byte, 16), done: make(chan struct{})}
		mu.Lock()
		clients[id] = client
		mu.Unlock()
		defer func() { mu.Lock(); delete(clients, id); close(client.done); mu.Unlock() }()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintf(w, "data: {\"clientId\":%q}\n\n", id)
		flush.Flush()
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case event := <-client.events:
				fmt.Fprintf(w, "data: %s\n\n", event)
				flush.Flush()
			case <-ticker.C:
				fmt.Fprint(w, ": connected\n\n")
				flush.Flush()
			}
		}
	})
	mux.HandleFunc("GET /api/control", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ids := []string{}
		for id := range clients {
			ids = append(ids, id)
		}
		mu.Unlock()
		send(w, 200, map[string]any{"clients": ids, "actions": []string{"inspect", "catalog", "data", "patch", "set", "click", "find", "screenshot", "reset"}})
	})
	mux.HandleFunc("POST /api/control", func(w http.ResponseWriter, r *http.Request) {
		if !validOrigin(w, r) {
			return
		}
		var command map[string]any
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&command); err != nil {
			send(w, 400, map[string]string{"error": err.Error()})
			return
		}
		mu.Lock()
		clientID, _ := command["clientId"].(string)
		if clientID == "" && len(clients) == 1 {
			for id := range clients {
				clientID = id
			}
		}
		if clientID == "" && len(clients) == 0 && (command["action"] == "screenshot" || command["action"] == "set" && command["screenshot"] == true) {
			mu.Unlock()
			started := time.Now()
			for key := range command {
				if key != "action" && key != "state" && key != "screenshot" && key != "clientId" {
					send(w, 400, map[string]string{"error": "Unsupported clientless capture field: " + key})
					return
				}
			}
			o := defaultCaptureOptions()
			if state, exists := command["state"]; exists {
				if _, ok := state.(map[string]any); !ok {
					send(w, 400, map[string]string{"error": "state must be an object"})
					return
				}
				data, err := json.Marshal(state)
				if err != nil {
					send(w, 400, map[string]string{"error": err.Error()})
					return
				}
				decoder := json.NewDecoder(bytes.NewReader(data))
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(&o); err != nil {
					send(w, 400, map[string]string{"error": err.Error()})
					return
				}
			}
			if o.Cols < 45 || o.Cols > 500 || o.Rows < 15 || o.Rows > 200 {
				send(w, 400, map[string]string{"error": "Capture dimensions require cols 45–500 and rows 15–200"})
				return
			}
			frame, err := render(o)
			if err != nil {
				send(w, 400, map[string]string{"error": err.Error()})
				return
			}
			png, lines, err := headless.capture(frame)
			if err != nil {
				send(w, 500, map[string]string{"error": err.Error()})
				return
			}
			id := uuid.NewString()
			path := "/api/control/frames/" + id + ".png"
			mu.Lock()
			images[path] = png
			imageOrder = append(imageOrder, path)
			if len(imageOrder) > 12 {
				delete(images, imageOrder[0])
				imageOrder = imageOrder[1:]
			}
			mu.Unlock()
			send(w, 200, map[string]any{"commandId": id, "revision": id, "clientId": "", "renderer": frame.Renderer, "captureRenderer": "goja-xterm-svg-resvg", "state": o, "lines": lines, "regions": frame.Regions, "items": frame.Items, "elapsedMs": float64(time.Since(started).Microseconds()) / 1000, "screenshot": map[string]any{"url": path, "width": frame.Cols * 8, "height": frame.Rows * 16, "font": headless.font, "fontSize": 13, "cellWidth": 8, "cellHeight": 16}})
			return
		}
		client := clients[clientID]
		if client == nil {
			ids := []string{}
			for id := range clients {
				ids = append(ids, id)
			}
			mu.Unlock()
			send(w, 409, map[string]any{"error": "Select one connected preview client; open the demo page if none are connected", "clients": ids})
			return
		}
		id := uuid.NewString()
		reply := make(chan controlReply, 1)
		pending[id] = pendingControl{clientID, reply}
		mu.Unlock()
		defer func() { mu.Lock(); delete(pending, id); mu.Unlock() }()
		delete(command, "clientId")
		event, _ := json.Marshal(map[string]any{"id": id, "command": command})
		select {
		case client.events <- event:
		default:
			send(w, 429, map[string]string{"error": "Preview command queue is full"})
			return
		}
		timer := time.NewTimer(20 * time.Second)
		defer timer.Stop()
		select {
		case result := <-reply:
			if result.Error != "" {
				send(w, 400, map[string]string{"error": result.Error})
				return
			}
			if result.Result == nil {
				send(w, 502, map[string]string{"error": "Browser returned no diagnostic result"})
				return
			}
			if screenshot, ok := result.Result["screenshot"].(map[string]any); ok {
				data, _ := screenshot["dataURL"].(string)
				prefix := "data:image/png;base64,"
				if !strings.HasPrefix(data, prefix) {
					send(w, 502, map[string]string{"error": "Browser returned invalid PNG"})
					return
				}
				png, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(data, prefix))
				if err != nil {
					send(w, 502, map[string]string{"error": "Invalid PNG encoding"})
					return
				}
				path := "/api/control/frames/" + id + ".png"
				mu.Lock()
				images[path] = png
				imageOrder = append(imageOrder, path)
				if len(imageOrder) > 12 {
					delete(images, imageOrder[0])
					imageOrder = imageOrder[1:]
				}
				mu.Unlock()
				delete(screenshot, "dataURL")
				screenshot["url"] = path
			}
			result.Result["commandId"] = id
			result.Result["clientId"] = clientID
			send(w, 200, result.Result)
		case <-client.done:
			send(w, 503, map[string]string{"error": "Preview page disconnected"})
		case <-timer.C:
			send(w, 504, map[string]string{"error": "Preview did not acknowledge a painted frame within 20 seconds"})
		case <-r.Context().Done():
			return
		}
	})
	mux.HandleFunc("POST /api/control/result", func(w http.ResponseWriter, r *http.Request) {
		if !validOrigin(w, r) {
			return
		}
		var result controlReply
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 12<<20)).Decode(&result); err != nil {
			send(w, 400, map[string]string{"error": err.Error()})
			return
		}
		mu.Lock()
		p, ok := pending[result.ID]
		mu.Unlock()
		if !ok || p.client != result.ClientID {
			send(w, 409, map[string]string{"error": "Expired command or wrong preview client"})
			return
		}
		select {
		case p.reply <- result:
			send(w, 200, map[string]bool{"accepted": true})
		default:
			send(w, 409, map[string]string{"error": "Command already acknowledged"})
		}
	})
	mux.HandleFunc("GET /api/control/frames/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		png, ok := images[r.URL.Path]
		mu.Unlock()
		if !ok {
			http.Error(w, "Screenshot expired or unknown", 404)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(png)
	})
}
