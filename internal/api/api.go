// Package api is the HTTP surface a self-hosted deployment serves: the accounts
// endpoints a phone and a Studio already know, the approval page a person opens
// once, and the two relay sockets.
//
// The routes are the ones the shipped kernel and the Android proxy already
// call, so a self-hosted install is a base-URL change rather than a second
// protocol. Nothing here is multi-tenant: one owner, one account, and every
// token is a session for that owner.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/f1shsmell/Reasonix-SelfHost/internal/config"
	"github.com/f1shsmell/Reasonix-SelfHost/internal/relay"
	"github.com/f1shsmell/Reasonix-SelfHost/internal/store"
)

// pollInterval is the slow_down window a client is held to. The device flow's
// RFC default is five seconds; the kernels honour whatever the grant carried,
// so this value is also what /device/start reports.
const pollInterval = 5 * time.Second

// Server holds everything a request needs. The owner is resolved once at
// startup: a deployment that cannot name its account should not answer at all.
type Server struct {
	cfg   config.Config
	store *store.Store
	relay *relay.Relay
	owner store.User

	approval *template.Template
}

// New wires the routes. The relay shares the mux because the two origins a
// deployment names usually resolve to one process behind one proxy.
func New(cfg config.Config, st *store.Store) (*Server, error) {
	owner, err := st.EnsureOwner(cfg.OwnerHandle, cfg.OwnerEmail)
	if err != nil {
		return nil, fmt.Errorf("api: cannot resolve the owner account: %w", err)
	}
	approval, err := template.New("approval").Parse(approvalPage)
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, store: st, relay: relay.New(st), owner: owner, approval: approval}, nil
}

// Handler returns the whole surface behind the CORS and logging wrapper.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	// ---- device sign-in -------------------------------------------------
	mux.HandleFunc("POST /device/start", s.startDevice)
	mux.HandleFunc("POST /device/poll", s.pollDevice)
	mux.HandleFunc("POST /device/approve", s.approveDevice)
	mux.HandleFunc("POST /device/deny", s.denyDevice)
	mux.HandleFunc("GET /device", s.approvalPage)

	// ---- account --------------------------------------------------------
	mux.HandleFunc("GET /me", s.me)
	mux.HandleFunc("POST /auth/logout", s.logout)
	mux.HandleFunc("GET /me/devices", s.listDevices)
	mux.HandleFunc("POST /me/devices", s.registerDevice)
	mux.HandleFunc("DELETE /me/devices/{id}", s.revokeDevice)
	mux.HandleFunc("POST /me/remote-grants", s.grant)

	// ---- relay ----------------------------------------------------------
	mux.HandleFunc("GET /v1/devices/{id}/connect", s.relay.DeviceConnect)
	mux.HandleFunc("GET /v1/sessions/connect", s.relay.ControllerConnect)

	// ---- host (a Studio talking about its enrolled phones) --------------
	mux.HandleFunc("POST /host/push/notify", s.notifyPhones)
	mux.HandleFunc("GET /host/controllers", s.listControllers)
	mux.HandleFunc("POST /host/controllers/{id}/resolve", s.resolveController)
	mux.HandleFunc("POST /host/controllers/{id}/revoke", s.revokeController)
	mux.HandleFunc("GET /host/controllers/{id}/report", s.readControllerReport)
	mux.HandleFunc("POST /host/controllers/{id}/seen", s.clearControllerReport)

	return s.cors(s.logged(mux))
}

// Close releases nothing yet; it exists so the process has one shutdown call
// even after the relay learns to drain its sockets.
func (s *Server) Close() {}

// ---- device sign-in ---------------------------------------------------

// deviceAuth is the credential a registered Studio proves itself with when it
// calls the host routes on its own behalf. It is not an account session: the
// id in the header is a claim, the credential is the proof, and the credential
// decides which device this is — the same rule the relay socket follows.
func (s *Server) deviceAuth(req *http.Request) (store.Device, error) {
	declared := strings.TrimSpace(req.Header.Get("x-reasonix-device-id"))
	credential := strings.TrimSpace(req.Header.Get("x-reasonix-device-credential"))
	if declared == "" || credential == "" {
		return store.Device{}, store.ErrUnauthorized
	}
	device, err := s.store.DeviceByCredential(credential)
	if err != nil {
		return store.Device{}, err
	}
	if device.ID != declared {
		return store.Device{}, store.ErrUnauthorized
	}
	return device, nil
}

