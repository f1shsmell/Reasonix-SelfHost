// Package relay carries ciphertext between a Studio and its controllers.
//
// The relay understands routing and nothing else: the handshake, the keys and
// every byte of content are sealed end to end by the two ends. It never holds a
// decryption key, so a compromised relay loses the metadata it already has —
// connection timing, byte counts, and which device a controller named.
package relay

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"

	"github.com/f1shsmell/Reasonix-SelfHost/internal/store"
)

// The scope a self-hosted controller is granted. The kernel checks this list
// before running anything, so a relay that widened it would widen the kernel.
const desktopScope = "desktop"

// heartbeat is how often the device should ping, in milliseconds. The kernel
// clamps whatever is sent to between 5 s and one minute.
const heartbeat = 20_000

// maxFrame bounds one relayed message. The kernel refuses to write a reply
// above 64 KiB, so a larger frame cannot be a legal one; the limit keeps a
// hostile controller from making the relay buffer without end.
const maxFrame = 96 << 10

// Relay owns the live sockets. Device connections are keyed by device id;
// controllers are keyed by the connection id the relay invents for them.
type Relay struct {
	store    *store.Store
	upgrader websocket.Upgrader

	mu      sync.Mutex
	devices map[string]*deviceLink
}

// deviceLink is one connected Studio.
type deviceLink struct {
	conn *websocket.Conn
	// write serialises frames; gorilla allows one concurrent writer.
	write       sync.Mutex
	controllers map[string]*controllerLink
}

// controllerLink is one connected phone.
type controllerLink struct {
	conn   *websocket.Conn
	write  sync.Mutex
	device string
}

// New builds the relay. The upgrader answers the subprotocol a controller
// offers, because a browser closes a socket whose accepted protocols do not
// name one it asked for.
func New(st *store.Store) *Relay {
	return &Relay{
		store:   st,
		devices: map[string]*deviceLink{},
		upgrader: websocket.Upgrader{
			Subprotocols:    []string{"reasonix.remote.v1"},
			ReadBufferSize:  8 << 10,
			WriteBufferSize: 8 << 10,
		},
	}
}

// ---- device side -----------------------------------------------------

