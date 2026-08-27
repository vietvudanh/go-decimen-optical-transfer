// Package qrgen is the one QR generation path. Every frame image the server
// serves comes through here, so nothing can drift on ECC level or version
// locking.
//
// Error correction stays at L, as in the original app: in-frame ECC and the
// fountain solve different problems (corruption vs erasure), and at these
// frame sizes "decode whole or discard" plus fountain redundancy is the better
// trade.
package qrgen

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"

	qrcode "github.com/skip2/go-qrcode"
)

// QuietZoneModules is the white border every code carries. skip2/go-qrcode
// draws exactly this border, matching the original sender's quiet zone.
const QuietZoneModules = 4

// Generator renders successive frames at one locked QR version. The first
// frame locks it; every later frame is forced to the same version, so all
// frames of a stream share identical geometry even if one frame's bytes would
// otherwise encode in a cheaper mode.
type Generator struct {
	version int
}

// PNG renders one frame's wire bytes as a bilevel PNG, scaled to roughly
// pxSize pixels per side.
func (g *Generator) PNG(frame []byte, pxSize int) ([]byte, error) {
	var (
		qr  *qrcode.QRCode
		err error
	)
	if g.version == 0 {
		qr, err = qrcode.New(string(frame), qrcode.Low)
		if err != nil {
			return nil, err
		}
		g.version = qr.VersionNumber
	} else {
		qr, err = qrcode.NewWithForcedVersion(string(frame), g.version, qrcode.Low)
		if err != nil {
			return nil, err
		}
	}
	return encodeBilevelPNG(qr.Bitmap(), pxSize)
}

// Version reports the locked QR version, or 0 before the first frame.
func (g *Generator) Version() int { return g.version }

// encodeBilevelPNG paints a module matrix with nearest-neighbour upscaling —
// one module must never blur into its neighbour, which is what a camera on the
// other end is trying to read.
func encodeBilevelPNG(bitmap [][]bool, pxSize int) ([]byte, error) {
	n := len(bitmap)
	if n == 0 {
		return nil, errors.New("empty qr bitmap")
	}
	scale := max(1, pxSize/n)
	side := n * scale
	img := image.NewPaletted(image.Rect(0, 0, side, side),
		color.Palette{color.White, color.Black})
	for y := 0; y < n; y++ {
		row := bitmap[y]
		for x := 0; x < n; x++ {
			if !row[x] {
				continue
			}
			for dy := 0; dy < scale; dy++ {
				line := img.Pix[(y*scale+dy)*img.Stride+x*scale:]
				for dx := 0; dx < scale; dx++ {
					line[dx] = 1
				}
			}
		}
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