// deviceAuthWithOwner is deviceAuth plus the account the device belongs to. The
// host routes scope every read and every change to one owner, and a self-hosted
// deployment has exactly one, so the pair is what they need.
func (s *Server) deviceAuthWithOwner(req *http.Request) (store.Device, int64, error) {
	declared := strings.TrimSpace(req.Header.Get("x-reasonix-device-id"))
	credential := strings.TrimSpace(req.Header.Get("x-reasonix-device-credential"))
	if declared == "" || credential == "" {
		return store.Device{}, 0, store.ErrUnauthorized
	}
	device, userID, err := s.store.DeviceByCredentialWithOwner(credential)
	if err != nil {
		return store.Device{}, 0, err
	}
	if device.ID != declared {
		return store.Device{}, 0, store.ErrUnauthorized
	}
	return device, userID, nil
}

// notificationKinds are the three occasions a desktop may report. The hosted
// service accepted these and nothing else, so no conversation text has anywhere
// to go; keeping the list closed keeps that true here.
var notificationKinds = map[string]bool{"approval": true, "input": true, "error": true}

// notifyPhones is `POST /host/push/notify`: a desktop saying that something is
// waiting, as a kind and a count.
//
// The answer is the same {delivered, failed, removed} the kernel parses. Nothing
// here reaches the phone directly — a self-hosted deployment has no push
// service — so the report is recorded against each active controller and a
// phone reads it when it next asks; see /host/controllers.
func (s *Server) notifyPhones(w http.ResponseWriter, req *http.Request) {
	device, err := s.deviceAuth(req)
	if err != nil {
		s.fail(w, http.StatusUnauthorized, "unauthorized", "A device credential is required.")
		return
	}
	var in struct {
		Type  string `json:"type"`
		Count int    `json:"count"`
	}
	if err := decode(req, &in); err != nil {
		s.fail(w, http.StatusBadRequest, "invalid_json", "Request body must be valid JSON.")
		return
	}
	if !notificationKinds[in.Type] {
		s.fail(w, http.StatusBadRequest, "invalid_kind", "A report is approval, input or error.")
		return
	}
	if in.Count < 1 || in.Count > 99 {
		s.fail(w, http.StatusBadRequest, "invalid_count", "A count is between 1 and 99.")
		return
	}
	// The owner is the account the device belongs to; a self-hosted deployment
	// has one, and a device outside it is not this owner's to report against.
	_, userID, err := s.store.DeviceByID(device.ID)
	if err != nil {
		s.fail(w, http.StatusUnauthorized, "unauthorized", "A device credential is required.")
		return
	}
	delivered, err := s.store.NotifyControllers(userID, device.ID, in.Type, in.Count)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "internal", "Could not record the report.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"delivered": delivered, "failed": 0, "removed": 0,
	})
}

// controllerWire is the shape the kernel's Phone reads. Fields a self-hosted
// deployment has no opinion about are answered with the honest empty value
// rather than omitted: a frontend that renders a row expects the key.
func controllerWire(controller store.Controller) map[string]any {
	lastSeen := controller.CreatedAt
	if controller.LastSeenAt != nil {
		lastSeen = *controller.LastSeenAt
	}
	out := map[string]any{
		"id":            controller.ID,
		"name":          controller.Name,
		"nameSource":    "",
		"state":         controller.State,
		"uaClass":       uaClass(controller.UserAgent),
		"createdAt":     controller.CreatedAt,
		"lastSeenAt":    lastSeen,
		"keyThumbprint": "",
	}
	if controller.RevokedAt != nil {
		out["revokedAt"] = *controller.RevokedAt
	}
	if controller.ReplaceID != "" {
		out["replaceCandidate"] = controller.ReplaceID
	}
	return out
}

