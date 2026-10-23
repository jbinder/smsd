// Package profile keeps phones apart. Each profile has its own database, and
// a phone syncs only once it has been assigned to a profile; a phone smsd has
// never seen is left alone until the user decides what to do with it. The
// assignments live in a small JSON file next to the configuration.
package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Profile is one person's (or one number's) history. It usually holds a single
// phone, but a replacement phone can be added to the same profile.
type Profile struct {
	// ID names the database file and never changes; Name is free to edit.
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Devices []string `json:"devices"`
}

type file struct {
	Profiles []Profile `json:"profiles"`
	// Ignored phones are never synced and not offered for setup again.
	Ignored []string `json:"ignored,omitempty"`
}

// Store is the set of profiles, persisted to a JSON file on every change. It
// is safe for concurrent use.
type Store struct {
	path string

	mu     sync.Mutex
	f      file
	exists bool
}

// Load reads the profiles file at path. A missing file is an empty store.
func Load(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading profiles: %w", err)
	}
	if err := json.Unmarshal(data, &s.f); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	seen := map[string]string{}
	for _, p := range s.f.Profiles {
		if !validID(p.ID) {
			return nil, fmt.Errorf("%s: profile %q has an invalid id %q", path, p.Name, p.ID)
		}
		for _, d := range p.Devices {
			if other, ok := seen[d]; ok {
				return nil, fmt.Errorf("%s: phone %s is in both %q and %q", path, d, other, p.ID)
			}
			seen[d] = p.ID
		}
	}
	s.exists = true
	return s, nil
}

// Exists reports whether the profiles file was there when loaded or has been
// written since.
func (s *Store) Exists() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exists
}

// List returns a copy of all profiles in creation order.
func (s *Store) List() []Profile {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Profile, len(s.f.Profiles))
	for i, p := range s.f.Profiles {
		p.Devices = slices.Clone(p.Devices)
		out[i] = p
	}
	return out
}

// Get returns the profile with id.
func (s *Store) Get(id string) (Profile, bool) {
	for _, p := range s.List() {
		if p.ID == id {
			return p, true
		}
	}
	return Profile{}, false
}

// ForDevice returns the profile serial belongs to.
func (s *Store) ForDevice(serial string) (Profile, bool) {
	for _, p := range s.List() {
		if slices.Contains(p.Devices, serial) {
			return p, true
		}
	}
	return Profile{}, false
}

// Ignored reports whether serial was set to be ignored.
func (s *Store) Ignored(serial string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.f.Ignored, serial)
}

// Create adds a profile called name holding serials and saves the store. Its
// id is derived from the name and made unique.
func (s *Store) Create(name string, serials ...string) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, serial := range serials {
		if id := s.owner(serial); id != "" {
			return Profile{}, fmt.Errorf("phone %s already belongs to profile %q", serial, id)
		}
	}
	p := Profile{ID: s.uniqueID(slug(name)), Name: name, Devices: slices.Clone(serials)}
	s.f.Profiles = append(s.f.Profiles, p)
	s.unignore(serials...)
	if err := s.save(); err != nil {
		s.f.Profiles = s.f.Profiles[:len(s.f.Profiles)-1]
		return Profile{}, err
	}
	return p, nil
}

// Attach adds serial to the existing profile id and saves the store.
func (s *Store) Attach(id, serial string) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner := s.owner(serial); owner != "" {
		return Profile{}, fmt.Errorf("phone %s already belongs to profile %q", serial, owner)
	}
	for i := range s.f.Profiles {
		p := &s.f.Profiles[i]
		if p.ID != id {
			continue
		}
		p.Devices = append(p.Devices, serial)
		s.unignore(serial)
		if err := s.save(); err != nil {
			p.Devices = p.Devices[:len(p.Devices)-1]
			return Profile{}, err
		}
		out := *p
		out.Devices = slices.Clone(p.Devices)
		return out, nil
	}
	return Profile{}, fmt.Errorf("no profile %q", id)
}

// Ignore records that serial should never be synced and saves the store.
func (s *Store) Ignore(serial string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.Contains(s.f.Ignored, serial) {
		return nil
	}
	s.f.Ignored = append(s.f.Ignored, serial)
	if err := s.save(); err != nil {
		s.f.Ignored = s.f.Ignored[:len(s.f.Ignored)-1]
		return err
	}
	return nil
}

// owner returns the id of the profile holding serial, or "". Caller holds mu.
func (s *Store) owner(serial string) string {
	for _, p := range s.f.Profiles {
		if slices.Contains(p.Devices, serial) {
			return p.ID
		}
	}
	return ""
}

// unignore drops serials from the ignore list. Caller holds mu.
func (s *Store) unignore(serials ...string) {
	s.f.Ignored = slices.DeleteFunc(s.f.Ignored, func(d string) bool {
		return slices.Contains(serials, d)
	})
}

func (s *Store) uniqueID(base string) string {
	taken := func(id string) bool {
		return slices.ContainsFunc(s.f.Profiles, func(p Profile) bool { return p.ID == id })
	}
	id := base
	for n := 2; taken(id); n++ {
		id = base + "-" + strconv.Itoa(n)
	}
	return id
}

// save writes the file atomically, so a crash mid-write cannot lose every
// assignment and make all phones look new. Caller holds mu.
func (s *Store) save() error {
	data, err := json.MarshalIndent(s.f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.exists = true
	return nil
}

// slug turns a name into a file-name-safe id: lower-case ASCII letters, digits
// and dashes.
func slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	id := strings.TrimSuffix(b.String(), "-")
	if id == "" {
		return "phone"
	}
	return id
}

func validID(id string) bool {
	return id != "" && slug(id) == id
}
