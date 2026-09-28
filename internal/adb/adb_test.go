package adb

import (
	"strings"
	"testing"
)

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

func TestSendCommand(t *testing.T) {
	got := sendCommand(5, 8, 1, "+43 660 1234567", []string{"it's\nfine"})
	want := `service call isms 5 i32 1 s16 com.android.shell i32 -1 s16 '+43 660 1234567' i32 -1 ` +
		`s16 'it'\''s` + "\n" + `fine' i32 0 i32 0 i32 1 i64 0`
	if got != want {
		t.Errorf("single part:\n got %s\nwant %s", got, want)
	}

	got = sendCommand(5, 8, 2, "123", []string{"a", "b"})
	want = `service call isms 8 i32 2 s16 com.android.shell i32 -1 s16 '123' i32 -1 ` +
		`i32 2 s16 'a' s16 'b' i32 -1 i32 -1 i32 1 i64 0`
	if got != want {
		t.Errorf("multipart:\n got %s\nwant %s", got, want)
	}
}

func TestParseParcelInt(t *testing.T) {
	v, err := parseParcelInt("Result: Parcel(00000000 00000002   '........')\n")
	if err != nil || v != 2 {
		t.Errorf("int reply = %d, %v; want 2", v, err)
	}
	v, err = parseParcelInt("Result: Parcel(00000000 ffffffff   '........')\n")
	if err != nil || v != -1 {
		t.Errorf("negative reply = %d, %v; want -1", v, err)
	}
	if _, err := parseParcelInt("Result: Parcel(00000000    '....')\n"); err != nil {
		t.Errorf("void reply: %v", err)
	}

	exc := "Result: Parcel(\n" +
		"  0x00000000: ffffffff 00000014 00650053 00750063 '........S.e.c.u.'\n" +
		"  0x00000010: 00690072 00790074 00200020 00650044 'r.i.t.y. . .D.e.'\n" +
		"  0x00000020: 0069006e 00640065 00000000          'n.i.e.d.....    ')\n"
	if _, err := parseParcelInt(exc); err == nil || !strings.Contains(err.Error(), "Security Denied") {
		t.Errorf("exception reply error = %v, want the exception text", err)
	}
	if _, err := parseParcelInt("service: Service isms does not exist\n"); err == nil {
		t.Errorf("garbage reply: want error")
	}
}
