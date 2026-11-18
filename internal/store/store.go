// Package store keeps the self-hosted accounts state: the owner, sessions,
// device sign-in grants, the devices registered against the account, and the
// one-time tickets a relay spays.
//
// Everything the hosted service keeps in D1 lives here in one SQLite file, so a
// single binary plus a data directory is the whole deployment.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Errors the callers tell apart. A device sign-in has four outcomes and each
// one has its own answer on the wire, so they are identities, not sentences.
var (
	ErrNotFound     = errors.New("store: not found")
	ErrExpired      = errors.New("store: expired")
	ErrDenied       = errors.New("store: denied")
	ErrPending      = errors.New("store: pending")
	ErrSlowDown     = errors.New("store: slow down")
	ErrUnauthorized = errors.New("store: unauthorized")
	ErrConflict     = errors.New("store: conflict")
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	handle     TEXT NOT NULL UNIQUE,
	email      TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	user_id    INTEGER NOT NULL,
	kind       TEXT NOT NULL,
	created_at TEXT NOT NULL,
	revoked_at TEXT
);

CREATE TABLE IF NOT EXISTS device_grants (
	device_hash  TEXT PRIMARY KEY,
	user_code    TEXT NOT NULL UNIQUE,
	state        TEXT NOT NULL DEFAULT 'pending',
	user_id      INTEGER,
	user_agent   TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL,
	expires_at   TEXT NOT NULL,
	last_poll_at TEXT
);

CREATE TABLE IF NOT EXISTS devices (
	id              TEXT PRIMARY KEY,
	user_id         INTEGER NOT NULL,
	name            TEXT NOT NULL,
	platform        TEXT NOT NULL,
	public_key      TEXT NOT NULL,
	capabilities    TEXT NOT NULL,
	credential_hash TEXT NOT NULL UNIQUE,
	gateway_id      TEXT NOT NULL DEFAULT '',
	created_at      TEXT NOT NULL,
	last_seen_at    TEXT,
	revoked_at      TEXT
);

