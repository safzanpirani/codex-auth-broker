package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestClientAuthenticationKeepsAdmissionAndAttributionTogether(t *testing.T) {
	reg := &keyRegistry{entries: []clientKey{{Name: "original", Key: "test-key", Role: roleClient}}}
	p := &responsesProxy{keys: reg, requests: newRequestLogStore(10)}
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	r.Header.Set("Authorization", "Bearer test-key")
	r.Header.Set(brokerUserHeader, "test-user")
	p.withClientAuthentication(func(w http.ResponseWriter, r *http.Request) {
		id, allowed := p.authenticate(r)
		if !allowed || id.Name != "original" {
			t.Fatalf("unexpected admission: %+v, %v", id, allowed)
		}
		reg.mu.Lock()
		reg.entries = []clientKey{{Name: "replacement", Key: "test-key", Role: roleClient}}
		reg.mu.Unlock()
		entry := p.beginRequestLog(r)
		if entry.Entry.ClientName != "original" || entry.Entry.User != "test-user" {
			t.Fatalf("attribution changed after rotation: %+v", entry.Entry)
		}
	})(httptest.NewRecorder(), r)
	if id, allowed := p.authenticate(r); !allowed || id.Name != "replacement" {
		t.Fatalf("next request did not see rotation: %+v, %v", id, allowed)
	}
}

