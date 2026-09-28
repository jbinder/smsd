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
