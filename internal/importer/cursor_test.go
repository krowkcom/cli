package importer

import "testing"

func TestCursorRoundTrip(t *testing.T) {
	jc := JSONLCursor{Offset: 1234, Size: 9999}
	enc, err := jc.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if enc != `{"offset":1234,"size":9999}` {
		t.Fatalf("encoded = %s", enc)
	}
	back, err := DecodeJSONLCursor(enc)
	if err != nil || back != jc {
		t.Fatalf("DecodeJSONLCursor = %+v, %v", back, err)
	}

	sc := SQLiteCursor{TimeUpdated: 1757414400000}
	enc, err = sc.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if enc != `{"time_updated":1757414400000}` {
		t.Fatalf("encoded = %s", enc)
	}
	sback, err := DecodeSQLiteCursor(enc)
	if err != nil || sback != sc {
		t.Fatalf("DecodeSQLiteCursor = %+v, %v", sback, err)
	}
}

// An absent import_state row is an empty string, and "never read" is a
// normal state rather than a corrupt one.
func TestDecodeCursorEmptyIsZero(t *testing.T) {
	jc, err := DecodeJSONLCursor("")
	if err != nil || !jc.Zero() {
		t.Fatalf("DecodeJSONLCursor(\"\") = %+v, %v", jc, err)
	}
	sc, err := DecodeSQLiteCursor("")
	if err != nil || !sc.Zero() {
		t.Fatalf("DecodeSQLiteCursor(\"\") = %+v, %v", sc, err)
	}
}

func TestDecodeCursorGarbageAndNegatives(t *testing.T) {
	if _, err := DecodeJSONLCursor("not json"); err == nil {
		t.Fatal("DecodeJSONLCursor accepted garbage")
	}
	if _, err := DecodeSQLiteCursor("not json"); err == nil {
		t.Fatal("DecodeSQLiteCursor accepted garbage")
	}
	// A negative watermark cannot have come from this package; rescanning
	// beats stranding the source.
	jc, err := DecodeJSONLCursor(`{"offset":-5,"size":-1}`)
	if err != nil {
		t.Fatalf("DecodeJSONLCursor: %v", err)
	}
	if jc != (JSONLCursor{}) {
		t.Fatalf("negative cursor = %+v, want the zero cursor", jc)
	}
	sc, err := DecodeSQLiteCursor(`{"time_updated":-5}`)
	if err != nil {
		t.Fatalf("DecodeSQLiteCursor: %v", err)
	}
	if sc != (SQLiteCursor{}) {
		t.Fatalf("negative cursor = %+v, want the zero cursor", sc)
	}
}

// Both cursor shapes satisfy Cursor, which is what lets a caller store one
// without knowing which it holds.
func TestCursorsImplementCursor(t *testing.T) {
	var cursors = []Cursor{JSONLCursor{Offset: 1}, SQLiteCursor{TimeUpdated: 1}}
	for _, c := range cursors {
		if c.Zero() {
			t.Fatalf("%T with a watermark reported zero", c)
		}
		if _, err := c.Encode(); err != nil {
			t.Fatalf("%T Encode: %v", c, err)
		}
	}
}

func TestRefKey(t *testing.T) {
	tests := []struct {
		name string
		ref  Ref
		want string
	}{
		{
			name: "id is the key",
			ref:  Ref{Provider: ProviderClaude, ID: "6f0f", Path: "/home/u/.claude/a.jsonl"},
			want: "claude:6f0f",
		},
		{
			name: "no id falls back to the path",
			ref:  Ref{Provider: ProviderOpencode, Path: "/home/u/.local/share/opencode/x"},
			want: "opencode:/home/u/.local/share/opencode/x",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.ref.Key(); got != tt.want {
				t.Fatalf("Key() = %q, want %q", got, tt.want)
			}
		})
	}
}
