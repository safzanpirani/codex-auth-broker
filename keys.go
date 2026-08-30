package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	roleAdmin  = "admin"
	roleClient = "client"
	// implicitClientName is the name attributed to the legacy single
	// --api-key / CODEX_AUTH_BROKER_API_KEY. It keeps role admin so existing
	// single-key setups retain dashboard access unchanged.
	implicitClientName = "default"
	// keyRegistryStatInterval bounds how often the registry stats the keys
	// file. Auth checks between stats reuse the cached entries, so rotation
	// takes effect within this interval without a restart and without a
	// per-request stat.
	keyRegistryStatInterval = 2 * time.Second
)

// clientKey is one entry of the --keys-file JSON array.
type clientKey struct {
	Name     string `json:"name"`
	Key      string `json:"key"`
	Role     string `json:"role"`
	Disabled bool   `json:"disabled,omitempty"`
}

// clientIdentity is the resolved caller of an authenticated request.
type clientIdentity struct {
	Name string
	Role string
}

func (id clientIdentity) admin() bool { return id.Role == roleAdmin }

// keyRegistry resolves bearer tokens to named clients. It merges the implicit
// single --api-key (as client "default", role admin) with the entries of an
// optional --keys-file, which is reloaded when its mtime or size changes.
type keyRegistry struct {
	implicit *clientKey

	mu      sync.Mutex
	path    string
	entries []clientKey
	mtime   time.Time
	size    int64
	checked time.Time
	// statInterval is keyRegistryStatInterval; overridable in tests.
	statInterval time.Duration
}

// newKeyRegistry builds the registry from the legacy single key and/or a keys
// file. Both empty means auth is disabled. A keys file that cannot be read or
// parsed at startup is a hard error; later reload failures keep the previous
// entries.
func newKeyRegistry(apiKey, keysFile string) (*keyRegistry, error) {
	reg := &keyRegistry{
		implicit:     implicitClientKey(apiKey),
		path:         strings.TrimSpace(keysFile),
		statInterval: keyRegistryStatInterval,
	}
	if reg.path == "" {
		return reg, nil
	}
	raw, stat, err := readPrivateFile(reg.path)
	if err != nil {
		return nil, fmt.Errorf("keys file: %w", err)
	}
	entries, err := parseKeysFile(raw)
	if err != nil {
		return nil, fmt.Errorf("keys file %s: %w", reg.path, err)
	}
	if err := rejectImplicitKeyCollision(reg.implicit, entries); err != nil {
		return nil, fmt.Errorf("keys file %s: %w", reg.path, err)
	}
	reg.entries = entries
	reg.mtime = stat.ModTime()
	reg.size = stat.Size()
	reg.checked = time.Now()
	return reg, nil
}

func implicitClientKey(apiKey string) *clientKey {
	key := strings.TrimSpace(apiKey)
	if key == "" {
		return nil
	}
	return &clientKey{Name: implicitClientName, Key: key, Role: roleAdmin}
}

// parseKeysFile validates the JSON array shape of a keys file. Role defaults
// to "client" when omitted; names must be unique so log lines and usage rows
// attribute unambiguously.
func parseKeysFile(raw []byte) ([]clientKey, error) {
	var entries []clientKey
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	seen := map[string]bool{}
	seenKeys := map[string]string{}
	for i := range entries {
		entries[i].Name = strings.TrimSpace(entries[i].Name)
		entries[i].Key = strings.TrimSpace(entries[i].Key)
		entries[i].Role = strings.ToLower(strings.TrimSpace(entries[i].Role))
		if entries[i].Name == "" {
			return nil, fmt.Errorf("entry %d: name is required", i)
		}
		if strings.TrimSpace(entries[i].Key) == "" {
			return nil, fmt.Errorf("entry %d (%s): key is required", i, entries[i].Name)
		}
		if entries[i].Role == "" {
			entries[i].Role = roleClient
		}
		if entries[i].Role != roleAdmin && entries[i].Role != roleClient {
			return nil, fmt.Errorf("entry %d (%s): role must be %q or %q", i, entries[i].Name, roleAdmin, roleClient)
		}
		if seen[entries[i].Name] {
			return nil, fmt.Errorf("duplicate client name %q", entries[i].Name)
		}
		if previous := seenKeys[entries[i].Key]; previous != "" {
			return nil, fmt.Errorf("client %q uses the same key as client %q", entries[i].Name, previous)
		}
		seen[entries[i].Name] = true
		seenKeys[entries[i].Key] = entries[i].Name
	}
	return entries, nil
}

