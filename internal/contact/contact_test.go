package contact

import "testing"

func TestParseContacts(t *testing.T) {
	// display_name deliberately contains the column separator.
	out := "Row: 0 _id=10, lookup=96i35, starred=0, display_name=Alice Example\n" +
		"Row: 1 _id=11, lookup=NULL, starred=1, display_name=Bob, Jr.\n" +
		"Row: 2 _id=NULL, lookup=x, starred=0, display_name=Broken\n"
	cs := ParseContacts(out)
	if len(cs) != 2 {
		t.Fatalf("got %d contacts, want 2: %+v", len(cs), cs)
	}
	if cs[0] != (Contact{AndroidID: 10, LookupKey: "96i35", Name: "Alice Example"}) {
		t.Errorf("contact0 = %+v", cs[0])
	}
	if cs[1] != (Contact{AndroidID: 11, Name: "Bob, Jr.", Starred: true}) {
		t.Errorf("contact1 = %+v", cs[1])
	}
}

func TestParseDetails(t *testing.T) {
	out := "Row: 0 _id=1, contact_id=10, mimetype=vnd.android.cursor.item/phone_v2, data2=2, data3=NULL, data4=NULL, data1=+43 660 1234567\n" +
		"Row: 1 _id=2, contact_id=10, mimetype=vnd.android.cursor.item/email_v2, data2=0, data3=Club, data4=NULL, data1=a@example.org\n" +
		"Row: 2 _id=3, contact_id=10, mimetype=vnd.android.cursor.item/note, data2=NULL, data3=NULL, data4=NULL, data1=line one, still one\n" +
		"line two\n" +
		"Row: 3 _id=4, contact_id=10, mimetype=vnd.android.cursor.item/photo, data2=NULL, data3=NULL, data4=NULL, data1=NULL\n" +
		"Row: 4 _id=5, contact_id=11, mimetype=vnd.android.cursor.item/organization, data2=1, data3=NULL, data4=Engineer, data1=ACME, Inc.\n" +
		"Row: 5 _id=6, contact_id=11, mimetype=vnd.android.cursor.item/phone_v2, data2=1, data3=NULL, data4=NULL, data1=NULL\n"
	ds := ParseDetails(out)
	want := []Detail{
		{AndroidID: 1, ContactID: 10, Kind: KindPhone, Label: "Mobile", Value: "+43 660 1234567"},
		{AndroidID: 2, ContactID: 10, Kind: KindEmail, Label: "Club", Value: "a@example.org"},
		{AndroidID: 3, ContactID: 10, Kind: KindNote, Value: "line one, still one\nline two"},
		{AndroidID: 5, ContactID: 11, Kind: KindOrganization, Label: "Engineer", Value: "ACME, Inc."},
	}
	if len(ds) != len(want) {
		t.Fatalf("got %d details, want %d: %+v", len(ds), len(want), ds)
	}
	for i := range want {
		if ds[i] != want[i] {
			t.Errorf("detail %d = %+v, want %+v", i, ds[i], want[i])
		}
	}
}

func TestParseEmpty(t *testing.T) {
	if got := ParseContacts("No result found."); got != nil {
		t.Errorf("want nil, got %+v", got)
	}
	if got := ParseDetails(""); got != nil {
		t.Errorf("want nil, got %+v", got)
	}
}
