package session

import (
	"bytes"
	"image/png"
	"math/rand"
	"testing"

	"github.com/cicdata-io/qr-server/internal/fountain"
	"github.com/cicdata-io/qr-server/internal/protocol"
	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

func TestFrameCapacity(t *testing.T) {
	if got := BlockLength(2953); got != 2931 {
		t.Errorf("BlockLength(2953) = %d, want 2931", got)
	}
	// At 500 bytes per frame the u16 block counter, not the file size limit, is
	// the real ceiling — about 30 MB.
	if got := SourceBlockCount(33_000_000, 500); got <= MaxSourceBlocks {
		t.Errorf("33 MB at 500 B/frame should exceed %d blocks, got %d", MaxSourceBlocks, got)
	}
	if got := SmallestSufficientFrameSize(33_000_000); got != 1000 {
		t.Errorf("SmallestSufficientFrameSize(33 MB) = %d, want 1000", got)
	}
	if got := SmallestSufficientFrameSize(1000); got != 500 {
		t.Errorf("SmallestSufficientFrameSize(1 KB) = %d, want 500", got)
	}
}

func TestNewRejectsTooManyBlocks(t *testing.T) {
	// Incompressible, so the container does not shrink under the ceiling.
	data := make([]byte, 40<<20)
	rand.New(rand.NewSource(4)).Read(data)
	_, err := New("big.bin", "application/octet-stream", data, Options{FrameBytes: 500})
	if err == nil {
		t.Fatal("expected a refusal naming a larger frame size")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("bytes per frame")) {
		t.Fatalf("refusal should name the setting that fixes it, got: %v", err)
	}
}

// TestSessionFramesDecodeToWire is the check no unit test of the layers can
// make: that the PNG a camera would actually see carries the exact wire bytes.
func TestSessionFramesDecodeToWire(t *testing.T) {
	data := bytes.Repeat([]byte("optical transfer payload "), 400)
	s, err := New("notes.txt", "text/plain", data, Options{FrameBytes: 1000, Cycles: 1, Scale: 512})
	if err != nil {
		t.Fatal(err)
	}
	if s.FrameCount != fountain.CycleLength(s.K) {
		t.Fatalf("one cycle should be %d frames, got %d", fountain.CycleLength(s.K), s.FrameCount)
	}

	reader := qrcode.NewQRCodeReader()
	hints := map[gozxing.DecodeHintType]any{
		gozxing.DecodeHintType_PURE_BARCODE: true,
		// Without this gozxing sniffs UTF-8 and mangles high bytes; the frames
		// are binary, and byte mode is ISO-8859-1 by definition.
		gozxing.DecodeHintType_CHARACTER_SET: "ISO-8859-1",
	}
	for _, n := range []int{0, 1, s.K, s.FrameCount - 1} {
		raw, err := s.FramePNG(n)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		bmp, err := gozxing.NewBinaryBitmapFromImage(img)
		if err != nil {
			t.Fatal(err)
		}
		res, err := reader.Decode(bmp, hints)
		if err != nil {
			t.Fatalf("frame %d did not decode: %v", n, err)
		}
		// gozxing decodes a byte-mode segment as ISO-8859-1, so every code point
		// maps back to the byte it came from.
		got := make([]byte, 0, len(res.GetText()))
		for _, r := range res.GetText() {
			got = append(got, byte(r))
		}
		want := s.WireFrame(n)
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d decoded to %d bytes that do not match the %d wire bytes",
				n, len(got), len(want))
		}
	}
}

// TestSessionRoundTripThroughFrames drives a session's own frames back through
// a decoder — the server's frames must be sufficient on their own.
func TestSessionRoundTripThroughFrames(t *testing.T) {
	src := make([]byte, 64*1024)
	rand.New(rand.NewSource(5)).Read(src)
	s, err := New("random.bin", "application/octet-stream", src, Options{FrameBytes: 2953, Cycles: 1})
	if err != nil {
		t.Fatal(err)
	}
	var dec *fountain.Decoder
	for n := 0; n < s.FrameCount; n++ {
		h, block, err := protocol.ParseFrame(s.WireFrame(n))
		if err != nil {
			t.Fatal(err)
		}
		if dec == nil {
			dec = fountain.NewDecoder(int(h.K), int(h.BlockLen), h.SessionID, int(h.TotalLen))
		}
		dec.AddFrame(h.Seq, block)
		if dec.IsComplete() {
			break
		}
	}
	if !dec.IsComplete() {
		t.Fatal("one full cycle should decode with no loss")
	}
	u, err := protocol.UnpackFile(dec.Assemble())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(u.Bytes, src) || u.Name != "random.bin" {
		t.Fatal("payload did not survive the session's own frames")
	}
}

func TestBundleZipIsReadable(t *testing.T) {
	z, err := BundleZip([]ZipEntry{
		{Name: "docs/a.txt", Data: []byte("alpha")},
		{Name: "docs/b.txt", Data: []byte("beta")},
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New("docs.zip", "application/zip", z, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// A zip is precompressed, so the container must not spend a gzip pass on it.
	if s.Compression != "none" {
		t.Errorf("zip bundle was gzipped: %s", s.Compression)
	}
}
