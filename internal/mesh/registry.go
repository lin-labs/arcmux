package mesh

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

const RegistryVersion = 1

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Registry is machine-local mesh identity and pairing state. Tokens must never
// be serialized through status APIs or logs.
type Registry struct {
	Version  int               `json:"version"`
	DeviceID string            `json:"device_id"`
	Serve    bool              `json:"serve"`
	Accept   map[string]string `json:"accept,omitempty"`
	Peers    []Peer            `json:"peers,omitempty"`
}

type Peer struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Token string `json:"token"`
}

func LoadRegistry(path string) (*Registry, error) {
	info, statErr := os.Lstat(path)
	if statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("mesh registry must not be a symlink")
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("mesh registry permissions %04o are too open; require 0600 or stricter", info.Mode().Perm())
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("stat mesh registry: %w", statErr)
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Registry{Version: RegistryVersion, Accept: map[string]string{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read mesh registry: %w", err)
	}
	var r Registry
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("parse mesh registry: %w", err)
	}
	if r.Accept == nil {
		r.Accept = map[string]string{}
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

func (r *Registry) Validate() error {
	if r.Version != RegistryVersion {
		return fmt.Errorf("mesh registry version %d is unsupported", r.Version)
	}
	if r.DeviceID != "" && !safeID.MatchString(r.DeviceID) {
		return fmt.Errorf("mesh device id %q is invalid", r.DeviceID)
	}
	seen := map[string]bool{}
	for id, token := range r.Accept {
		if !safeID.MatchString(id) || token == "" {
			return fmt.Errorf("mesh accepted peer %q is invalid", id)
		}
	}
	for _, p := range r.Peers {
		if !safeID.MatchString(p.ID) || p.URL == "" || p.Token == "" {
			return fmt.Errorf("mesh outbound peer %q is incomplete", p.ID)
		}
		if p.ID == r.DeviceID {
			return fmt.Errorf("mesh peer %q cannot be this device", p.ID)
		}
		if seen[p.ID] {
			return fmt.Errorf("mesh peer %q is duplicated", p.ID)
		}
		if _, inbound := r.Accept[p.ID]; inbound {
			return fmt.Errorf("mesh peer %q cannot be both inbound and outbound in protocol v1", p.ID)
		}
		seen[p.ID] = true
	}
	return nil
}

// SaveRegistry atomically persists the complete registry with owner-only
// permissions. It never touches config.toml.
func SaveRegistry(path string, r *Registry) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create mesh config dir: %w", err)
	}
	sort.Slice(r.Peers, func(i, j int) bool { return r.Peers[i].ID < r.Peers[j].ID })
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mesh-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (r *Registry) UpsertPeer(peer Peer) {
	for i := range r.Peers {
		if r.Peers[i].ID == peer.ID {
			r.Peers[i] = peer
			return
		}
	}
	r.Peers = append(r.Peers, peer)
}
