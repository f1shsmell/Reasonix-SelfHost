package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/f1shsmell/Reasonix-SelfHost/internal/store"
)

// relayHarness is one relay over a real store: a device and as many tickets as
// the test asks for, dialable over loopback.
type relayHarness struct {
	t          *testing.T
	server     *httptest.Server
	deviceID   string
	credential string
	userID     int64
	store      *store.Store
}

func newRelayHarness(t *testing.T) *relayHarness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	owner, err := st.EnsureOwner("owner", "")
	if err != nil {
		t.Fatalf("ensure owner: %v", err)
	}
	device, credential, err := st.RegisterDevice(owner.ID, store.Registration{
		Name: "studio", Platform: "linux", PublicKey: "cHVibGljLWtleQ",
		Capabilities: []string{"desktop"},
	})
	if err != nil {
		t.Fatalf("register device: %v", err)
	}
	r := New(st)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/devices/{id}/connect", r.DeviceConnect)
	mux.HandleFunc("GET /v1/sessions/connect", r.ControllerConnect)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &relayHarness{t: t, server: server, deviceID: device.ID, credential: credential, userID: owner.ID, store: st}
}

func (h *relayHarness) dialDevice() *websocket.Conn {
	h.t.Helper()
	url := "ws" + strings.TrimPrefix(h.server.URL, "http") + "/v1/devices/" + h.deviceID + "/connect"
	header := http.Header{}
	header.Set("Authorization", "Bearer "+h.credential)
	conn, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		h.t.Fatalf("device dial: %v", err)
	}
	h.t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func (h *relayHarness) dialController() *websocket.Conn {
	h.t.Helper()
	return h.dialControllerWithScopes([]string{"desktop"})
}

func (h *relayHarness) controllerTicket(scopes []string) string {
	h.t.Helper()
	ticket, err := h.store.IssueTicket(h.deviceID, h.userID, scopes, time.Minute)
	if err != nil {
		h.t.Fatalf("issue ticket: %v", err)
	}
	return ticket
}

func (h *relayHarness) dialControllerWithScopes(scopes []string) *websocket.Conn {
	h.t.Helper()
	ticket := h.controllerTicket(scopes)
	url := "ws" + strings.TrimPrefix(h.server.URL, "http") + "/v1/sessions/connect"
	dialer := websocket.Dialer{Subprotocols: []string{"reasonix.remote.v1", "reasonix.auth." + ticket}}
	conn, _, err := dialer.Dial(url, nil)
	if err != nil {
		h.t.Fatalf("controller dial: %v", err)
	}
	h.t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func shortenWriteDeadline(t *testing.T) {
	t.Helper()
	previous := writeDeadline.Load()
	writeDeadline.Store(int64(300 * time.Millisecond))
	t.Cleanup(func() { writeDeadline.Store(previous) })
}

func readFrame(t *testing.T, conn *websocket.Conn, within time.Duration) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(within))
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	frame := map[string]any{}
	if err := json.Unmarshal(payload, &frame); err != nil {
		t.Fatalf("frame not json: %v", err)
	}
	return frame
}

// connectionID waits for the relay to name a freshly connected controller.
func awaitConnectionID(t *testing.T, deviceConn *websocket.Conn) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		frame := readFrame(t, deviceConn, time.Until(deadline))
		if frame["type"] == "controller_connected" {
			if id, _ := frame["connectionId"].(string); id != "" {
				return id
			}
		}
	}
	t.Fatal("relay never named the controller")
	return ""
}

func sendTo(t *testing.T, conn *websocket.Conn, to, payload string) {
	t.Helper()
	wire, err := json.Marshal(map[string]string{"to": to, "payload": payload})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, wire); err != nil {
		t.Fatalf("device write: %v", err)
	}
}