CREATE TABLE IF NOT EXISTS tickets (
	ticket_hash TEXT PRIMARY KEY,
	device_id   TEXT NOT NULL,
	user_id     INTEGER NOT NULL,
	scopes      TEXT NOT NULL,
	created_at  TEXT NOT NULL,
	expires_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS devices_by_user ON devices (user_id);
CREATE INDEX IF NOT EXISTS grants_by_expiry ON device_grants (expires_at);
CREATE INDEX IF NOT EXISTS tickets_by_expiry ON tickets (expires_at);

-- A controller is a browser or a phone that reached a Studio through the relay.
-- The hosted service kept one row per enrolled controller so a desktop could
-- list them, answer a pending one, revoke one, and tell the enrolled ones that
-- something is waiting. A self-hosted relay carried the sockets but kept no
-- record, which is why /host/controllers and /host/push/notify had nothing to
-- read. The row outlives the socket: a phone in a pocket is exactly the case
-- the notification exists for.
CREATE TABLE IF NOT EXISTS controllers (
	id              TEXT PRIMARY KEY,
	device_id       TEXT NOT NULL,
	user_id         INTEGER NOT NULL,
	name            TEXT NOT NULL DEFAULT '',
	user_agent      TEXT NOT NULL DEFAULT '',
	state           TEXT NOT NULL DEFAULT 'active',
	created_at      TEXT NOT NULL,
	last_seen_at    TEXT,
	revoked_at      TEXT,
	replace_id      TEXT,
	last_kind       TEXT,
	last_count      INTEGER NOT NULL DEFAULT 0,
	notified_at     TEXT,
	FOREIGN KEY (device_id) REFERENCES devices (id)
);

-- The filters this table is actually read with:
--   Controllers(userID, deviceID, state) — state last, because "all" drops it.
--   controllerByDeviceAndAgent       — and revoked_at, which it also filters.
CREATE INDEX IF NOT EXISTS controllers_by_device_state ON controllers (user_id, device_id, state);
CREATE INDEX IF NOT EXISTS controllers_by_phone ON controllers (device_id, user_agent, revoked_at);
`

// Store is the one handle on the database. One connection is deliberate: the
// write rate of a personal relay is a handful of rows a minute, and SQLite's
// single-writer rule costs less than the retry loop a pool would need.
type Store struct {
	db *sql.DB
}

// Open creates the schema when it is missing and returns the handle.
func Open(path string) (*Store, error) {
	dsn := path
	if !strings.HasPrefix(dsn, "file:") {
		dsn = "file:" + dsn
	}
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	dsn += separator + "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// ---- users -----------------------------------------------------------

// User is the person the account belongs to.
type User struct {
	ID     int64  `json:"id"`
	Handle string `json:"handle"`
	Email  string `json:"email"`
}

// EnsureOwner creates the account named handle when none exists, and returns
// whichever account is there. A self-hosted deployment has one person; the
// table is not a multi-tenant feature.
func (s *Store) EnsureOwner(handle, email string) (User, error) {
	handle = strings.TrimSpace(handle)
	if handle == "" {
		handle = "owner"
	}
	var user User
	err := s.db.QueryRow(`SELECT id, handle, email FROM users ORDER BY id LIMIT 1`).Scan(&user.ID, &user.Handle, &user.Email)
	if err == nil {
		return user, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return User{}, err
	}
	res, err := s.db.Exec(
		`INSERT INTO users (handle, email, created_at) VALUES (?, ?, ?)`,
		handle, strings.TrimSpace(email), now(),
	)
	if err != nil {
		return User{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, err
	}
	return User{ID: id, Handle: handle, Email: strings.TrimSpace(email)}, nil
}

// UserByID reads one account.
func (s *Store) UserByID(id int64) (User, error) {
	var user User
	err := s.db.QueryRow(`SELECT id, handle, email FROM users WHERE id = ?`, id).Scan(&user.ID, &user.Handle, &user.Email)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return user, err
}

// ---- sessions --------------------------------------------------------

// SessionKind tells a browser session from one a device sign-in minted. The
// token is the only credential either surface holds; the database keeps its
// hash so reading the file cannot resurrect a live session.
const (
	KindWeb = "web"
	KindCLI = "cli"
)

// CreateSession mints a token and stores only its hash.
func (s *Store) CreateSession(userID int64, kind string) (string, error) {
	if kind == "" {
		kind = KindCLI
	}
	token, err := randomToken(32)
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec(
		`INSERT INTO sessions (token_hash, user_id, kind, created_at) VALUES (?, ?, ?, ?)`,
		hashCode(token), userID, kind, now(),
	)
	if err != nil {
		return "", err
	}
	return token, nil
}

// SessionUser resolves a token to its account, or ErrUnauthorized.
func (s *Store) SessionUser(token string) (User, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return User{}, ErrUnauthorized
	}
	var user User
	err := s.db.QueryRow(`
		SELECT u.id, u.handle, u.email
		  FROM sessions s JOIN users u ON u.id = s.user_id
		 WHERE s.token_hash = ? AND s.revoked_at IS NULL`,
		hashCode(token),
	).Scan(&user.ID, &user.Handle, &user.Email)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrUnauthorized
	}
	return user, err
}

// RevokeSession deletes one session. A token the server already forgot is not
// an error: the caller's goal is for it to stop working.
func (s *Store) RevokeSession(token string) error {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, hashCode(token))
	return err
}

// ---- device sign-in (RFC 8628 shaped) --------------------------------

// GrantInfo is what an approval screen may show about a pending code.
type GrantInfo struct {
	UserCode  string
	UserAgent string
	CreatedAt time.Time
	ExpiresAt time.Time
	State     string
}

// StartGrant opens a sign-in: the client polls with DeviceCode, the person
// approves the short UserCode.
func (s *Store) StartGrant(ttl time.Duration, userAgent string) (deviceCode, userCode string, expiresAt time.Time, err error) {
	expiresAt = time.Now().UTC().Add(ttl)
	for attempt := 0; attempt < 8; attempt++ {
		deviceCode, err = randomToken(32)
		if err != nil {
			return "", "", time.Time{}, err
		}
		userCode, err = randomUserCode(8)
		if err != nil {
			return "", "", time.Time{}, err
		}
		_, err = s.db.Exec(
			`INSERT INTO device_grants (device_hash, user_code, user_agent, created_at, expires_at)
			 VALUES (?, ?, ?, ?, ?)`,
			hashCode(deviceCode), userCode, strings.TrimSpace(userAgent), now(), expiresAt.Format(time.RFC3339Nano),
		)
		if err == nil {
			return deviceCode, userCode, expiresAt, nil
		}
		if !strings.Contains(err.Error(), "UNIQUE") {
			return "", "", time.Time{}, err
		}
	}
	return "", "", time.Time{}, ErrConflict
}

// PollGrant answers one poll. It returns ErrPending while the person has not
// decided, ErrSlowDown when the client asked too soon, ErrDenied, ErrExpired,
// ErrNotFound for an unknown or already-spent code, and the account on
// success. A successful poll spends the grant, which is what makes the answer
// single-use.
func (s *Store) PollGrant(deviceCode string, interval time.Duration) (status string, user User, err error) {
	hash := hashCode(strings.TrimSpace(deviceCode))
	if strings.TrimSpace(deviceCode) == "" {
		return "", User{}, ErrNotFound
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", User{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		state    string
		userID   sql.NullInt64
		expires  string
		lastPoll sql.NullString
	)
	err = tx.QueryRow(
		`SELECT state, user_id, expires_at, last_poll_at FROM device_grants WHERE device_hash = ?`, hash,
	).Scan(&state, &userID, &expires, &lastPoll)
	if errors.Is(err, sql.ErrNoRows) {
		return "", User{}, ErrNotFound
	}
	if err != nil {
		return "", User{}, err
	}
	expiry, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil {
		return "", User{}, err
	}
	nowAt := time.Now().UTC()
	if nowAt.After(expiry) {
		_, _ = tx.Exec(`DELETE FROM device_grants WHERE device_hash = ?`, hash)
		_ = tx.Commit()
		return "", User{}, ErrExpired
	}
	switch state {
	case "denied":
		_, _ = tx.Exec(`DELETE FROM device_grants WHERE device_hash = ?`, hash)
		_ = tx.Commit()
		return "", User{}, ErrDenied
	case "approved":
		if !userID.Valid {
			return "", User{}, ErrNotFound
		}
		_, _ = tx.Exec(`DELETE FROM device_grants WHERE device_hash = ?`, hash)
		claimed := userID.Int64
		if err := tx.Commit(); err != nil {
			return "", User{}, err
		}
		account, err := s.UserByID(claimed)
		if err != nil {
			return "", User{}, err
		}
		return "complete", account, nil
	}
	if lastPoll.Valid {
		if at, perr := time.Parse(time.RFC3339Nano, lastPoll.String); perr == nil && nowAt.Sub(at) < interval {
			return "", User{}, ErrSlowDown
		}
	}
	if _, err := tx.Exec(`UPDATE device_grants SET last_poll_at = ? WHERE device_hash = ?`, nowAt.Format(time.RFC3339Nano), hash); err != nil {
		return "", User{}, err
	}
	return "", User{}, ErrPending
}

// ApproveGrant binds a pending user code to the account that accepted it.
func (s *Store) ApproveGrant(userCode string, userID int64) error {
	code := NormalizeUserCode(userCode)
	if code == "" {
		return ErrNotFound
	}
	res, err := s.db.Exec(`
		UPDATE device_grants
		   SET state = 'approved', user_id = ?
		 WHERE user_code = ? AND state = 'pending' AND expires_at > ?`,
		userID, code, now())
	if err != nil {
		return err
	}
	changed, _ := res.RowsAffected()
	if changed == 0 {
		return ErrNotFound
	}
	return nil
}

// DenyGrant rejects a pending code so its poller stops waiting.
func (s *Store) DenyGrant(userCode string) error {
	code := NormalizeUserCode(userCode)
	if code == "" {
		return nil
	}
	_, err := s.db.Exec(
		`UPDATE device_grants SET state = 'denied' WHERE user_code = ? AND state = 'pending'`, code,
	)
	return err
}

// GrantInfo describes a pending code for the approval screen.
func (s *Store) GrantInfo(userCode string) (GrantInfo, error) {
	code := NormalizeUserCode(userCode)
	if code == "" {
		return GrantInfo{}, ErrNotFound
	}
	var (
		info      GrantInfo
		created   string
		expires   string
		state     string
		userAgent string
	)
	err := s.db.QueryRow(
		`SELECT created_at, expires_at, state, user_agent FROM device_grants WHERE user_code = ?`, code,
	).Scan(&created, &expires, &state, &userAgent)
	if errors.Is(err, sql.ErrNoRows) {
		return GrantInfo{}, ErrNotFound
	}
	if err != nil {
		return GrantInfo{}, err
	}
	info.UserCode = code
	info.State = state
	info.UserAgent = userAgent
	if at, perr := time.Parse(time.RFC3339Nano, created); perr == nil {
		info.CreatedAt = at
	}
	if at, perr := time.Parse(time.RFC3339Nano, expires); perr == nil {
		info.ExpiresAt = at
	}
	if !info.ExpiresAt.IsZero() && time.Now().UTC().After(info.ExpiresAt) {
		return GrantInfo{}, ErrExpired
	}
	return info, nil
}

// ---- devices ---------------------------------------------------------

// Device is a Studio registered against the account. PublicKey is the X25519
// key a controller seals its channel to; the credential that proves the device
// is only ever stored hashed.
type Device struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Platform     string   `json:"platform"`
	PublicKey    string   `json:"publicKey"`
	Capabilities []string `json:"capabilities"`
	GatewayID    string   `json:"gatewayId,omitempty"`
	LastSeenAt   *string  `json:"lastSeenAt"`
	RevokedAt    *string  `json:"revokedAt"`
}

// Registration is what a Studio posts to /me/devices.
type Registration struct {
	Name         string   `json:"name"`
	Platform     string   `json:"platform"`
	PublicKey    string   `json:"publicKey"`
	Capabilities []string `json:"capabilities"`
	GatewayID    string   `json:"gatewayId,omitempty"`
}

// RegisterDevice stores a new device and returns the one credential it will
// ever see. The caller must keep it: only its hash is written.
func (s *Store) RegisterDevice(userID int64, in Registration) (Device, string, error) {
	if strings.TrimSpace(in.PublicKey) == "" {
		return Device{}, "", ErrConflict
	}
	id, err := randomHex(16)
	if err != nil {
		return Device{}, "", err
	}
	credential, err := randomToken(32)
	if err != nil {
		return Device{}, "", err
	}
	caps, err := json.Marshal(defaultCapabilities(in.Capabilities))
	if err != nil {
		return Device{}, "", err
	}
	stamp := now()
	_, err = s.db.Exec(`
		INSERT INTO devices (id, user_id, name, platform, public_key, capabilities, credential_hash, gateway_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, userID, strings.TrimSpace(in.Name), strings.TrimSpace(in.Platform), in.PublicKey,
		string(caps), hashCode(credential), strings.TrimSpace(in.GatewayID), stamp,
	)
	if err != nil {
		return Device{}, "", err
	}
	device := Device{
		ID: id, Name: strings.TrimSpace(in.Name), Platform: strings.TrimSpace(in.Platform),
		PublicKey: in.PublicKey, Capabilities: defaultCapabilities(in.Capabilities),
		GatewayID: strings.TrimSpace(in.GatewayID),
	}
	return device, credential, nil
}

