// Package config reads the deployment's four decisions from the environment.
//
// Nothing here has a hosted default: a self-hosted service that quietly dialed
// reasonix.io would be the one thing this program exists to avoid.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Config is everything the process needs to serve one account and one relay.
type Config struct {
	// Addr is where the HTTP server listens. TLS is the reverse proxy's job;
	// this program speaks plain HTTP on loopback.
	Addr string
	// DataDir holds the SQLite file and, later, anything else worth keeping.
	DataDir string
	// PublicURL is the accounts origin a browser and the verification links
	// use, e.g. https://id.example.com.
	PublicURL string
	// RelayURL is the origin a controller dials, e.g. https://relay.example.com.
	// The client rewrites it to wss itself.
	RelayURL string
	// AllowedOrigins are the browser origins allowed to call with credentials.
	AllowedOrigins []string
	// OwnerHandle and OwnerEmail name the single account.
	OwnerHandle string
	OwnerEmail  string
	// AdminToken is what the approval page asks for. It is the only human
	// credential a self-hosted deployment needs, because the device flow was
	// designed so the account service never has to hold a password.
	AdminToken string
	// GrantTTL bounds a device code, and TicketTTL bounds a relay ticket.
	GrantTTL  time.Duration
	TicketTTL time.Duration
}

// Load reads the environment and validates what it finds. Every failure names
// the variable, because a service that starts with a half-read config is worse
// than one that refuses to start.
func Load(getenv func(string) string) (Config, error) {
	get := func(name, fallback string) string {
		if getenv == nil {
			return fallback
		}
		if value := strings.TrimSpace(getenv(name)); value != "" {
			return value
		}
		return fallback
	}
	cfg := Config{
		Addr:        get("REASONIX_SELFHOST_ADDR", "127.0.0.1:8790"),
		DataDir:     get("REASONIX_SELFHOST_DATA", "data"),
		PublicURL:   strings.TrimRight(get("REASONIX_SELFHOST_PUBLIC_URL", ""), "/"),
		RelayURL:    strings.TrimRight(get("REASONIX_SELFHOST_RELAY_URL", ""), "/"),
		OwnerHandle: get("REASONIX_SELFHOST_OWNER_HANDLE", "owner"),
		OwnerEmail:  get("REASONIX_SELFHOST_OWNER_EMAIL", ""),
		AdminToken:  get("REASONIX_SELFHOST_ADMIN_TOKEN", ""),
		GrantTTL:    10 * time.Minute,
		TicketTTL:   2 * time.Minute,
	}
	if raw := get("REASONIX_SELFHOST_ALLOWED_ORIGINS", ""); raw != "" {
		for _, origin := range strings.Split(raw, ",") {
			if trimmed := strings.TrimSpace(origin); trimmed != "" {
				cfg.AllowedOrigins = append(cfg.AllowedOrigins, strings.TrimRight(trimmed, "/"))
			}
		}
	}
	if raw := get("REASONIX_SELFHOST_GRANT_TTL", ""); raw != "" {
		ttl, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("REASONIX_SELFHOST_GRANT_TTL: %w", err)
		}
		cfg.GrantTTL = ttl
	}
	if raw := get("REASONIX_SELFHOST_TICKET_TTL", ""); raw != "" {
		ttl, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("REASONIX_SELFHOST_TICKET_TTL: %w", err)
		}
		cfg.TicketTTL = ttl
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	if c.PublicURL == "" {
		return errors.New("REASONIX_SELFHOST_PUBLIC_URL is required (e.g. https://id.example.com)")
	}
	if c.RelayURL == "" {
		return errors.New("REASONIX_SELFHOST_RELAY_URL is required (e.g. https://relay.example.com)")
	}
	for name, raw := range map[string]string{"REASONIX_SELFHOST_PUBLIC_URL": c.PublicURL, "REASONIX_SELFHOST_RELAY_URL": c.RelayURL} {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" {
			return fmt.Errorf("%s must be an absolute URL", name)
		}
		if parsed.Scheme != "https" && parsed.Scheme != "http" {
			return fmt.Errorf("%s must be http or https", name)
		}
		if parsed.Path != "" && parsed.Path != "/" {
			return fmt.Errorf("%s must not carry a path", name)
		}
	}
	if c.AdminToken == "" {
		return errors.New("REASONIX_SELFHOST_ADMIN_TOKEN is required: the approval page has no other way to tell you from a stranger")
	}
	if len(c.AdminToken) < 16 {
		return errors.New("REASONIX_SELFHOST_ADMIN_TOKEN must be at least 16 characters")
	}
	if c.GrantTTL <= 0 || c.TicketTTL <= 0 {
		return errors.New("grant and ticket TTLs must be positive")
	}
	return nil
}

// FilePath is the database file inside DataDir.
func (c Config) FilePath() string { return c.DataDir + string(os.PathSeparator) + "accounts.db" }
