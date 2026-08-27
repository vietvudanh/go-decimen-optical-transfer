// Package session holds one armed transfer: the packed container, the fountain
// encoder over it, and the QR generator whose version the first frame locked.
//
// A session is the server-side equivalent of the desktop app's "armed" state.
// The carousel is endless but deterministic, so a finite render is well
// defined: Cycles full carousel cycles, one systematic sweep plus k repair
// frames each. One cycle decodes at low loss; extra cycles add repair
// diversity that a looping player would otherwise replay verbatim.
package session

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cicdata-io/qr-server/internal/fountain"
	"github.com/cicdata-io/qr-server/internal/protocol"
	"github.com/cicdata-io/qr-server/internal/qrgen"
)

// MaxSourceBlocks is the ceiling on k: the frame header numbers source blocks
// in a u16, so a large payload at a small frame size runs out of block numbers
// long before it runs out of the file size limit — at 500 bytes per frame the
// real ceiling is about 30 MB, not 64. Caught before streaming starts, with a
// message naming the setting that fixes it.
const MaxSourceBlocks = 0xffff

// Transmit tuning, ported from shared/send-settings.ts so the server's options
// can never point at a setting the original sender does not offer.
var (
	FrameBytesOptions = []int{500, 1000, 1465, 1850, 2331, 2953}
	TxFPSOptions      = []int{10, 15, 20, 24, 30, 55, 60}
)

const (
	DefaultFrameBytes = 2953
	DefaultTxFPS      = 60
	DefaultCycles     = 2
	DefaultScale      = 512
)

// BlockLength is the payload bytes per frame, once the header has taken its cut.
func BlockLength(frameBytes int) int { return frameBytes - protocol.HeaderLen }

// SourceBlockCount is the source blocks a payload splits into at this frame size.
func SourceBlockCount(payloadBytes, frameBytes int) int {
	bl := BlockLength(frameBytes)
	return (payloadBytes + bl - 1) / bl
}

// MinimumFrameBytes is the smallest bytes-per-frame that can carry this payload.
func MinimumFrameBytes(payloadBytes int) int {
	return (payloadBytes+MaxSourceBlocks-1)/MaxSourceBlocks + protocol.HeaderLen
}

// SmallestSufficientFrameSize names a value that is actually in the dropdown
// rather than the bare arithmetic minimum. Zero when nothing offered fits.
func SmallestSufficientFrameSize(payloadBytes int) int {
	minimum := MinimumFrameBytes(payloadBytes)
	for _, v := range FrameBytesOptions {
		if v >= minimum {
			return v
		}
	}
	return 0
}

// Options are the transmit knobs for one session.
type Options struct {
	FrameBytes int
	Cycles     int
	FPS        int
	Scale      int // target pixels per side of a rendered frame
}

// Session is one armed payload plus everything needed to render its frames.
type Session struct {
	ID        string
	Created   time.Time
	FileName  string
	MediaType string

	OriginalSize    int
	TransmittedSize int
	Compression     string

	Opts Options

	K          int
	BlockLen   int
	FrameCount int
	SessionID  uint16

	enc    *fountain.Encoder
	header protocol.Header

	mu    sync.Mutex
	gen   qrgen.Generator
	cache map[int][]byte
}