func TestParseKeysFile(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
		want    []clientKey
	}{
		{
			name: "valid entries",
			raw:  `[{"name":"backend","key":"k1","role":"client"},{"name":"dev","key":"k2","role":"admin"}]`,
			want: []clientKey{
				{Name: "backend", Key: "k1", Role: roleClient},
				{Name: "dev", Key: "k2", Role: roleAdmin},
			},
		},
		{
			name: "role defaults to client",
			raw:  `[{"name":"backend","key":"k1"}]`,
			want: []clientKey{{Name: "backend", Key: "k1", Role: roleClient}},
		},
		{
			name: "disabled entry kept",
			raw:  `[{"name":"old","key":"k1","role":"client","disabled":true}]`,
			want: []clientKey{{Name: "old", Key: "k1", Role: roleClient, Disabled: true}},
		},
		{
			name: "empty array",
			raw:  `[]`,
			want: nil,
		},
		{name: "missing name", raw: `[{"key":"k1","role":"client"}]`, wantErr: true},
		{name: "missing key", raw: `[{"name":"backend","role":"client"}]`, wantErr: true},
		{name: "bad role", raw: `[{"name":"backend","key":"k1","role":"root"}]`, wantErr: true},
		{name: "duplicate name", raw: `[{"name":"a","key":"k1"},{"name":"a","key":"k2"}]`, wantErr: true},
		{name: "duplicate key", raw: `[{"name":"a","key":"shared"},{"name":"b","key":"shared"}]`, wantErr: true},
		{name: "invalid JSON", raw: `{`, wantErr: true},
		{name: "object not array", raw: `{"name":"a","key":"k1"}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseKeysFile([]byte(tt.raw))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseKeysFile succeeded, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseKeysFile failed: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d entries, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("entry %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestKeyRegistryResolve(t *testing.T) {
	reg := &keyRegistry{
		implicit: implicitClientKey("legacy-key"),
		entries: []clientKey{
			{Name: "backend", Key: "backend-key", Role: roleClient},
			{Name: "dev", Key: "dev-key", Role: roleAdmin},
			{Name: "retired", Key: "retired-key", Role: roleClient, Disabled: true},
		},
		statInterval: keyRegistryStatInterval,
	}
	tests := []struct {
		name     string
		token    string
		wantOK   bool
		wantName string
		wantRole string
	}{
		{name: "implicit legacy key is admin default", token: "legacy-key", wantOK: true, wantName: implicitClientName, wantRole: roleAdmin},
		{name: "client key resolves name and role", token: "backend-key", wantOK: true, wantName: "backend", wantRole: roleClient},
		{name: "admin key resolves role", token: "dev-key", wantOK: true, wantName: "dev", wantRole: roleAdmin},
		{name: "disabled key rejected", token: "retired-key", wantOK: false},
		{name: "unknown key rejected", token: "nope", wantOK: false},
		{name: "prefix of key rejected", token: "backend-ke", wantOK: false},
		{name: "key plus suffix rejected", token: "backend-key2", wantOK: false},
		{name: "empty token rejected", token: "", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := reg.resolve(tt.token)
			if ok != tt.wantOK {
				t.Fatalf("resolve ok = %t, want %t", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if id.Name != tt.wantName || id.Role != tt.wantRole {
				t.Fatalf("resolve = %+v, want name=%s role=%s", id, tt.wantName, tt.wantRole)
			}
		})
	}
}

func TestKeyRegistryReloadOnFileChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	writeKeys := func(raw string, mtime time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().Add(-time.Hour)
	writeKeys(`[{"name":"backend","key":"old-key","role":"client"}]`, base)

	reg, err := newKeyRegistry("", path)
	if err != nil {
		t.Fatalf("newKeyRegistry failed: %v", err)
	}
	reg.statInterval = 0 // stat on every resolve so the test needs no sleeping
	if !reg.enabled() {
		t.Fatal("registry with keys file should report enabled")
	}
	if _, ok := reg.resolve("old-key"); !ok {
		t.Fatal("initial key not accepted")
	}

	// Rotation: replace the key, flip roles — picked up without a restart.
	writeKeys(`[{"name":"backend","key":"new-key","role":"admin"}]`, base.Add(time.Minute))
	if _, ok := reg.resolve("old-key"); ok {
		t.Fatal("rotated-out key still accepted after reload")
	}
	id, ok := reg.resolve("new-key")
	if !ok {
		t.Fatal("rotated-in key not accepted after reload")
	}
	if id.Role != roleAdmin {
		t.Fatalf("reloaded role = %s, want admin", id.Role)
	}

	// A broken rewrite must not lock everyone out: keep the last good set.
	writeKeys(`{not json`, base.Add(2*time.Minute))
	if _, ok := reg.resolve("new-key"); !ok {
		t.Fatal("previous keys dropped after an invalid reload")
	}
}

func TestNewKeyRegistryRejectsBadStartupFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(path, []byte(`[{"name":"","key":"k"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newKeyRegistry("", path); err == nil {
		t.Fatal("invalid startup keys file accepted")
	}
	if _, err := newKeyRegistry("", filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing startup keys file accepted")
	}
}

func TestNewKeyRegistryRejectsImplicitKeyCollision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(path, []byte(`[{"name":"backend","key":"shared","role":"client"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newKeyRegistry("shared", path); err == nil {
		t.Fatal("keys file reused the implicit admin key")
	}
}

func TestKeyRegistryRejectsImplicitNameCollision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	collision := []byte(`[{"name":"default","key":"client-key","role":"client"}]`)
	if err := os.WriteFile(path, collision, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newKeyRegistry("admin-key", path); err == nil {
		t.Fatal("keys file reused the implicit client name")
	}
	if _, err := newKeyRegistry("", path); err != nil {
		t.Fatalf("keys-only configuration rejected default name: %v", err)
	}
	if err := os.WriteFile(path, []byte(`[{"name":"backend","key":"original-key"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := newKeyRegistry("admin-key", path)
	if err != nil {
		t.Fatal(err)
	}
	registry.statInterval = 0
	if err := os.WriteFile(path, collision, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.resolve("client-key"); ok {
		t.Fatal("reload accepted the implicit name collision")
	}
	if id, ok := registry.resolve("original-key"); !ok || id.Name != "backend" {
		t.Fatal("invalid reload discarded the previous client")
	}
}

func TestNewKeyRegistryRejectsPermissiveFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file modes do not represent ACL privacy")
	}
	path := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(path, []byte(`[{"name":"backend","key":"secret"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newKeyRegistry("", path); err == nil {
		t.Fatal("keys file with group or world permissions was accepted")
	}
}

func TestAuthenticateMultiKey(t *testing.T) {
	proxy := &responsesProxy{keys: &keyRegistry{
		implicit: implicitClientKey("legacy-key"),
		entries: []clientKey{
			{Name: "backend", Key: "backend-key", Role: roleClient},
		},
		statInterval: keyRegistryStatInterval,
	}}
	tests := []struct {
		name     string
		header   string
		wantOK   bool
		wantName string
	}{
		{name: "named key accepted", header: "Bearer backend-key", wantOK: true, wantName: "backend"},
		{name: "legacy key accepted as default", header: "Bearer legacy-key", wantOK: true, wantName: implicitClientName},
		{name: "missing header rejected", header: "", wantOK: false},
		{name: "wrong key rejected", header: "Bearer wrong", wantOK: false},
		{name: "wrong scheme rejected", header: "Basic backend-key", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if tt.header != "" {
				request.Header.Set("Authorization", tt.header)
			}
			id, ok := proxy.authenticate(request)
			if ok != tt.wantOK {
				t.Fatalf("authenticate ok = %t, want %t", ok, tt.wantOK)
			}
			if ok && id.Name != tt.wantName {
				t.Fatalf("client name = %q, want %q", id.Name, tt.wantName)
			}
		})
	}
}

// TestAuthenticateCompatSingleKey pins the backwards-compat invariant: a proxy
// configured only with cfg.apiKey (no registry) behaves as the implicit client
// "default" with role admin, and no key config at all disables auth.
func TestAuthenticateCompatSingleKey(t *testing.T) {
	proxy := &responsesProxy{cfg: config{apiKey: "secret-key"}}
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer secret-key")
	id, ok := proxy.authenticate(request)
	if !ok {
		t.Fatal("single --api-key not accepted")
	}
	if id.Name != implicitClientName || id.Role != roleAdmin {
		t.Fatalf("identity = %+v, want default/admin", id)
	}

	open := &responsesProxy{}
	anon, ok := open.authenticate(httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if !ok {
		t.Fatal("request rejected with no keys configured")
	}
	if anon.Name != "" {
		t.Fatalf("anonymous identity has name %q", anon.Name)
	}
}

func TestRequestLogRecordsClientName(t *testing.T) {
	proxy := &responsesProxy{
		cfg:      config{apiKey: "secret-key"},
		requests: newRequestLogStore(10),
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.Header.Set("Authorization", "Bearer secret-key")
	entry := proxy.beginRequestLog(request)
	if entry.Entry.ClientName != implicitClientName {
		t.Fatalf("client_name = %q, want %q", entry.Entry.ClientName, implicitClientName)
	}

	request.Header.Set("Authorization", "Bearer wrong")
	entry = proxy.beginRequestLog(request)
	if entry.Entry.ClientName != "" {
		t.Fatalf("unauthorized request attributed to %q", entry.Entry.ClientName)
	}
}
