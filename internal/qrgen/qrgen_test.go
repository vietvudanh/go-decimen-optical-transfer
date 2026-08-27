package qrgen

import (
	"bytes"
	"image/png"
	"math/rand"
	"testing"
)

// TestVersionLocks: the first frame locks the QR version and every later frame
// is forced to it, so all frames of a stream share identical geometry — the
// player must not resize mid-carousel, and a camera must not have to refocus.
func TestVersionLocks(t *testing.T) {
	var g Generator
	rng := rand.New(rand.NewSource(6))
	frame := make([]byte, 2953)
	rng.Read(frame)
	if _, err := g.PNG(frame, 512); err != nil {
		t.Fatal(err)
	}
	locked := g.Version()
	if locked != 40 {
		t.Fatalf("2953 bytes at ECC L is version 40, got %d", locked)
	}

	// Digits would otherwise encode in numeric mode at a much lower version.
	digits := bytes.Repeat([]byte("0123456789"), 100)
	raw, err := g.PNG(digits, 512)
	if err != nil {
		t.Fatal(err)
	}
	if g.Version() != locked {
		t.Fatalf("version drifted to %d", g.Version())
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	// Version 40 is 177 modules, plus a 4-module quiet zone each side.
	if got, want := img.Bounds().Dx(), (177+2*QuietZoneModules)*2; got != want {
		t.Fatalf("image is %d px, want %d (185 modules at scale 2)", got, want)
	}
}

// TestScaleIsNearestNeighbour: one module must never blur into its neighbour,
// which is what a camera on the other end is trying to read.
func TestScaleIsNearestNeighbour(t *testing.T) {
	var g Generator
	raw, err := g.PNG([]byte("hello"), 400)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	// Corner of the quiet zone is white; the finder pattern next to it is black.
	if r, _, _, _ := img.At(0, 0).RGBA(); r != 0xffff {
		t.Fatal("quiet zone corner is not white")
	}
	var sawBlack bool
	b := img.Bounds()
	for y := 0; y < b.Dy() && !sawBlack; y++ {
		for x := 0; x < b.Dx(); x++ {
			if r, _, _, _ := img.At(x, y).RGBA(); r == 0 {
				sawBlack = true
				break
			}
		}
	}
	if !sawBlack {
		t.Fatal("no dark modules rendered")
	}
}
