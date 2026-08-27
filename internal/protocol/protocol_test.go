package protocol

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// TestCanonicalFrame pins the golden vector from docs/technical/golden-vectors.md.
// A diff to these bytes is a wire-format change.
func TestCanonicalFrame(t *testing.T) {
	got := PackFrame(Header{
		SessionID: 0xBEEF, Seq: 0x01020304, K: 0x0111, BlockLen: 6,
		TotalLen: 0x00FEDCBA, PayloadFNV: 0x89ABCDEF, Flags: 0,
	}, []byte{1, 2, 3, 4, 5, 6})
	want := "d1c30300efbe04030201110106" + "00badcfe00efcdab89" + "010203040506"
	if hex.EncodeToString(got) != want {
		t.Fatalf("frame bytes\n got %s\nwant %s", hex.EncodeToString(got), want)
	}
	h, block, err := ParseFrame(got)
	if err != nil {
		t.Fatal(err)
	}
	if h.SessionID != 0xBEEF || h.Seq != 0x01020304 || h.K != 0x0111 ||
		h.BlockLen != 6 || h.TotalLen != 0x00FEDCBA || h.PayloadFNV != 0x89ABCDEF {
		t.Fatalf("header round-trip mismatch: %+v", h)
	}
	if !bytes.Equal(block, []byte{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("block round-trip mismatch: %x", block)
	}
}

func TestClassification(t *testing.T) {
	base := PackFrame(Header{SessionID: 1, K: 1, BlockLen: 1, TotalLen: 1}, []byte{9})
	mutate := func(i int, v byte) []byte {
		b := append([]byte(nil), base...)
		b[i] = v
		return b
	}
	cases := []struct {
		name string
		in   []byte
		ok   bool
	}{
		{"well-formed", base, true},
		{"magic0 wrong", mutate(0, 0xd2), false},
		{"magic1 wrong", mutate(1, 0x42), false},
		{"v1 sender", mutate(1, 0x0c), false},
		{"newer version", mutate(2, 0x04), false},
		{"version 0", mutate(2, 0x00), false},
		{"unknown critical flag", mutate(3, 0x01), false},
		// The ignorable half of the flags byte must decode normally, or it was
		// never ignorable at all.
		{"unknown ignorable flag", mutate(3, 0x10), true},
		{"length disagrees with blockLen", base[:len(base)-1], false},
	}
	for _, c := range cases {
		_, _, err := ParseFrame(c.in)
		if (err == nil) != c.ok {
			t.Errorf("%s: ok=%v, err=%v", c.name, c.ok, err)
		}
	}
}

func TestFNV1a(t *testing.T) {
	if got := FNV1a([]byte("hello")); got != 0x4f9f2cab {
		t.Fatalf("fnv1a(hello) = %#x", got)
	}
	if got := FNV1a(nil); got != 0x811c9dc5 {
		t.Fatalf("fnv1a(empty) = %#x", got)
	}
}

// TestSplitmix32 pins the PRNG the fountain seeds from. Sender and receiver
// derive block subsets from it independently, so a drift here is silent and
// total.
func TestSplitmix32(t *testing.T) {
	rnd := Splitmix32(0)
	want := []uint32{0x64625032, 0xd9c0799c, 0xaf362e10, 0x7fa88912} // cross-checked against the JS sender
	for i, w := range want {
		if got := rnd(); got != w {
			t.Fatalf("splitmix32(0)[%d] = %#08x, want %#08x", i, got, w)
		}
	}
}

func TestContainerRoundTrip(t *testing.T) {
	// Compressible, over the 768-byte gzip threshold.
	data := bytes.Repeat([]byte("the quick brown fox "), 200)
	p, err := PackFile("dir/../notes.txt", "text/plain", data)
	if err != nil {
		t.Fatal(err)
	}
	if p.Compression != "gzip" {
		t.Fatalf("expected gzip for repetitive text, got %q", p.Compression)
	}
	if uint32(len(p.Container)) == 0 {
		t.Fatal("empty container")
	}
	u, err := UnpackFile(p.Container)
	if err != nil {
		t.Fatal(err)
	}
	if u.Name != "notes.txt" {
		t.Errorf("name = %q, want notes.txt (basename only)", u.Name)
	}
	if !bytes.Equal(u.Bytes, data) {
		t.Error("payload did not survive the round trip")
	}
}

func TestPrecompressedTypes(t *testing.T) {
	for _, tc := range []struct {
		t    string
		want bool
	}{
		{"image/jpeg", true}, {"image/svg+xml", false}, {"image/bmp", false},
		{"video/mp4", true}, {"audio/wav", false}, {"audio/mpeg", true},
		{"application/zip", true}, {"application/epub+zip", true},
		{"text/plain", false}, {"application/pdf", false},
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", true},
	} {
		if got := IsPrecompressedType(tc.t); got != tc.want {
			t.Errorf("IsPrecompressedType(%q) = %v, want %v", tc.t, got, tc.want)
		}
	}
}
