package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type M = map[string]any
type Account struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Enabled      bool   `json:"enabled"`
	Token        string `json:"token"`
	RefreshToken string `json:"refresh_token"`
	UserID       string `json:"user_id"`
	Expiry       int64  `json:"expiry"`
	Scope        string `json:"scope"`
	TenantID     string `json:"tenant_id,omitempty"`
	TenantName   string `json:"tenant_name,omitempty"`
	MachineID    string `json:"machine_id"`
	DeviceID     string `json:"device_id"`
	PrivateKey   []byte `json:"private_key,omitempty"`
	DeviceInfo   M      `json:"device_info,omitempty"`
	Check        M      `json:"check,omitempty"`
	Balance      M      `json:"balance,omitempty"`
}
type APIKey struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Secret  string    `json:"secret"`
	Enabled bool      `json:"enabled"`
	Created time.Time `json:"created"`
}
type ModelSetting struct {
	Disabled bool   `json:"disabled"`
	Alias    string `json:"alias"`
	Effort   string `json:"effort"`
	Max      bool   `json:"max"`
}
type Usage struct {
	Time       time.Time `json:"time"`
	Model      string    `json:"model"`
	Account    string    `json:"account"`
	Tokens     M         `json:"tokens"`
	Success    bool      `json:"success"`
	DurationMS int64     `json:"duration_ms"`
}
type State struct {
	Accounts []Account               `json:"accounts"`
	APIKey   string                  `json:"api_key"`
	Keys     []APIKey                `json:"keys,omitempty"`
	Models   map[string]ModelSetting `json:"models"`
	Usage    []Usage                 `json:"usage"`
}
type Store struct {
	mu    sync.Mutex
	state State
	dir   string
	aead  cipher.AEAD
}

func randomID() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmtUUID(b)
}
func fmtUUID(b []byte) string {
	const h = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out = append(out, '-')
		}
		out = append(out, h[v>>4], h[v&15])
	}
	return string(out)
}
func secret() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func openStore(dir, key string) (*Store, error) {
	raw, e := base64.StdEncoding.DecodeString(key)
	if e != nil || len(raw) != 32 {
		return nil, errors.New("RELAY_MASTER_KEY must be a base64 encoded 32-byte key")
	}
	if e = os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	block, e := aes.NewCipher(raw)
	if e != nil {
		return nil, e
	}
	a, e := cipher.NewGCM(block)
	if e != nil {
		return nil, e
	}
	s := &Store{dir: dir, aead: a, state: State{APIKey: secret(), Models: map[string]ModelSetting{}}}
	b, e := os.ReadFile(filepath.Join(dir, "state.enc"))
	if os.IsNotExist(e) {
		return s, s.saveLocked()
	}
	if e != nil {
		return nil, e
	}
	if len(b) < a.NonceSize() {
		return nil, errors.New("invalid encrypted state")
	}
	plain, e := a.Open(nil, b[:a.NonceSize()], b[a.NonceSize():], []byte("trae-relay-web:v1"))
	if e != nil {
		return nil, errors.New("cannot decrypt state: check master key")
	}
	e = json.Unmarshal(plain, &s.state)
	if s.state.Models == nil {
		s.state.Models = map[string]ModelSetting{}
	}
	return s, e
}
func (s *Store) saveLocked() error {
	b, e := json.Marshal(s.state)
	if e != nil {
		return e
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return e
	}
	b = s.aead.Seal(nonce, nonce, b, []byte("trae-relay-web:v1"))
	tmp := filepath.Join(s.dir, "state.tmp")
	f, e := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if e != nil {
		return e
	}
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return os.Rename(tmp, filepath.Join(s.dir, "state.enc"))
}
func (s *Store) putAccount(a Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, v := range s.state.Accounts {
		if v.ID == a.ID {
			s.state.Accounts[i] = a
			return s.saveLocked()
		}
	}
	s.state.Accounts = append(s.state.Accounts, a)
	return s.saveLocked()
}
func (s *Store) record(u Usage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Usage = append(s.state.Usage, u)
	if len(s.state.Usage) > 500 {
		s.state.Usage = s.state.Usage[len(s.state.Usage)-500:]
	}
	return s.saveLocked()
}
func (s *Store) active() (Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.state.Accounts {
		if a.Enabled {
			return a, true
		}
	}
	return Account{}, false
}
func (s *Store) apiKey() string { s.mu.Lock(); defer s.mu.Unlock(); return s.state.APIKey }
func (s *Store) setting(id string) ModelSetting {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Models[id]
}

// The legacy key remains valid during migration and is independently manageable.
func (s *Store) keysLocked() []APIKey {
	out := append([]APIKey{}, s.state.Keys...)
	if s.state.APIKey != "" {
		out = append([]APIKey{{ID: "legacy", Name: "默认密钥", Secret: s.state.APIKey, Enabled: true}}, out...)
	}
	return out
}
func (s *Store) validKey(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	valid := false
	for _, k := range s.keysLocked() {
		if equal(key, k.Secret) && k.Enabled && key != "" {
			valid = true
		}
	}
	return valid
}