// DeviceConnect is `GET /v1/devices/{id}/connect`, dialled by the kernel with
// its device credential. The id in the path is a claim; the credential is the
// proof, and the credential decides which device this is.
func (r *Relay) DeviceConnect(w http.ResponseWriter, req *http.Request) {
	credential := bearer(req)
	device, err := r.store.DeviceByCredential(credential)
	if err != nil {
		http.Error(w, "unknown device", http.StatusUnauthorized)
		return
	}
	if id := req.PathValue("id"); id != "" && id != device.ID {
		http.Error(w, "unknown device", http.StatusUnauthorized)
		return
	}
	conn, err := r.upgrader.Upgrade(w, req, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(maxFrame)

	link := &deviceLink{conn: conn, controllers: map[string]*controllerLink{}}

	r.mu.Lock()
	previous := r.devices[device.ID]
	r.devices[device.ID] = link
	r.mu.Unlock()
	if previous != nil {
		// A reconnect replaces the older socket. Closing it is what stops two
		// copies of one Studio from both being told they are online.
		_ = previous.conn.Close()
	}
	r.store.TouchDevice(device.ID)
	go r.runDevice(device.ID, link)
}

func (r *Relay) runDevice(deviceID string, link *deviceLink) {
	defer func() {
		r.mu.Lock()
		if r.devices[deviceID] == link {
			delete(r.devices, deviceID)
		}
		r.mu.Unlock()
		link.closeControllers()
		_ = link.conn.Close()
	}()

	// The device is told what it already has, so it can drop sessions the relay
	// no longer holds before any controller speaks.
	link.sendHello()

	for {
		_, payload, err := link.conn.ReadMessage()
		if err != nil {
			return
		}
		var frame struct {
			To           string `json:"to"`
			Payload      string `json:"payload"`
			Type         string `json:"type"`
			ConnectionID string `json:"connectionId"`
		}
		if json.Unmarshal(payload, &frame) != nil {
			continue
		}
		switch frame.Type {
		case "disconnect_controller":
			r.disconnect(deviceID, frame.ConnectionID)
			continue
		case "":
		default:
			// heartbeat_ack and anything newer: the device needs no answer.
			continue
		}
		if frame.To == "" || frame.Payload == "" {
			continue
		}
		r.deliver(deviceID, frame.To, frame.Payload)
	}
}

func (link *deviceLink) sendHello() {
	ids := make([]string, 0, len(link.controllers))
	for id := range link.controllers {
		ids = append(ids, id)
	}
	link.sendJSON(map[string]any{
		"type": "relay_hello", "controllers": ids, "heartbeatMs": heartbeat,
	})
}

func (link *deviceLink) sendJSON(value any) error {
	wire, err := json.Marshal(value)
	if err != nil {
		return err
	}
	link.write.Lock()
	defer link.write.Unlock()
	return link.conn.WriteMessage(websocket.TextMessage, wire)
}

func (link *deviceLink) closeControllers() {
	link.write.Lock()
	defer link.write.Unlock()
	for _, controller := range link.controllers {
		_ = controller.conn.Close()
	}
	link.controllers = map[string]*controllerLink{}
}

// ---- controller side -------------------------------------------------

// ControllerConnect is `GET /v1/sessions/connect`, dialled by a phone. The
// ticket is the whole authorisation: it names the device, it was minted for one
// use, and it is spent before any socket exists.
func (r *Relay) ControllerConnect(w http.ResponseWriter, req *http.Request) {
	ticket, offered := ticketFromSubprotocols(websocket.Subprotocols(req))
	if ticket == "" {
		http.Error(w, "no grant", http.StatusServiceUnavailable)
		return
	}
	grant, err := r.store.SpendTicket(ticket)
	if err != nil {
		if errors.Is(err, store.ErrExpired) {
			http.Error(w, "grant expired", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "no grant", http.StatusServiceUnavailable)
		return
	}
	if !hasScope(grant.Scopes, desktopScope) {
		http.Error(w, "grant lacks desktop scope", http.StatusForbidden)
		return
	}

	r.mu.Lock()
	device := r.devices[grant.DeviceID]
	r.mu.Unlock()
	if device == nil {
		http.Error(w, "device offline", http.StatusServiceUnavailable)
		return
	}

	// Enroll before the upgrade so a failure here is still an answer on the wire
	// rather than a socket that opens and closes with no reason. The row is keyed
	// by the phone's user agent: the relay has no other stable name for "the same
	// phone again".
	if _, err := r.store.EnrollController(grant.DeviceID, grant.UserID, req.UserAgent()); err != nil {
		http.Error(w, "cannot enroll the controller", http.StatusInternalServerError)
		return
	}

	conn, err := r.upgrader.Upgrade(w, req, nil)
	if err != nil {
		return
	}
	_ = offered
	conn.SetReadLimit(maxFrame)

	id := connectionID()
	link := &controllerLink{conn: conn, device: grant.DeviceID}

	r.mu.Lock()
	current := r.devices[grant.DeviceID]
	if current != nil {
		current.controllers[id] = link
	}
	r.mu.Unlock()
	if current == nil {
		// The device left between the first read and the upgrade. The row this
		// dial enrolled must not survive it: a notify would count a phone that
		// has no socket at all.
		r.store.RemoveUnseenController(grant.DeviceID, req.UserAgent())
		_ = conn.Close()
		return
	}

	if err := current.sendJSON(map[string]any{"type": "controller_connected", "connectionId": id}); err != nil {
		r.disconnect(grant.DeviceID, id)
		_ = conn.Close()
		return
	}
	current.sendHello()

	go r.runController(grant.DeviceID, id, link, grant.Scopes)
}

func (r *Relay) runController(deviceID, id string, link *controllerLink, scopes []string) {
	defer func() {
		r.disconnect(deviceID, id)
		_ = link.conn.Close()
	}()
	for {
		kind, payload, err := link.conn.ReadMessage()
		if err != nil {
			return
		}
		if kind != websocket.TextMessage {
			continue
		}
		r.toDevice(deviceID, map[string]any{
			"type":         "controller_message",
			"connectionId": id,
			"scopes":       scopes,
			"payload":      string(payload),
		})
	}
}

// deliver hands one sealed payload to the controller the device named. A
// payload for a controller that left is dropped: the device learns about the
// disconnect through relay_hello.
func (r *Relay) deliver(deviceID, connectionID, payload string) {
	r.mu.Lock()
	device := r.devices[deviceID]
	var target *controllerLink
	if device != nil {
		target = device.controllers[connectionID]
	}
	r.mu.Unlock()
	if target == nil {
		return
	}
	target.write.Lock()
	defer target.write.Unlock()
	_ = target.conn.WriteMessage(websocket.TextMessage, []byte(payload))
}

// toDevice sends a routing frame to the Studio, if it is still connected.
func (r *Relay) toDevice(deviceID string, frame map[string]any) {
	r.mu.Lock()
	device := r.devices[deviceID]
	r.mu.Unlock()
	if device == nil {
		return
	}
	if err := device.sendJSON(frame); err != nil {
		_ = device.conn.Close()
	}
}

// disconnect drops one controller and tells the device, which is what makes the
// kernel's presence list follow the relay instead of guessing.
func (r *Relay) disconnect(deviceID, connectionID string) {
	if connectionID == "" {
		return
	}
	r.mu.Lock()
	device := r.devices[deviceID]
	var target *controllerLink
	if device != nil {
		target = device.controllers[connectionID]
		delete(device.controllers, connectionID)
	}
	r.mu.Unlock()
	if target != nil {
		_ = target.conn.Close()
	}
	if device != nil {
		_ = device.sendJSON(map[string]any{"type": "controller_disconnected", "connectionId": connectionID})
		device.sendHello()
	}
}

// ---- helpers ---------------------------------------------------------

func bearer(req *http.Request) string {
	value := req.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(value) > len(prefix) && value[:len(prefix)] == prefix {
		return value[len(prefix):]
	}
	return ""
}

func ticketFromSubprotocols(offered []string) (ticket string, ok bool) {
	found := false
	for _, value := range offered {
		if value == "reasonix.remote.v1" {
			found = true
		}
		if len(value) > len("reasonix.auth.") && value[:len("reasonix.auth.")] == "reasonix.auth." {
			ticket = value[len("reasonix.auth."):]
		}
	}
	return ticket, found
}

func hasScope(scopes []string, want string) bool {
	for _, scope := range scopes {
		if scope == want {
			return true
		}
	}
	return false
}

var connectionCounter struct {
	sync.Mutex
	n uint64
}

// connectionID is 32 hex characters, which is the shape the kernel accepts for
// a controller identity it is asked to disconnect.
func connectionID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand failing is not a case worth refusing a connection over;
		// the counter still keeps ids apart within one process.
		connectionCounter.Lock()
		connectionCounter.n++
		seed := connectionCounter.n
		connectionCounter.Unlock()
		for index := range raw {
			raw[index] = byte(seed >> (uint(index%8) * 8))
		}
	}
	return hex.EncodeToString(raw)
}