// uaClass is the coarse family a controller's user agent names. The hosted
// service used it to tell two phones apart in a list; the same three families
// cover what reaches a self-hosted relay.
func uaClass(userAgent string) string {
	// The four values the window already knows, from port/phone_device.ts:
	// a phone in the app's WebView is told apart from the same phone in a
	// browser, because only the second one is a "Web Studio" session.
	lower := strings.ToLower(userAgent)
	switch {
	case lower == "":
		return ""
	case strings.Contains(lower, "iphone"), strings.Contains(lower, "ipad"), strings.Contains(lower, "ipod"):
		if strings.Contains(lower, "safari") &&
			!strings.Contains(lower, "fxios") && !strings.Contains(lower, "crios") && !strings.Contains(lower, "edgios") {
			return "ios-safari"
		}
		return "ios-webview"
	case strings.Contains(lower, "android"):
		// Go's transport and Android's WebView both spell the marker as
		// `; wv`, and the closing paren only appears when another field
		// follows it — matching on `; wv)` alone missed half of them.
		if strings.Contains(lower, "; wv") {
			return "android-webview"
		}
		return "android-chrome"
	case strings.Contains(lower, "mozilla"), strings.Contains(lower, "chrome"), strings.Contains(lower, "safari"):
		return "browser"
	}
	return "other"
}

// listControllers is `GET /host/controllers?state=`: the phones enrolled
// against this desktop, which is what the window draws under "设备".
func (s *Server) listControllers(w http.ResponseWriter, req *http.Request) {
	device, userID, err := s.deviceAuthWithOwner(req)
	if err != nil {
		s.fail(w, http.StatusUnauthorized, "unauthorized", "A device credential is required.")
		return
	}
	state := req.URL.Query().Get("state")
	controllers, err := s.store.Controllers(userID, device.ID, state)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.fail(w, http.StatusBadRequest, "invalid_state", "A state is pending, active, revoked or all.")
			return
		}
		s.fail(w, http.StatusInternalServerError, "internal", "Could not read the controllers.")
		return
	}
	wire := make([]map[string]any, 0, len(controllers))
	for _, controller := range controllers {
		wire = append(wire, controllerWire(controller))
	}
	writeJSON(w, http.StatusOK, map[string]any{"controllers": wire})
}