func rejectImplicitKeyCollision(implicit *clientKey, entries []clientKey) error {
	if implicit == nil {
		return nil
	}
	for _, entry := range entries {
		if entry.Key == implicit.Key {
			return fmt.Errorf("client %q uses the same key as the implicit %q client", entry.Name, implicitClientName)
		}
	}
	return nil
}

// enabled reports whether client auth is configured at all. When false every
// request is allowed anonymously, matching the historical no-key behavior.
func (reg *keyRegistry) enabled() bool {
	if reg == nil {
		return false
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.implicit != nil || reg.path != "" || len(reg.entries) > 0
}

// resolve matches a bearer token against every configured key. The comparison
// visits ALL entries with a constant-time compare per entry — never a map
// lookup or an early return on match — so timing does not reveal which (or
// whether a) key matched. Disabled entries are compared but never accepted.
func (reg *keyRegistry) resolve(token string) (clientIdentity, bool) {
	if reg == nil {
		return clientIdentity{}, false
	}
	entries := reg.currentEntries()
	matched := -1
	for i := range entries {
		ok := subtle.ConstantTimeCompare([]byte(token), []byte(entries[i].Key)) == 1
		if ok && !entries[i].Disabled && matched == -1 {
			matched = i
		}
	}
	if matched == -1 {
		return clientIdentity{}, false
	}
	return clientIdentity{Name: entries[matched].Name, Role: entries[matched].Role}, true
}

// currentEntries returns the implicit key plus the (possibly reloaded) file
// entries. The file is stat'ed at most once per statInterval; a change in
// mtime or size triggers a re-read, so key rotation needs no restart. Reload
// failures are logged (never with key material) and keep the previous set.
func (reg *keyRegistry) currentEntries() []clientKey {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if reg.path != "" && time.Since(reg.checked) >= reg.statInterval {
		reg.checked = time.Now()
		raw, stat, err := readPrivateFile(reg.path)
		if err != nil {
			log.Printf("keys file %s read rejected: %v; keeping %d loaded keys", reg.path, err, len(reg.entries))
		} else if !stat.ModTime().Equal(reg.mtime) || stat.Size() != reg.size {
			if entries, err := parseKeysFile(raw); err != nil {
				log.Printf("keys file %s reload rejected: %v; keeping %d loaded keys", reg.path, err, len(reg.entries))
			} else if err := rejectImplicitKeyCollision(reg.implicit, entries); err != nil {
				log.Printf("keys file %s reload rejected: %v; keeping %d loaded keys", reg.path, err, len(reg.entries))
			} else {
				reg.entries = entries
				reg.mtime = stat.ModTime()
				reg.size = stat.Size()
				log.Printf("keys file %s reloaded: %d keys", reg.path, len(entries))
			}
		}
	}
	out := make([]clientKey, 0, len(reg.entries)+1)
	if reg.implicit != nil {
		out = append(out, *reg.implicit)
	}
	out = append(out, reg.entries...)
	return out
}

// registry returns the proxy's key registry, falling back to an implicit-only
// registry built from cfg.apiKey so tests (and any caller) that construct
// responsesProxy directly keep the historical single-key behavior.
func (p *responsesProxy) registry() *keyRegistry {
	if p.keys != nil {
		return p.keys
	}
	return &keyRegistry{implicit: implicitClientKey(p.cfg.apiKey), statInterval: keyRegistryStatInterval}
}

// bearerToken extracts the Authorization bearer token, or "" when absent.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(header, prefix))
}

// authenticate resolves the caller of a /v1/* request. When no keys are
// configured it allows the request anonymously (identity zero, ok true),
// matching the historical open behavior.
func (p *responsesProxy) authenticate(r *http.Request) (clientIdentity, bool) {
	reg := p.registry()
	if !reg.enabled() {
		return clientIdentity{}, true
	}
	token := bearerToken(r)
	if token == "" {
		return clientIdentity{}, false
	}
	return reg.resolve(token)
}