// Devices lists the account's devices, revoked ones included: a controller
// that names a removed device gets told why instead of a bare 404.
func (s *Store) Devices(userID int64) ([]Device, error) {
	rows, err := s.db.Query(`
		SELECT id, name, platform, public_key, capabilities, gateway_id, last_seen_at, revoked_at
		  FROM devices WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		device, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, device)
	}
	return out, rows.Err()
}

// DeviceByID reads one device and the account it belongs to.
func (s *Store) DeviceByID(id string) (Device, int64, error) {
	row := s.db.QueryRow(`
		SELECT id, name, platform, public_key, capabilities, gateway_id, last_seen_at, revoked_at, user_id
		  FROM devices WHERE id = ?`, strings.TrimSpace(id))
	return scanDeviceWithOwner(row)
}

// DeviceByCredential resolves the credential a Studio dials the relay with.
func (s *Store) DeviceByCredential(credential string) (Device, error) {
	if strings.TrimSpace(credential) == "" {
		return Device{}, ErrUnauthorized
	}
	row := s.db.QueryRow(`
		SELECT id, name, platform, public_key, capabilities, gateway_id, last_seen_at, revoked_at
		  FROM devices WHERE credential_hash = ?`, hashCode(credential))
	device, err := scanDevice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrUnauthorized
	}
	if err != nil {
		return Device{}, err
	}
	if device.RevokedAt != nil {
		return Device{}, ErrUnauthorized
	}
	return device, nil
}

// DeviceByCredentialWithOwner is DeviceByCredential plus the account the device
// belongs to. The host routes need both, and reading the owner in the same
// statement keeps a store failure a store failure instead of flattening it into
// a 401 further up.
func (s *Store) DeviceByCredentialWithOwner(credential string) (Device, int64, error) {
	if strings.TrimSpace(credential) == "" {
		return Device{}, 0, ErrUnauthorized
	}
	row := s.db.QueryRow(`
		SELECT id, name, platform, public_key, capabilities, gateway_id, last_seen_at, revoked_at, user_id
		  FROM devices WHERE credential_hash = ?`, hashCode(credential))
	device, userID, err := scanDeviceWithOwner(row)
	if errors.Is(err, ErrNotFound) {
		return Device{}, 0, ErrUnauthorized
	}
	if err != nil {
		return Device{}, 0, err
	}
	if device.RevokedAt != nil {
		return Device{}, 0, ErrUnauthorized
	}
	return device, userID, nil
}

// TouchDevice records that a device's relay link came up.
func (s *Store) TouchDevice(id string) {
	_, _ = s.db.Exec(`UPDATE devices SET last_seen_at = ? WHERE id = ?`, now(), id)
}

// RevokeDevice ends a device. It cannot be undone: the Studio registers again
// with a fresh key.
func (s *Store) RevokeDevice(userID int64, id string) error {
	res, err := s.db.Exec(
		`UPDATE devices SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
		now(), strings.TrimSpace(id), userID,
	)
	if err != nil {
		return err
	}
	changed, _ := res.RowsAffected()
	if changed == 0 {
		return ErrNotFound
	}
	return nil
}