// resolveController is `POST /host/controllers/{id}/resolve`: the owner's answer
// to a waiting phone. `add` makes it active, `reject` ends it, and `replace`
// activates it while ending the one it names — two active copies of one phone
// would both be told they are connected.
func (s *Server) resolveController(w http.ResponseWriter, req *http.Request) {
	device, userID, err := s.deviceAuthWithOwner(req)
	if err != nil {
		s.fail(w, http.StatusUnauthorized, "unauthorized", "A device credential is required.")
		return
	}
	var in struct {
		Action    string `json:"action"`
		ReplaceID string `json:"replaceId"`
	}
	if err := decode(req, &in); err != nil {
		s.fail(w, http.StatusBadRequest, "invalid_json", "Request body must be valid JSON.")
		return
	}
	var state string
	switch in.Action {
	case "add":
		state = store.ControllerActive
	case "reject":
		state = store.ControllerRevoked
	case "replace":
		if strings.TrimSpace(in.ReplaceID) == "" {
			s.fail(w, http.StatusBadRequest, "missing_replace_id", "A replacement names the controller it replaces.")
			return
		}
		state = store.ControllerActive
	default:
		s.fail(w, http.StatusBadRequest, "invalid_action", "An action is add, replace or reject.")
		return
	}
	err = s.store.SetControllerState(userID, device.ID, req.PathValue("id"), state, in.ReplaceID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.fail(w, http.StatusNotFound, "controller_not_found", "No such controller on this desktop.")
			return
		}
		s.fail(w, http.StatusInternalServerError, "internal", "Could not answer the controller.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// revokeController is `POST /host/controllers/{id}/revoke`. It cannot be undone:
// the phone starts again with a fresh ticket.
func (s *Server) revokeController(w http.ResponseWriter, req *http.Request) {
	device, userID, err := s.deviceAuthWithOwner(req)
	if err != nil {
		s.fail(w, http.StatusUnauthorized, "unauthorized", "A device credential is required.")
		return
	}
	err = s.store.SetControllerState(userID, device.ID, req.PathValue("id"), store.ControllerRevoked, "")
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.fail(w, http.StatusNotFound, "controller_not_found", "No such controller on this desktop.")
			return
		}
		s.fail(w, http.StatusInternalServerError, "internal", "Could not revoke the controller.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// readControllerReport is `GET /host/controllers/{id}/report`: what is waiting
// for this phone, if anything. A phone polls this on its own timer, because a
// self-hosted deployment has no push service to reach it with.
func (s *Server) readControllerReport(w http.ResponseWriter, req *http.Request) {
	device, userID, err := s.deviceAuthWithOwner(req)
	if err != nil {
		s.fail(w, http.StatusUnauthorized, "unauthorized", "A device credential is required.")
		return
	}
	report, err := s.store.PendingReport(userID, device.ID, req.PathValue("id"))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, http.StatusInternalServerError, "internal", "Could not read the report.")
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		s.fail(w, http.StatusNotFound, "controller_not_found", "No such controller on this desktop.")
		return
	}
	if report == nil {
		writeJSON(w, http.StatusOK, map[string]any{"pending": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pending": true, "type": report.Kind, "count": report.Count, "notifiedAt": report.NotifiedAt,
	})
}

// clearControllerReport is `POST /host/controllers/{id}/seen`: the phone has
// read what was waiting, so the next poll answers nothing.
func (s *Server) clearControllerReport(w http.ResponseWriter, req *http.Request) {
	device, userID, err := s.deviceAuthWithOwner(req)
	if err != nil {
		s.fail(w, http.StatusUnauthorized, "unauthorized", "A device credential is required.")
		return
	}
	if err := s.store.ClearReport(userID, device.ID, req.PathValue("id")); err != nil {
		s.fail(w, http.StatusInternalServerError, "internal", "Could not clear the report.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// startDevice opens a sign-in. The answer is the RFC 8628 shape the kernel
// already parses, with the verification links pointing at this deployment.
func (s *Server) startDevice(w http.ResponseWriter, req *http.Request) {
	deviceCode, userCode, expiresAt, err := s.store.StartGrant(s.cfg.GrantTTL, req.UserAgent())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "internal", "Could not start the sign-in.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deviceCode":              deviceCode,
		"userCode":                store.FormatUserCode(userCode),
		"verificationUri":         s.cfg.PublicURL + "/device",
		"verificationUriComplete": s.cfg.PublicURL + "/device?code=" + userCode,
		"interval":                int(pollInterval / time.Second),
		"expiresIn":               int(time.Until(expiresAt).Seconds()),
	})
}

// pollDevice answers one poll. A finished sign-in spends the grant and mints
// the session token the client keeps; every refusal is an error code the
// kernel names, so a client can tell "wait" from "start over".
func (s *Server) pollDevice(w http.ResponseWriter, req *http.Request) {
	var in struct {
		DeviceCode string `json:"deviceCode"`
	}
	if err := decode(req, &in); err != nil {
		s.fail(w, http.StatusBadRequest, "invalid_json", "Request body must be valid JSON.")
		return
	}
	status, user, err := s.store.PollGrant(in.DeviceCode, pollInterval)
	switch {
	case errors.Is(err, store.ErrPending):
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	case errors.Is(err, store.ErrSlowDown):
		writeJSON(w, http.StatusOK, map[string]any{"status": "slow_down"})
		return
	case errors.Is(err, store.ErrDenied):
		s.fail(w, http.StatusBadRequest, "access_denied", "The sign-in was denied.")
		return
	case errors.Is(err, store.ErrExpired):
		s.fail(w, http.StatusBadRequest, "expired_token", "The sign-in expired.")
		return
	case errors.Is(err, store.ErrNotFound):
		s.fail(w, http.StatusBadRequest, "invalid_grant", "Unknown or already-used device code.")
		return
	case err != nil:
		s.fail(w, http.StatusInternalServerError, "internal", "Could not read the sign-in.")
		return
	}
	if status != "complete" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	}
	token, err := s.store.CreateSession(user.ID, store.KindCLI)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "internal", "Could not mint a session.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":       "complete",
		"sessionToken": token,
		"user":         userWire(user),
	})
}