// New arms a payload: packs the container, sizes the fountain, and renders
// frame 0 to lock the QR version every later frame is forced to.
func New(fileName, mediaType string, data []byte, o Options) (*Session, error) {
	if o.FrameBytes == 0 {
		o.FrameBytes = DefaultFrameBytes
	}
	if o.Cycles <= 0 {
		o.Cycles = DefaultCycles
	}
	if o.FPS <= 0 {
		o.FPS = DefaultTxFPS
	}
	if o.Scale <= 0 {
		o.Scale = DefaultScale
	}
	if BlockLength(o.FrameBytes) <= 0 {
		return nil, fmt.Errorf("frame size %d is smaller than the %d-byte header",
			o.FrameBytes, protocol.HeaderLen)
	}

	packed, err := protocol.PackFile(fileName, mediaType, data)
	if err != nil {
		return nil, err
	}
	container := packed.Container

	if k := SourceBlockCount(len(container), o.FrameBytes); k > MaxSourceBlocks {
		fix := SmallestSufficientFrameSize(len(container))
		if fix == 0 {
			return nil, errors.New("payload is too large for any offered frame size")
		}
		return nil, fmt.Errorf("%d bytes needs %d blocks at %d bytes per frame (max %d) — use %d bytes per frame",
			len(container), k, o.FrameBytes, MaxSourceBlocks, fix)
	}

	sessionID, err := randomSessionID()
	if err != nil {
		return nil, err
	}
	blockLen := BlockLength(o.FrameBytes)
	enc := fountain.NewEncoder(container, blockLen, sessionID)

	s := &Session{
		ID:              randomID(),
		Created:         time.Now(),
		FileName:        protocol.SafeFileName(fileName),
		MediaType:       mediaType,
		OriginalSize:    packed.OriginalSize,
		TransmittedSize: packed.TransmittedSize,
		Compression:     packed.Compression,
		Opts:            o,
		K:               enc.K,
		BlockLen:        blockLen,
		FrameCount:      o.Cycles * fountain.CycleLength(enc.K),
		SessionID:       sessionID,
		enc:             enc,
		header: protocol.Header{
			SessionID:  sessionID,
			K:          uint16(enc.K),
			BlockLen:   uint16(blockLen),
			TotalLen:   uint32(len(container)),
			PayloadFNV: protocol.FNV1a(container),
			// Plain v3 frames, same as the live sender: nothing sets a flag bit.
			Flags: 0,
		},
		cache: map[int][]byte{},
	}
	if _, err := s.FramePNG(0); err != nil {
		return nil, err
	}
	return s, nil
}

// WireFrame returns the raw bytes of frame n as a receiver would see them.
func (s *Session) WireFrame(n int) []byte {
	seq := uint32(n)
	h := s.header
	h.Seq = seq
	return protocol.PackFrame(h, s.enc.Encode(seq))
}

// FramePNG renders frame n, memoised — a looping player asks for the same
// frames over and over, and re-deflating a full-scale raster each time is the
// one thing that would make the stream stutter.
func (s *Session) FramePNG(n int) ([]byte, error) {
	if n < 0 || n >= s.FrameCount {
		return nil, fmt.Errorf("frame %d out of range (0..%d)", n, s.FrameCount-1)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.cache[n]; ok {
		return b, nil
	}
	b, err := s.gen.PNG(s.WireFrame(n), s.Opts.Scale)
	if err != nil {
		return nil, err
	}
	s.cache[n] = b
	return b, nil
}

// QRVersion is the locked QR version for this session's frames.
func (s *Session) QRVersion() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen.Version()
}

func randomID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// randomSessionID draws the 16-bit stream id. A collision across a restart is
// rare but real; the receiver's stream identity covers every constant header
// field, not just this one, so a collision does not corrupt a decode.
func randomSessionID() (uint16, error) {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b[:]), nil
}

// ZipEntry is one file destined for a multi-file bundle.
type ZipEntry struct {
	Name string
	Data []byte
}

// BundleZip packs several files into one deflate zip. A folder upload is many
// files but the wire format carries exactly one container, so the bundle is
// what gets transferred and the receiver unpacks it on the far side.
func BundleZip(entries []ZipEntry) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := zw.Create(e.Name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(e.Data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Store keeps armed sessions in memory and drops them when they go stale.
type Store struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	ttl      time.Duration
}

// NewStore returns a store that evicts sessions untouched for ttl.
func NewStore(ttl time.Duration) *Store {
	return &Store{sessions: map[string]*Session{}, ttl: ttl}
}

// Put adds a session and sweeps expired ones.
func (st *Store) Put(s *Session) {
	st.mu.Lock()
	defer st.mu.Unlock()
	cutoff := time.Now().Add(-st.ttl)
	for id, old := range st.sessions {
		if old.Created.Before(cutoff) {
			delete(st.sessions, id)
		}
	}
	st.sessions[s.ID] = s
}

// Get returns a session by id.
func (st *Store) Get(id string) (*Session, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	s, ok := st.sessions[id]
	return s, ok
}