func scanDevice(rows interface{ Scan(...any) error }) (Device, error) {
	var (
		device    Device
		caps      string
		lastSeen  sql.NullString
		revokedAt sql.NullString
	)
	err := rows.Scan(&device.ID, &device.Name, &device.Platform, &device.PublicKey, &caps,
		&device.GatewayID, &lastSeen, &revokedAt)
	if err != nil {
		return Device{}, err
	}
	fillDevice(&device, caps, lastSeen, revokedAt)
	return device, nil
}

func scanDeviceWithOwner(row interface{ Scan(...any) error }) (Device, int64, error) {
	var (
		device    Device
		caps      string
		lastSeen  sql.NullString
		revokedAt sql.NullString
		userID    int64
	)
	err := row.Scan(&device.ID, &device.Name, &device.Platform, &device.PublicKey, &caps,
		&device.GatewayID, &lastSeen, &revokedAt, &userID)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, 0, ErrNotFound
	}
	if err != nil {
		return Device{}, 0, err
	}
	fillDevice(&device, caps, lastSeen, revokedAt)
	return device, userID, nil
}

func fillDevice(device *Device, caps string, lastSeen, revokedAt sql.NullString) {
	device.Capabilities = []string{}
	if caps != "" {
		_ = json.Unmarshal([]byte(caps), &device.Capabilities)
	}
	if lastSeen.Valid {
		value := lastSeen.String
		device.LastSeenAt = &value
	}
	if revokedAt.Valid {
		value := revokedAt.String
		device.RevokedAt = &value
	}
}