// approveDevice binds a pending code to the owner. It is the only write on the
// approval surface, so it is the only route the admin token guards besides the
// page itself.
func (s *Server) approveDevice(w http.ResponseWriter, req *http.Request) {
	values, err := fields(req)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "invalid_json", "Request body must be valid JSON.")
		return
	}
	if !s.admin(req, values["token"]) {
		s.fail(w, http.StatusUnauthorized, "unauthorized", "The admin token is missing or wrong.")
		return
	}
	code := values["userCode"]
	if code == "" {
		s.fail(w, http.StatusBadRequest, "missing_user_code", "A user code is required.")
		return
	}
	if err := s.store.ApproveGrant(code, s.owner.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.fail(w, http.StatusNotFound, "grant_not_found", "That code is unknown, used, or expired.")
			return
		}
		s.fail(w, http.StatusInternalServerError, "internal", "Could not approve the code.")
		return
	}
	if wantsHTML(req) {
		http.Redirect(w, req, "/device?done=1", http.StatusSeeOther)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approved": true})
}

// denyDevice ends a pending sign-in so its poller stops waiting.
func (s *Server) denyDevice(w http.ResponseWriter, req *http.Request) {
	values, err := fields(req)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "invalid_json", "Request body must be valid JSON.")
		return
	}
	if !s.admin(req, values["token"]) {
		s.fail(w, http.StatusUnauthorized, "unauthorized", "The admin token is missing or wrong.")
		return
	}
	if err := s.store.DenyGrant(values["userCode"]); err != nil {
		s.fail(w, http.StatusInternalServerError, "internal", "Could not deny the code.")
		return
	}
	if wantsHTML(req) {
		http.Redirect(w, req, "/device?denied=1", http.StatusSeeOther)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"denied": true})
}

// approvalPage is the one screen a person opens. It shows what the code is for
// before it approves anything, and it asks for the admin token on the form
// instead of trusting a cookie: the page is reached from a phone's browser,
// which may share its cookie jar with nobody.
func (s *Server) approvalPage(w http.ResponseWriter, req *http.Request) {
	code := store.NormalizeUserCode(req.URL.Query().Get("code"))
	data := map[string]any{
		"Code":    store.FormatUserCode(code),
		"RawCode": code,
		"Owner":   s.owner.Handle,
		"Done":    req.URL.Query().Get("done") == "1",
		"Denied":  req.URL.Query().Get("denied") == "1",
		"Error":   "",
	}
	if code != "" {
		info, err := s.store.GrantInfo(code)
		switch {
		case err == nil:
			data["Client"] = info.UserAgent
			data["Expires"] = info.ExpiresAt.Format("2006-01-02 15:04:05 UTC")
		case errors.Is(err, store.ErrExpired):
			data["Error"] = "这个码已经过期，请在手机上重新发起登录。"
		default:
			data["Error"] = "没有这个码，请确认手机上显示的字符。"
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.approval.Execute(w, data); err != nil {
		log.Printf("approval page: %v", err)
	}
}

// ---- account ----------------------------------------------------------

// me reports who a session token belongs to.
func (s *Server) me(w http.ResponseWriter, req *http.Request) {
	user, err := s.session(req)
	if err != nil {
		s.fail(w, http.StatusUnauthorized, "account.signed_out", "Not signed in.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": userWire(user)})
}

// logout forgets one session. Ending an already-ended session is not an error:
// the caller only wants the token to stop working.
func (s *Server) logout(w http.ResponseWriter, req *http.Request) {
	if token := bearer(req); token != "" {
		_ = s.store.RevokeSession(token)
	}
	writeJSON(w, http.StatusOK, map[string]any{"signedOut": true})
}

// listDevices is what a controller reads to offer a picker.
func (s *Server) listDevices(w http.ResponseWriter, req *http.Request) {
	user, err := s.session(req)
	if err != nil {
		s.fail(w, http.StatusUnauthorized, "account.signed_out", "Not signed in.")
		return
	}
	devices, err := s.store.Devices(user.ID)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "internal", "Could not read the devices.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
}

// registerDevice is the Studio's call: it posts the X25519 public key it will
// prove itself with and gets back the one credential it may keep.
func (s *Server) registerDevice(w http.ResponseWriter, req *http.Request) {
	user, err := s.session(req)
	if err != nil {
		s.fail(w, http.StatusUnauthorized, "account.signed_out", "Not signed in.")
		return
	}
	var in store.Registration
	if err := decode(req, &in); err != nil {
		s.fail(w, http.StatusBadRequest, "invalid_json", "Request body must be valid JSON.")
		return
	}
	device, credential, err := s.store.RegisterDevice(user.ID, in)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			s.fail(w, http.StatusBadRequest, "bad_request", "A device needs a public key.")
			return
		}
		s.fail(w, http.StatusInternalServerError, "internal", "Could not register the device.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device": device, "deviceCredential": credential})
}