// A controller that stops reading must not stall the device read loop: its
// write times out, its socket closes, and another controller keeps receiving.
func TestStalledControllerDoesNotBlockOthers(t *testing.T) {
	shortenWriteDeadline(t)
	h := newRelayHarness(t)
	deviceConn := h.dialDevice()
	if hello := readFrame(t, deviceConn, 3*time.Second); hello["type"] != "relay_hello" {
		t.Fatalf("device hello = %v", hello)
	}

	stalled := h.dialController()
	stalledID := awaitConnectionID(t, deviceConn)
	live := h.dialController()
	liveID := awaitConnectionID(t, deviceConn)

	// Flood the stalled controller until the relay's write to it blocks. The
	// mutex keeps the marker write below on the same single-writer socket.
	var mu sync.Mutex
	floodDone := make(chan struct{})
	go func() {
		defer close(floodDone)
		big := strings.Repeat("x", 32<<10)
		for i := 0; i < 200; i++ {
			mu.Lock()
			wire, _ := json.Marshal(map[string]string{"to": stalledID, "payload": big})
			err := deviceConn.WriteMessage(websocket.TextMessage, wire)
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()

	// Give the flood time to fill the stalled socket's buffers, then prove the
	// live controller still receives. Without write deadlines this read never
	// completes: deliver is stuck on the stalled socket inside the read loop.
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	sendTo(t, deviceConn, liveID, "marker")
	mu.Unlock()

	_ = live.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, payload, err := live.ReadMessage()
	if err != nil {
		t.Fatalf("live controller starved by stalled peer: %v", err)
	}
	if string(payload) != "marker" {
		t.Fatalf("live controller read %q", payload)
	}

	// The relay must also have dropped the stalled controller and said so.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		frame := readFrame(t, deviceConn, time.Until(deadline))
		if frame["type"] == "controller_disconnected" && frame["connectionId"] == stalledID {
			<-floodDone
			// The stalled socket itself must be closed, not just forgotten.
			_ = stalled.SetReadDeadline(time.Now().Add(3 * time.Second))
			for {
				if _, _, err := stalled.ReadMessage(); err != nil {
					return
				}
			}
		}
	}
	t.Fatal("relay never disconnected the stalled controller")
}

// A device that stops reading must not pin its controllers' read loops: the
// write to it times out and the whole device link is released.
func TestStalledDeviceReleasesControllers(t *testing.T) {
	shortenWriteDeadline(t)
	h := newRelayHarness(t)
	deviceConn := h.dialDevice()
	if hello := readFrame(t, deviceConn, 3*time.Second); hello["type"] != "relay_hello" {
		t.Fatalf("device hello = %v", hello)
	}
	controller := h.dialController()

	// The device never reads again. Flood controller→device frames until the
	// relay's write to the device blocks; the deadline must then close the
	// device link, which closes this controller too.
	go func() {
		big := strings.Repeat("x", 32<<10)
		for i := 0; i < 200; i++ {
			if err := controller.WriteMessage(websocket.TextMessage, []byte(big)); err != nil {
				return
			}
		}
	}()

	_ = controller.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := controller.ReadMessage(); err == nil {
		t.Fatal("controller outlived the stalled device")
	}
}

// A files-only grant connects, and the scopes cross to the device exactly as
// minted: the relay forwards the list, it never edits it.
func TestFilesOnlyGrantConnectsAndScopesCrossUnchanged(t *testing.T) {
	h := newRelayHarness(t)
	deviceConn := h.dialDevice()
	if hello := readFrame(t, deviceConn, 3*time.Second); hello["type"] != "relay_hello" {
		t.Fatalf("device hello = %v", hello)
	}
	controller := h.dialControllerWithScopes([]string{"files"})
	id := awaitConnectionID(t, deviceConn)
	if err := controller.WriteMessage(websocket.TextMessage, []byte("sealed-request")); err != nil {
		t.Fatalf("controller write: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		frame := readFrame(t, deviceConn, time.Until(deadline))
		if frame["type"] != "controller_message" {
			continue
		}
		if frame["payload"] != "sealed-request" || frame["connectionId"] != id {
			t.Fatalf("controller_message = %v", frame)
		}
		scopes, _ := frame["scopes"].([]any)
		if len(scopes) != 1 || scopes[0] != "files" {
			t.Fatalf("forwarded scopes = %v, want [files]", frame["scopes"])
		}
		return
	}
	t.Fatal("the device never saw the controller's message")
}

func TestGrantWithoutAKnownScopeIsRefused(t *testing.T) {
	h := newRelayHarness(t)
	// The store defaults an empty list to desktop, so only a list of unknown
	// names can reach the gate without a known scope.
	for _, scopes := range [][]string{{"bogus"}, {"bogus", "worse"}} {
		ticket := h.controllerTicket(scopes)
		url := "ws" + strings.TrimPrefix(h.server.URL, "http") + "/v1/sessions/connect"
		dialer := websocket.Dialer{Subprotocols: []string{"reasonix.remote.v1", "reasonix.auth." + ticket}}
		conn, resp, err := dialer.Dial(url, nil)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("scopes %v connected, want a refusal", scopes)
		}
		if resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Fatalf("scopes %v dial = %v (resp %v), want 403", scopes, err, resp)
		}
	}
}
