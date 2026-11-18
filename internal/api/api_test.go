package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/f1shsmell/Reasonix-SelfHost/internal/config"
	"github.com/f1shsmell/Reasonix-SelfHost/internal/store"
)

const adminToken = "test-admin-token-0123456789"

// harness is one deployment: a SQLite file in the test's own directory and a
// server listening on loopback, so the whole flow runs the real code.
type harness struct {
	t      *testing.T
	cfg    config.Config
	server *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Config{
		Addr:        "127.0.0.1:0",
		DataDir:     t.TempDir(),
		PublicURL:   "https://id.example.test",
		RelayURL:    "https://relay.example.test",
		OwnerHandle: "owner",
		AdminToken:  adminToken,
		GrantTTL:    2 * time.Minute,
		TicketTTL:   time.Minute,
		AllowedOrigins: []string{"https://app.example.test"},
	}
	server, err := New(cfg, st)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return &harness{t: t, cfg: cfg, server: httpServer}
}

func (h *harness) do(method, path, token string, body any) (int, map[string]any) {
	h.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		h.t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// signIn runs the whole device flow and returns the session token.
func (h *harness) signIn() string {
	h.t.Helper()
	status, grant := h.do(http.MethodPost, "/device/start", "", nil)
	if status != http.StatusOK {
		h.t.Fatalf("device/start = %d", status)
	}
	userCode, _ := grant["userCode"].(string)
	deviceCode, _ := grant["deviceCode"].(string)
	if userCode == "" || deviceCode == "" {
		h.t.Fatalf("device/start returned %v", grant)
	}
	if uri, _ := grant["verificationUriComplete"].(string); !strings.Contains(uri, h.cfg.PublicURL) {
		h.t.Fatalf("verification link %q is not on this deployment", uri)
	}

	// Pending until the person approves.
	status, polled := h.do(http.MethodPost, "/device/poll", "", map[string]string{"deviceCode": deviceCode})
	if status != http.StatusOK || polled["status"] != "pending" {
		h.t.Fatalf("first poll = %d %v", status, polled)
	}

	// The approval page takes the admin token in the body.
	form := strings.NewReader("userCode=" + userCode + "&token=" + adminToken)
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/device/approve", form)
	if err != nil {
		h.t.Fatalf("approve request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("approve: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("approve = %d", resp.StatusCode)
	}

	status, polled = h.do(http.MethodPost, "/device/poll", "", map[string]string{"deviceCode": deviceCode})
	if status != http.StatusOK || polled["status"] != "complete" {
		h.t.Fatalf("second poll = %d %v", status, polled)
	}
	token, _ := polled["sessionToken"].(string)
	if token == "" {
		h.t.Fatalf("poll returned no session token: %v", polled)
	}
	// The grant is single-use.
	status, _ = h.do(http.MethodPost, "/device/poll", "", map[string]string{"deviceCode": deviceCode})
	if status != http.StatusBadRequest {
		h.t.Fatalf("replayed poll = %d, want 400", status)
	}
	return token
}

func TestDeviceFlowAndRelay(t *testing.T) {
	h := newHarness(t)
	token := h.signIn()

	status, me := h.do(http.MethodGet, "/me", token, nil)
	if status != http.StatusOK {
		t.Fatalf("/me = %d", status)
	}
	user, _ := me["user"].(map[string]any)
	if user["handle"] != "owner" {
		t.Fatalf("/me = %v", me)
	}

	// A Studio registers with the key a controller will seal to.
	status, registered := h.do(http.MethodPost, "/me/devices", token, map[string]any{
		"name": "test-studio", "platform": "linux", "publicKey": "cHVibGljLWtleQ",
		"capabilities": []string{"desktop"},
	})
	if status != http.StatusOK {
		t.Fatalf("register device = %d %v", status, registered)
	}
	device, _ := registered["device"].(map[string]any)
	deviceID, _ := device["id"].(string)
	credential, _ := registered["deviceCredential"].(string)
	if deviceID == "" || credential == "" {
		t.Fatalf("registration incomplete: %v", registered)
	}

	status, listed := h.do(http.MethodGet, "/me/devices", token, nil)
	if status != http.StatusOK {
		t.Fatalf("list devices = %d", status)
	}
	if devices, _ := listed["devices"].([]any); len(devices) != 1 {
		t.Fatalf("devices = %v", listed)
	}

	// The device dials the relay with its credential.
	deviceURL := "ws" + strings.TrimPrefix(h.server.URL, "http") + "/v1/devices/" + deviceID + "/connect"
	deviceHeader := http.Header{}
	deviceHeader.Set("Authorization", "Bearer "+credential)
	deviceConn, _, err := websocket.DefaultDialer.Dial(deviceURL, deviceHeader)
	if err != nil {
		t.Fatalf("device dial: %v", err)
	}
	defer deviceConn.Close()
	if hello := readFrame(t, deviceConn); hello["type"] != "relay_hello" {
		t.Fatalf("device hello = %v", hello)
	}

	// A controller asks for a ticket and spends it on one socket.
	status, granted := h.do(http.MethodPost, "/me/remote-grants", token, map[string]string{"targetDeviceId": deviceID})
	if status != http.StatusOK {
		t.Fatalf("grant = %d %v", status, granted)
	}
	if relay, _ := granted["relay"].(string); relay != h.cfg.RelayURL {
		t.Fatalf("grant relay = %v, want %q", granted["relay"], h.cfg.RelayURL)
	}
	grant, _ := granted["grant"].(map[string]any)
	ticket, _ := grant["ticket"].(string)
	if ticket == "" {
		t.Fatalf("grant carried no ticket: %v", granted)
	}

	controllerURL := "ws" + strings.TrimPrefix(h.server.URL, "http") + "/v1/sessions/connect"
	dialer := websocket.Dialer{Subprotocols: []string{"reasonix.remote.v1", "reasonix.auth." + ticket}}
	controllerConn, _, err := dialer.Dial(controllerURL, nil)
	if err != nil {
		t.Fatalf("controller dial: %v", err)
	}
	defer controllerConn.Close()

	// The device learns the connection id from the relay.
	connectionID := ""
	deadline := time.Now().Add(5 * time.Second)
	for connectionID == "" && time.Now().Before(deadline) {
		frame := readFrame(t, deviceConn)
		switch frame["type"] {
		case "controller_connected":
			connectionID, _ = frame["connectionId"].(string)
		case "relay_hello":
			if ids, _ := frame["controllers"].([]any); len(ids) == 1 {
				connectionID, _ = ids[0].(string)
			}
		}
	}
	if connectionID == "" {
		t.Fatal("relay never named the controller")
	}

	// Controller to device.
	if err := controllerConn.WriteMessage(websocket.TextMessage, []byte("sealed-request")); err != nil {
		t.Fatalf("controller write: %v", err)
	}
	for {
		frame := readFrame(t, deviceConn)
		if frame["type"] != "controller_message" {
			continue
		}
		if frame["payload"] != "sealed-request" || frame["connectionId"] != connectionID {
			t.Fatalf("controller_message = %v", frame)
		}
		if scopes, _ := frame["scopes"].([]any); len(scopes) != 1 || scopes[0] != "desktop" {
			t.Fatalf("controller_message scopes = %v", frame["scopes"])
		}
		break
	}

	// Device to controller.
	reply, _ := json.Marshal(map[string]string{"to": connectionID, "payload": "sealed-reply"})
	if err := deviceConn.WriteMessage(websocket.TextMessage, reply); err != nil {
		t.Fatalf("device write: %v", err)
	}
	controllerConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, payload, err := controllerConn.ReadMessage()
	if err != nil {
		t.Fatalf("controller read: %v", err)
	}
	if string(payload) != "sealed-reply" {
		t.Fatalf("controller read %q", payload)
	}
}

func TestGrantRefusesAnotherAccountsDevice(t *testing.T) {
	h := newHarness(t)
	token := h.signIn()

	status, body := h.do(http.MethodPost, "/me/remote-grants", token, map[string]string{"targetDeviceId": "not-a-device"})
	if status != http.StatusNotFound {
		t.Fatalf("grant for unknown device = %d %v", status, body)
	}
}

func TestAdminTokenIsRequiredToApprove(t *testing.T) {
	h := newHarness(t)
	_, grant := h.do(http.MethodPost, "/device/start", "", nil)
	userCode, _ := grant["userCode"].(string)

	status, body := h.do(http.MethodPost, "/device/approve", "wrong-token", map[string]string{"userCode": userCode})
	if status != http.StatusUnauthorized {
		t.Fatalf("approve with a wrong token = %d %v", status, body)
	}
}

func TestCorsOnlyAnswersConfiguredOrigins(t *testing.T) {
	h := newHarness(t)

	req, _ := http.NewRequest(http.MethodOptions, h.server.URL+"/device/start", nil)
	req.Header.Set("Origin", "https://evil.example.test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("reflected an unconfigured origin: %q", got)
	}

	req, _ = http.NewRequest(http.MethodOptions, h.server.URL+"/device/start", nil)
	req.Header.Set("Origin", "https://app.example.test")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example.test" {
		t.Fatalf("configured origin = %q", got)
	}
}

func TestApprovalPageShowsTheCode(t *testing.T) {
	h := newHarness(t)
	_, grant := h.do(http.MethodPost, "/device/start", "", nil)
	userCode, _ := grant["userCode"].(string)

	resp, err := http.Get(h.server.URL + "/device?code=" + strings.ReplaceAll(userCode, "-", ""))
	if err != nil {
		t.Fatalf("approval page: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approval page = %d", resp.StatusCode)
	}
	buf := make([]byte, 8192)
	read, _ := resp.Body.Read(buf)
	page := string(buf[:read])
	if !strings.Contains(page, "设备登录") || !strings.Contains(page, userCode) {
		t.Fatalf("approval page did not show the code:\n%s", page)
	}
}

func readFrame(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read relay frame: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(payload, &frame); err != nil {
		t.Fatalf("relay frame %q: %v", payload, err)
	}
	return frame
}