// revokeDevice ends a Studio's registration. The kernel registers again with a
// fresh key on the next start, so this is not a soft flag.
func (s *Server) revokeDevice(w http.ResponseWriter, req *http.Request) {
	user, err := s.session(req)
	if err != nil {
		s.fail(w, http.StatusUnauthorized, "account.signed_out", "Not signed in.")
		return
	}
	if err := s.store.RevokeDevice(user.ID, req.PathValue("id")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.fail(w, http.StatusNotFound, "device_not_found", "No such device.")
			return
		}
		s.fail(w, http.StatusInternalServerError, "internal", "Could not revoke the device.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// grant mints the one-time ticket a controller spends at the relay, and names
// the relay itself so a client never has to know the deployment's address.
func (s *Server) grant(w http.ResponseWriter, req *http.Request) {
	user, err := s.session(req)
	if err != nil {
		s.fail(w, http.StatusUnauthorized, "account.signed_out", "Not signed in.")
		return
	}
	var in struct {
		TargetDeviceID string   `json:"targetDeviceId"`
		Scopes         []string `json:"scopes"`
	}
	if err := decode(req, &in); err != nil {
		s.fail(w, http.StatusBadRequest, "invalid_json", "Request body must be valid JSON.")
		return
	}
	if strings.TrimSpace(in.TargetDeviceID) == "" {
		s.fail(w, http.StatusBadRequest, "missing_device", "A target device is required.")
		return
	}
	device, owner, err := s.store.DeviceByID(in.TargetDeviceID)
	if err != nil || owner != user.ID {
		s.fail(w, http.StatusNotFound, "device_not_found", "No such device on this account.")
		return
	}
	if device.RevokedAt != nil {
		s.fail(w, http.StatusNotFound, "device_not_found", "That device was removed.")
		return
	}
	if !containsScope(device.Capabilities, "desktop") {
		s.fail(w, http.StatusNotFound, "device_not_found", "That device has no remote desktop.")
		return
	}
	scopes := in.Scopes
	if len(scopes) == 0 {
		scopes = []string{"desktop"}
	}
	ticket, err := s.store.IssueTicket(device.ID, user.ID, scopes, s.cfg.TicketTTL)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "internal", "Could not mint a grant.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"grant":  map[string]any{"ticket": ticket},
		"device": device,
		"relay":  s.cfg.RelayURL,
	})
}

// ---- middleware -------------------------------------------------------

// cors answers only the origins the deployment named. The grant endpoint is
// called from the in-app login page, so credentials are allowed, but never to
// an origin that was not configured: reflecting the request's own Origin while
// allowing credentials is the one CORS mistake that hands a page to a stranger.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		origin := strings.TrimRight(req.Header.Get("Origin"), "/")
		if origin != "" && s.originAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "authorization, content-type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Vary", "Origin")
		}
		if req.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, req)
	})
}