func defaultCapabilities(given []string) []string {
	if len(given) > 0 {
		return given
	}
	return []string{"tasks", "logs", "files", "desktop"}
}

// ---- tickets ---------------------------------------------------------

// Ticket is what a controller's relay socket is authorised by once.
type Ticket struct {
	DeviceID string
	UserID   int64
	Scopes   []string
}

// IssueTicket mints a short-lived, single-use grant.
func (s *Store) IssueTicket(deviceID string, userID int64, scopes []string, ttl time.Duration) (string, error) {
	ticket, err := randomToken(16)
	if err != nil {
		return "", err
	}
	if len(scopes) == 0 {
		scopes = []string{"desktop"}
	}
	encoded, err := json.Marshal(scopes)
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec(`
		INSERT INTO tickets (ticket_hash, device_id, user_id, scopes, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		hashCode(ticket), deviceID, userID, string(encoded), now(),
		time.Now().UTC().Add(ttl).Format(time.RFC3339Nano),
	)
	if err != nil {
		return "", err
	}
	return ticket, nil
}

// SpendTicket consumes a ticket. A ticket is single-use by construction: the
// row is deleted in the same statement that reads it, so a relay restart or a
// replayed subprotocol cannot open a second socket with it.
func (s *Store) SpendTicket(ticket string) (Ticket, error) {
	hash := hashCode(strings.TrimSpace(ticket))
	if strings.TrimSpace(ticket) == "" {
		return Ticket{}, ErrNotFound
	}
	var (
		out     Ticket
		scopes  string
		expires string
	)
	err := s.db.QueryRow(`
		DELETE FROM tickets WHERE ticket_hash = ?
		RETURNING device_id, user_id, scopes, expires_at`, hash,
	).Scan(&out.DeviceID, &out.UserID, &scopes, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Ticket{}, ErrNotFound
	}
	if err != nil {
		return Ticket{}, err
	}
	if at, perr := time.Parse(time.RFC3339Nano, expires); perr == nil && time.Now().UTC().After(at) {
		return Ticket{}, ErrExpired
	}
	_ = json.Unmarshal([]byte(scopes), &out.Scopes)
	if len(out.Scopes) == 0 {
		out.Scopes = []string{"desktop"}
	}
	return out, nil
}

// Prune drops rows nothing can use any more.
func (s *Store) Prune() error {
	cutoff := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`DELETE FROM device_grants WHERE expires_at < ?`, cutoff); err != nil {
		return err
	}
	// A revoked controller is history, not state: nothing reads it once it is
	// over, and leaving it makes every reconnect look like a new phone in the
	// table it no longer shows up in.
	_, err := s.db.Exec(`DELETE FROM controllers WHERE state = ? AND revoked_at IS NOT NULL AND revoked_at < ?`,
		ControllerRevoked, cutoff)
	return err
}

// ---- controllers -----------------------------------------------------

// Controller is one browser or phone that reached a Studio through the relay,
// as that Studio sees it. It outlives the socket: a phone in a pocket is the
// case the notification exists for, and there is no connection to ask then.
type Controller struct {
	ID         string
	DeviceID   string
	UserID     int64
	Name       string
	UserAgent  string
	State      string
	CreatedAt  string
	LastSeenAt *string
	RevokedAt  *string
	ReplaceID  string
}

// Controller states. A controller that has never been answered is pending;
// one the owner added is active; one the owner ended is revoked.
const (
	ControllerPending = "pending"
	ControllerActive  = "active"
	ControllerRevoked = "revoked"
)

// EnrollController records a controller that just authenticated at the relay.
// The first sight of a controller makes it active: a self-hosted deployment has
// one owner who just minted its ticket, which is the same approval the hosted
// service asked for with an admin code. A row that already exists is only
// refreshed, so repeated connections do not multiply the list.
func (s *Store) EnrollController(deviceID string, userID int64, userAgent string) (Controller, error) {
	userAgent = strings.TrimSpace(userAgent)
	existing, found, err := s.controllerByDeviceAndAgent(deviceID, userAgent)
	if err != nil {
		return Controller{}, err
	}
	if found {
		stamp := now()
		if _, err := s.db.Exec(`UPDATE controllers SET last_seen_at = ? WHERE id = ?`, stamp, existing.ID); err != nil {
			return Controller{}, err
		}
		existing.LastSeenAt = &stamp
		return existing, nil
	}
	id, err := randomHex(16)
	if err != nil {
		return Controller{}, err
	}
	stamp := now()
	_, err = s.db.Exec(`
		INSERT INTO controllers (id, device_id, user_id, name, user_agent, state, created_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, deviceID, userID, "", userAgent, ControllerActive, stamp, stamp,
	)
	if err != nil {
		return Controller{}, err
	}
	return Controller{
		ID: id, DeviceID: deviceID, UserID: userID, UserAgent: userAgent,
		State: ControllerActive, CreatedAt: stamp, LastSeenAt: &stamp,
	}, nil
}

// RemoveUnseenController drops the row a dial enrolled for a device that went
// offline between the first read and the upgrade. Without it a phone that never
// got a socket stays counted in NotifyControllers, and the row that follows the
// reconnect is a second one for the same phone.
func (s *Store) RemoveUnseenController(deviceID, userAgent string) {
	_, _ = s.db.Exec(`DELETE FROM controllers WHERE device_id = ? AND user_agent = ?`,
		deviceID, strings.TrimSpace(userAgent))
}

// controllerByDeviceAndAgent finds the row one phone produced before, so a
// reconnect refreshes it instead of enrolling the same phone again.
func (s *Store) controllerByDeviceAndAgent(deviceID, userAgent string) (Controller, bool, error) {
	row := s.db.QueryRow(`
		SELECT id, device_id, user_id, name, user_agent, state, created_at, last_seen_at, revoked_at, replace_id
		  FROM controllers WHERE device_id = ? AND user_agent = ? AND revoked_at IS NULL
		 ORDER BY created_at LIMIT 1`, deviceID, userAgent)
	out, err := scanController(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Controller{}, false, nil
	}
	if err != nil {
		return Controller{}, false, err
	}
	return out, true, nil
}

// Controllers lists the controllers enrolled against one device, in the given
// state ("all" or empty for every one).
func (s *Store) Controllers(userID int64, deviceID, state string) ([]Controller, error) {
	query := `SELECT id, device_id, user_id, name, user_agent, state, created_at, last_seen_at, revoked_at, replace_id
	            FROM controllers WHERE user_id = ? AND device_id = ?`
	args := []any{userID, deviceID}
	switch strings.TrimSpace(state) {
	case "", "all":
	case ControllerPending, ControllerActive, ControllerRevoked:
		query += ` AND state = ?`
		args = append(args, strings.TrimSpace(state))
	default:
		return nil, ErrNotFound
	}
	query += ` ORDER BY created_at`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Controller{}
	for rows.Next() {
		controller, err := scanController(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, controller)
	}
	return out, rows.Err()
}

// ControllerByID reads one controller of one device.
func (s *Store) ControllerByID(userID int64, deviceID, id string) (Controller, error) {
	row := s.db.QueryRow(`
		SELECT id, device_id, user_id, name, user_agent, state, created_at, last_seen_at, revoked_at, replace_id
		  FROM controllers WHERE user_id = ? AND device_id = ? AND id = ?`,
		userID, deviceID, strings.TrimSpace(id))
	out, err := scanController(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Controller{}, ErrNotFound
	}
	return out, err
}

// SetControllerState moves one controller to a state the owner chose. Replacing
// names the active controller that gives way, and that one is revoked in the
// same transaction: two active controllers for one phone would both be told
// they are connected.
func (s *Store) SetControllerState(userID int64, deviceID, id, state, replaceID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stamp := now()
	if err := tx.QueryRow(`SELECT 1 FROM controllers WHERE user_id = ? AND device_id = ? AND id = ?`,
		userID, deviceID, strings.TrimSpace(id)).Scan(new(int)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	// Two shapes because a state change either sets or clears the revocation
	// stamp; a mistake here is a controller that cannot be ended.
	var execErr error
	if state == ControllerRevoked {
		_, execErr = tx.Exec(`UPDATE controllers SET state = ?, revoked_at = ?, notified_at = NULL, last_kind = NULL, last_count = 0
		                        WHERE user_id = ? AND device_id = ? AND id = ?`,
			state, stamp, userID, deviceID, strings.TrimSpace(id))
	} else {
		_, execErr = tx.Exec(`UPDATE controllers SET state = ?, revoked_at = NULL, notified_at = NULL, last_kind = NULL, last_count = 0
		                        WHERE user_id = ? AND device_id = ? AND id = ?`,
			state, userID, deviceID, strings.TrimSpace(id))
	}
	if execErr != nil {
		return execErr
	}
	if state == ControllerActive && strings.TrimSpace(replaceID) != "" {
		if _, err := tx.Exec(`UPDATE controllers SET state = ?, revoked_at = ? WHERE user_id = ? AND device_id = ? AND id = ?`,
			ControllerRevoked, stamp, userID, deviceID, strings.TrimSpace(replaceID)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// NotifyControllers records one report against every active controller of a
// device, and answers how many took it.
//
// The report is a kind and a count — the three occasions the kernel names — so
// nothing about a conversation reaches this table. An active controller that was
// already told the same kind keeps the larger count instead of queueing a second
// message: a phone that read one "3 waiting" does not need "1 waiting" next.
func (s *Store) NotifyControllers(userID int64, deviceID, kind string, count int) (delivered int, err error) {
	rows, err := s.db.Query(`
		SELECT id FROM controllers
		 WHERE user_id = ? AND device_id = ? AND state = ? AND revoked_at IS NULL`,
		userID, deviceID, ControllerActive)
	if err != nil {
		return 0, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	stamp := now()
	// One statement per controller, inside one transaction. Two separate
	// updates meant a report that changed the kind could be interleaved with
	// the opposite order and lose one of them; the CASE does the merge the two
	// statements were trying to express, atomically.
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		if _, err := tx.Exec(`
			UPDATE controllers
			   SET last_kind  = ?,
			       last_count = CASE WHEN last_kind = ? THEN MAX(last_count, ?) ELSE ? END,
			       notified_at = ?
			 WHERE id = ?`,
			kind, kind, count, count, stamp, id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// ControllerReport is one controller's pending notification, or nil when it has
// none. The app reads this instead of holding a socket open for a push: a
// self-hosted deployment has no push service, and polling what is waiting is
// the shape that needs none.
type ControllerReport struct {
	Kind      string
	Count     int
	NotifiedAt string
}

// PendingReport reads what is waiting for one active controller.
func (s *Store) PendingReport(userID int64, deviceID, id string) (*ControllerReport, error) {
	var (
		kind     sql.NullString
		count    int
		notified sql.NullString
	)
	err := s.db.QueryRow(`
		SELECT last_kind, last_count, notified_at FROM controllers
		 WHERE user_id = ? AND device_id = ? AND id = ? AND state = ? AND revoked_at IS NULL`,
		userID, deviceID, strings.TrimSpace(id), ControllerActive).Scan(&kind, &count, &notified)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !kind.Valid || !notified.Valid || count <= 0 {
		return nil, nil
	}
	return &ControllerReport{Kind: kind.String, Count: count, NotifiedAt: notified.String}, nil
}

// ClearReport drops a controller's pending notification once it has been read.
func (s *Store) ClearReport(userID int64, deviceID, id string) error {
	_, err := s.db.Exec(`
		UPDATE controllers SET last_kind = NULL, last_count = 0, notified_at = NULL
		 WHERE user_id = ? AND device_id = ? AND id = ?`, userID, deviceID, strings.TrimSpace(id))
	return err
}

func scanController(rows interface{ Scan(...any) error }) (Controller, error) {
	var (
		out       Controller
		lastSeen  sql.NullString
		revokedAt sql.NullString
		replace   sql.NullString
	)
	err := rows.Scan(&out.ID, &out.DeviceID, &out.UserID, &out.Name, &out.UserAgent, &out.State,
		&out.CreatedAt, &lastSeen, &revokedAt, &replace)
	if err != nil {
		return Controller{}, err
	}
	if lastSeen.Valid {
		out.LastSeenAt = &lastSeen.String
	}
	if revokedAt.Valid {
		out.RevokedAt = &revokedAt.String
	}
	if replace.Valid {
		out.ReplaceID = replace.String
	}
	return out, nil
}

// ---- helpers ---------------------------------------------------------

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func hashCode(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// randomHex is 32 lowercase hex characters for size 16. Device ids use it
// because they end up in a URL path and in a QR code.
func randomHex(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func randomToken(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// userCodeAlphabet drops 0/O/1/I so a code read off one screen and typed into
// another cannot go wrong on a glyph.
const userCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func randomUserCode(length int) (string, error) {
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	out := make([]byte, length)
	for index := range out {
		out[index] = userCodeAlphabet[int(raw[index])%len(userCodeAlphabet)]
	}
	return string(out), nil
}

// NormalizeUserCode accepts what a person types: separators and case are not
// part of the identity.
func NormalizeUserCode(raw string) string {
	var builder strings.Builder
	for _, char := range strings.ToUpper(strings.TrimSpace(raw)) {
		if (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') {
			builder.WriteRune(char)
		}
	}
	return builder.String()
}

// FormatUserCode presents a canonical code as two hyphen-separated groups.
func FormatUserCode(code string) string {
	if len(code) <= 4 {
		return code
	}
	return code[:len(code)/2] + "-" + code[len(code)/2:]
}
