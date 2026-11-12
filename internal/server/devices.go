package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"sync"
	"time"
)

var deviceNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)

// Device is a registered client. Only the sha256 of the token is stored.
type Device struct {
	Name      string    `json:"name"`
	TokenHash string    `json:"token_hash"`
	CreatedAt time.Time `json:"created_at"`
}

// Devices manages devices.json. The file is reloaded when its mtime changes
// so that CLI commands take effect without restarting the server.
type Devices struct {
	path    string
	mu      sync.Mutex
	modTime time.Time
	list    []Device
}

func NewDevices(path string) *Devices { return &Devices{path: path} }

// Path returns the devices.json path.
func (d *Devices) Path() string { return d.path }

func (d *Devices) load() error {
	st, err := os.Stat(d.path)
	if errors.Is(err, os.ErrNotExist) {
		d.list = nil
		d.modTime = time.Time{}
		return nil
	}
	if err != nil {
		return err
	}
	if st.ModTime().Equal(d.modTime) && d.list != nil {
		return nil
	}
	b, err := os.ReadFile(d.path)
	if err != nil {
		return err
	}
	var list []Device
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("devices.json: %w", err)
	}
	d.list = list
	d.modTime = st.ModTime()
	return nil
}

func (d *Devices) save() error {
	b, err := json.MarshalIndent(d.list, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(d.path, b); err != nil {
		return err
	}
	d.modTime = time.Time{}
	return nil
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// Authenticate returns the device name for token.
func (d *Devices) Authenticate(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.load(); err != nil {
		return "", false
	}
	th := hashToken(token)
	for _, dev := range d.list {
		if subtle.ConstantTimeCompare([]byte(dev.TokenHash), []byte(th)) == 1 {
			return dev.Name, true
		}
	}
	return "", false
}

// Add creates a device and returns its new token.
func (d *Devices) Add(name string) (string, error) {
	if !deviceNameRe.MatchString(name) {
		return "", errors.New("device name must match [A-Za-z0-9_.-]{1,32}")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.load(); err != nil {
		return "", err
	}
	for _, dev := range d.list {
		if dev.Name == name {
			return "", fmt.Errorf("device %q already exists (revoke it first)", name)
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := "sds_" + hex.EncodeToString(buf)
	d.list = append(d.list, Device{Name: name, TokenHash: hashToken(token), CreatedAt: time.Now().UTC()})
	return token, d.save()
}

// Revoke removes a device.
func (d *Devices) Revoke(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.load(); err != nil {
		return err
	}
	for i, dev := range d.list {
		if dev.Name == name {
			d.list = append(d.list[:i], d.list[i+1:]...)
			return d.save()
		}
	}
	return fmt.Errorf("device %q not found", name)
}

// List returns all devices sorted by name.
func (d *Devices) List() ([]Device, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.load(); err != nil {
		return nil, err
	}
	out := append([]Device{}, d.list...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