func (s *Server) originAllowed(origin string) bool {
	for _, allowed := range s.cfg.AllowedOrigins {
		if strings.EqualFold(strings.TrimRight(allowed, "/"), origin) {
			return true
		}
	}
	return false
}

// logged keeps one line per request without ever printing a token. The path is
// enough to tell a failing deployment from a working one.
func (s *Server) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, req)
		log.Printf("%s %s %s", req.Method, req.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

// ---- helpers ----------------------------------------------------------

// session resolves the Bearer token to the owner. It also accepts the admin
// token, so the command line can call the owner's own endpoints without first
// running a device flow.
func (s *Server) session(req *http.Request) (store.User, error) {
	if token := bearer(req); token != "" {
		if s.cfg.AdminToken != "" && token == s.cfg.AdminToken {
			return s.owner, nil
		}
		return s.store.SessionUser(token)
	}
	if cookie, err := req.Cookie(cookieName); err == nil {
		return s.store.SessionUser(cookie.Value)
	}
	return store.User{}, store.ErrUnauthorized
}

// admin accepts the token three ways: a Bearer header for scripts, the Basic
// auth password for curl, and the form field for the approval page. The page
// has to take it from the body because a phone's browser should not put the
// secret in a URL a screenshot or history could carry.
func (s *Server) admin(req *http.Request, formToken string) bool {
	if token := bearer(req); token != "" && token == s.cfg.AdminToken {
		return true
	}
	if _, password, ok := req.BasicAuth(); ok && password == s.cfg.AdminToken {
		return true
	}
	return formToken != "" && formToken == s.cfg.AdminToken
}

const cookieName = "reasonix_session"

func bearer(req *http.Request) string {
	value := req.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(value) > len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
		return strings.TrimSpace(value[len(prefix):])
	}
	return ""
}

// decode reads a JSON body, or a form when the browser sent one.
func decode(req *http.Request, out any) error {
	contentType := req.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/x-www-form-urlencoded") ||
		strings.HasPrefix(contentType, "multipart/form-data") {
		return req.ParseForm()
	}
	if req.Body == nil {
		return nil
	}
	return json.NewDecoder(req.Body).Decode(out)
}

// fields reads the body as string fields, whichever way it was sent. The two
// approval routes serve both a phone's fetch (JSON) and the HTML page (a form),
// and the admin token arrives in the same body either way.
func fields(req *http.Request) (map[string]string, error) {
	out := map[string]string{}
	contentType := req.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/x-www-form-urlencoded") ||
		strings.HasPrefix(contentType, "multipart/form-data") {
		if err := req.ParseForm(); err != nil {
			return nil, err
		}
		for key := range req.PostForm {
			out[key] = strings.TrimSpace(req.PostFormValue(key))
		}
		return out, nil
	}
	if req.Body == nil {
		return out, nil
	}
	var body map[string]any
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		return nil, err
	}
	for key, value := range body {
		if text, ok := value.(string); ok {
			out[key] = strings.TrimSpace(text)
		}
	}
	return out, nil
}

func wantsHTML(req *http.Request) bool {
	return strings.Contains(req.Header.Get("Accept"), "text/html")
}

func containsScope(scopes []string, want string) bool {
	for _, scope := range scopes {
		if scope == want {
			return true
		}
	}
	return false
}

// userWire is the account shape the kernel and the app already read, without
// the fields a self-hosted service has no opinion about.
func userWire(user store.User) map[string]any {
	return map[string]any{
		"id":            user.ID,
		"handle":        user.Handle,
		"email":         user.Email,
		"emailVerified": false,
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("write response: %v", err)
	}
}

// fail sends the `{error:{code,message}}` envelope the kernel's account client
// reads, with the status the caller should branch on.
func (s *Server) fail(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"code": code, "message": message},
		"code":  code,
	})
}
