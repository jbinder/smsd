package adb

import "testing"

func TestParseDevices(t *testing.T) {
	out := "List of devices attached\n" +
		"emulator-5554\tdevice\n" +
		"ABC123\tunauthorized\n" +
		"XYZ789\toffline\n" +
		"* daemon started successfully *\n"
	devs := parseDevices(out)
	if len(devs) != 3 {
		t.Fatalf("got %d devices, want 3: %+v", len(devs), devs)
	}
	if !devs[0].Connected() {
		t.Errorf("emulator should be connected")
	}
	if devs[1].State != "unauthorized" || devs[1].Connected() {
		t.Errorf("ABC123 state wrong: %+v", devs[1])
	}
}

func TestParseContacts(t *testing.T) {
	// display_name deliberately contains a comma to exercise trailing-field parsing.
	out := "Row: 0 data1=+15551234567, display_name=Alice Example\n" +
		"Row: 1 data1=5559876543, display_name=Bob, Jr.\n" +
		"Row: 2 data1=NULL, display_name=NoNumber"
	cs := parseContacts(out)
	if len(cs) != 2 {
		t.Fatalf("got %d contacts, want 2: %+v", len(cs), cs)
	}
	if cs[0].Phone != "+15551234567" || cs[0].Name != "Alice Example" {
		t.Errorf("contact0 = %+v", cs[0])
	}
	if cs[1].Name != "Bob, Jr." {
		t.Errorf("contact1 name = %q, want 'Bob, Jr.'", cs[1].Name)
	}
}

func TestParseContacts_Empty(t *testing.T) {
	if got := parseContacts("No result found."); got != nil {
		t.Errorf("want nil, got %+v", got)
	}
}
