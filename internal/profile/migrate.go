package profile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jbinder/smsd/internal/database"
)

// DBPath is where profile p's database lives under dir.
func DBPath(dir string, p Profile) string {
	return filepath.Join(dir, p.ID+".db")
}

// MigrateShared splits the single database older versions kept for every
// phone into one profile per phone, named after its serial. The shared
// database is left untouched as a backup. It does nothing when the profiles
// file already exists or there is no shared database, and returns the
// profiles it created.
func MigrateShared(s *Store, shared, dir string) ([]Profile, error) {
	if s.Exists() {
		return nil, nil
	}
	if _, err := os.Stat(shared); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	src, err := database.Open(shared)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	devices, err := src.Devices()
	if err != nil {
		return nil, fmt.Errorf("listing phones in %s: %w", shared, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	// Every database is written before the profiles file, so an interrupted
	// migration runs again from scratch on the next start.
	var created []Profile
	ids := map[string]bool{}
	for _, serial := range devices {
		id := slug(serial)
		for n := 2; ids[id]; n++ {
			id = fmt.Sprintf("%s-%d", slug(serial), n)
		}
		ids[id] = true
		p := Profile{ID: id, Name: serial, Devices: []string{serial}}
		path := DBPath(dir, p)
		os.Remove(path) // a leftover of an interrupted run
		if err := src.ExportDevice(path, serial); err != nil {
			return nil, fmt.Errorf("separating phone %s: %w", serial, err)
		}
		created = append(created, p)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.f.Profiles = append(s.f.Profiles, created...)
	if err := s.save(); err != nil {
		s.f.Profiles = nil
		return nil, err
	}
	return created, nil
}
