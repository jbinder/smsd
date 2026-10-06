package call

import "testing"

func TestParseQueryOutput(t *testing.T) {
	raw := `Row: 0 _id=990, number=+4369911441080, date=1791221515027, duration=343, type=1, presentation=1
Row: 1 _id=989, number=+436601489613, date=1790949467802, duration=0, type=3, presentation=1
Row: 2 _id=12, number=, date=1790000000000, duration=0, type=3, presentation=2
Row: 3 _id=NULL, number=+1, date=1, duration=0, type=1, presentation=1
`
	calls := ParseQueryOutput(raw)
	if len(calls) != 3 {
		t.Fatalf("got %d calls, want 3 (row without id skipped): %+v", len(calls), calls)
	}
	want := Call{AndroidID: 990, Number: "+4369911441080", Date: 1791221515027, Duration: 343,
		Type: TypeIncoming, Presentation: PresentationAllowed}
	if calls[0] != want {
		t.Errorf("calls[0] = %+v, want %+v", calls[0], want)
	}
	if !calls[1].Missed() || calls[0].Missed() {
		t.Errorf("Missed() wrong: %v %v", calls[1].Missed(), calls[0].Missed())
	}
	if got := calls[2].Caller(); got != "Private number" {
		t.Errorf("withheld caller = %q, want Private number", got)
	}
	if got := calls[1].Caller(); got != "+436601489613" {
		t.Errorf("caller = %q", got)
	}
}

func TestParseQueryOutput_Empty(t *testing.T) {
	for _, raw := range []string{"", "No result found.\n"} {
		if got := ParseQueryOutput(raw); got != nil {
			t.Errorf("ParseQueryOutput(%q) = %+v, want nil", raw, got)
		}
	}
}

func TestParseQueryOutput_IDsOnly(t *testing.T) {
	calls := ParseQueryOutput("Row: 0 _id=5\nRow: 1 _id=7\n")
	if len(calls) != 2 || calls[0].AndroidID != 5 || calls[1].AndroidID != 7 {
		t.Errorf("got %+v", calls)
	}
}

func TestParseState(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want State
		ok   bool
	}{
		{"idle dual SIM", `  Phone Id=0
    mCallState=0
    mCallIncomingNumber=
  Phone Id=1
    mCallState=0
    mCallIncomingNumber=
`, State{State: StateIdle}, true},
		{"second SIM ringing", `  Phone Id=0
    mCallState=0
    mCallIncomingNumber=
  Phone Id=1
    mCallState=1
    mCallIncomingNumber=+436601489613
`, State{State: StateRinging, Number: "+436601489613"}, true},
		{"ringing during a call on the other SIM", `  Phone Id=0
    mCallState=2
    mCallIncomingNumber=+111
  Phone Id=1
    mCallState=1
    mCallIncomingNumber=+222
`, State{State: StateRinging, Number: "+222"}, true},
		{"ringing with hidden number", `  Phone Id=0
    mCallState=1
    mCallIncomingNumber=
`, State{State: StateRinging}, true},
		{"in a call", `  Phone Id=0
    mCallState=2
    mCallIncomingNumber=+111
`, State{State: StateOffhook}, true},
		{"unreadable", "Can't find service: telephony.registry\n", State{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseState(tc.raw)
			if got != tc.want || ok != tc.ok {
				t.Errorf("ParseState = %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}
