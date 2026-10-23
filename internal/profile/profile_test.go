package profile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jbinder/smsd/internal/call"
	"github.com/jbinder/smsd/internal/database"
	"github.com/jbinder/smsd/internal/sms"
)

func TestStorePersistsAssignments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Exists() {
		t.Fatal("a missing file should not exist")
	}
	if _, ok := s.ForDevice("A1"); ok {
		t.Fatal("unknown phone has a profile")
	}

	p, err := s.Create("XQ_BT52", "A1")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "xq-bt52" {
		t.Errorf("id = %q", p.ID)
	}
	if q, _ := s.Create("XQ BT52"); q.ID != "xq-bt52-2" {
		t.Errorf("second id = %q, want a unique one", q.ID)
	}
	if _, err := s.Create("again", "A1"); err == nil {
		t.Error("a phone was put in two profiles")
	}
	if _, err := s.Attach("xq-bt52", "B2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Attach("nope", "C3"); err == nil {
		t.Error("attached to a missing profile")
	}
	if err := s.Ignore("D4"); err != nil {
		t.Fatal(err)
	}

	s, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Exists() {
		t.Error("saved file does not exist")
	}
	for _, serial := range []string{"A1", "B2"} {
		if got, ok := s.ForDevice(serial); !ok || got.ID != "xq-bt52" {
			t.Errorf("%s belongs to %+v", serial, got)
		}
	}
	if !s.Ignored("D4") || s.Ignored("A1") {
		t.Error("ignore list not kept")
	}

	// Choosing a profile for an ignored phone takes it off the list.
	if _, err := s.Attach("xq-bt52", "D4"); err != nil {
		t.Fatal(err)
	}
	if s.Ignored("D4") {
		t.Error("attached phone is still ignored")
	}
}

func TestLoadRejectsBadFiles(t *testing.T) {
	for name, content := range map[string]string{
		"shared phone": `{"profiles":[{"id":"a","devices":["X"]},{"id":"b","devices":["X"]}]}`,
		"bad id":       `{"profiles":[{"id":"../evil","devices":["X"]}]}`,
		"not json":     `{`,
	} {
		path := filepath.Join(t.TempDir(), "profiles.json")
		os.WriteFile(path, []byte(content), 0o644)
		if _, err := Load(path); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

func TestMigrateSharedSplitsPhones(t *testing.T) {
	dir := t.TempDir()
	sharedPath := filepath.Join(dir, "smsd.db")
	shared, err := database.Open(sharedPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, dev := range []string{"PHONE1", "PHONE2"} {
		if _, err := shared.ImportMessages(dev, []sms.Message{
			{AndroidID: 1, ThreadID: 1, Address: "+15551234567", Body: "from " + dev, Date: 1000, Type: sms.TypeReceived},
		}, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := shared.ImportCalls("PHONE2", []call.Call{
		{AndroidID: 7, Number: "+1", Date: 1000, Type: call.TypeIncoming},
	}, true); err != nil {
		t.Fatal(err)
	}
	shared.Close()

	s, _ := Load(filepath.Join(dir, "profiles.json"))
	profDir := filepath.Join(dir, "profiles")
	created, err := MigrateShared(s, sharedPath, profDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 {
		t.Fatalf("created %+v", created)
	}

	for _, want := range []struct {
		serial string
		calls  int
	}{{"PHONE1", 0}, {"PHONE2", 1}} {
		p, ok := s.ForDevice(want.serial)
		if !ok {
			t.Fatalf("%s has no profile", want.serial)
		}
		db, err := database.Open(DBPath(profDir, p))
		if err != nil {
			t.Fatal(err)
		}
		devices, _ := db.Devices()
		if len(devices) != 1 || devices[0] != want.serial {
			t.Errorf("%s's database holds %v", want.serial, devices)
		}
		page, _ := db.Messages("+15551234567", 0, database.Cursor{}, 10)
		if len(page.Messages) != 1 || page.Messages[0].Body != "from "+want.serial {
			t.Errorf("%s's messages: %+v", want.serial, page.Messages)
		}
		calls, _ := db.Calls("", 0, database.Cursor{}, 10)
		if len(calls.Calls) != want.calls {
			t.Errorf("%s has %d calls, want %d", want.serial, len(calls.Calls), want.calls)
		}
		db.Close()
	}

	// The shared database is kept whole, and a second run changes nothing.
	shared, _ = database.Open(sharedPath)
	if devices, _ := shared.Devices(); len(devices) != 2 {
		t.Errorf("shared database now holds %v", devices)
	}
	shared.Close()
	again, err := MigrateShared(s, sharedPath, profDir)
	if err != nil || len(again) != 0 {
		t.Errorf("second run: %v, %v", again, err)
	}
}
