// Package protocol is a Go port of the Decimen optical-transfer wire format
// (wire v3) and its file container. Ported from shared/protocol.ts of the
// decimen-optical-transfer desktop/web app; the bytes on the wire are
// byte-identical, so a Decimen receiver decodes what this server renders.
//
// Frame layout (little-endian), 22-byte header followed by blockLen bytes:
//
//	 0  u8   magic 0xD1
//	 1  u8   magic 0xC3
//	 2  u8   version (3)
//	 3  u8   flags   (0x0F must-understand, 0xF0 ignorable)
//	 4  u16  sessionId
//	 6  u32  seq
//	10  u16  k          source block count
//	12  u16  blockLen   payload bytes per frame
//	14  u32  totalLen   container length
//	18  u32  payloadFnv FNV-1a of the whole container
package protocol

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	// HeaderLen is the frame header size in bytes.
	HeaderLen = 22

	magic0 = 0xd1
	magic1 = 0xc3

	// WireVersion is the wire format version this build speaks.
	WireVersion = 3

	// CriticalFlags are the flag bits a receiver must understand.
	CriticalFlags = 0x0f

	// FlagEncrypted marks an encrypted container. Never set by this build.
	FlagEncrypted = 0x01

	// MaxFileBytes is the largest payload the format will carry.
	MaxFileBytes = 64 * 1024 * 1024

	fileHeaderLen = 49
)

var fileMagic = [4]byte{0x44, 0x43, 0x46, 0x32} // "DCF2"

// Header is a parsed frame header. Seq is the only field that varies within
// a stream.
type Header struct {
	SessionID  uint16
	Seq        uint32
	K          uint16
	BlockLen   uint16
	TotalLen   uint32
	PayloadFNV uint32
	Flags      uint8
}

// PackFrame serialises a header plus one fountain block into wire bytes.
func PackFrame(h Header, block []byte) []byte {
	out := make([]byte, HeaderLen+len(block))
	out[0] = magic0
	out[1] = magic1
	out[2] = WireVersion
	out[3] = h.Flags
	binary.LittleEndian.PutUint16(out[4:], h.SessionID)
	binary.LittleEndian.PutUint32(out[6:], h.Seq)
	binary.LittleEndian.PutUint16(out[10:], h.K)
	binary.LittleEndian.PutUint16(out[12:], h.BlockLen)
	binary.LittleEndian.PutUint32(out[14:], h.TotalLen)
	binary.LittleEndian.PutUint32(out[18:], h.PayloadFNV)
	copy(out[HeaderLen:], block)
	return out
}

// ParseFrame is the inverse of PackFrame. It exists so the round-trip can be
// tested against the golden vectors rather than only against itself.
func ParseFrame(b []byte) (Header, []byte, error) {
	if len(b) < 4 || b[0] != magic0 || b[1] != magic1 {
		return Header{}, nil, errors.New("foreign frame")
	}
	if b[2] != WireVersion {
		return Header{}, nil, fmt.Errorf("wire version %d, want %d", b[2], WireVersion)
	}
	if b[3]&CriticalFlags != 0 {
		return Header{}, nil, fmt.Errorf("unsupported critical flags %#x", b[3]&CriticalFlags)
	}
	if len(b) <= HeaderLen {
		return Header{}, nil, errors.New("malformed frame")
	}
	h := Header{
		Flags:      b[3],
		SessionID:  binary.LittleEndian.Uint16(b[4:]),
		Seq:        binary.LittleEndian.Uint32(b[6:]),
		K:          binary.LittleEndian.Uint16(b[10:]),
		BlockLen:   binary.LittleEndian.Uint16(b[12:]),
		TotalLen:   binary.LittleEndian.Uint32(b[14:]),
		PayloadFNV: binary.LittleEndian.Uint32(b[18:]),
	}
	if h.K == 0 || h.BlockLen == 0 || h.TotalLen == 0 || len(b) != HeaderLen+int(h.BlockLen) {
		return Header{}, nil, errors.New("malformed frame")
	}
	return h, b[HeaderLen:], nil
}

// FNV1a is the container checksum carried in every frame header.
func FNV1a(b []byte) uint32 {
	h := uint32(0x811c9dc5)
	for _, c := range b {
		h ^= uint32(c)
		h *= 0x01000193
	}
	return h
}

// Splitmix32 returns the deterministic PRNG the fountain seeds from. Integer
// ops only, so it agrees bit-for-bit with the JavaScript sender.
func Splitmix32(seed uint32) func() uint32 {
	s := seed
	return func() uint32 {
		s += 0x9e3779b9
		t := s ^ (s >> 16)
		t *= 0x21f0aaad
		t ^= t >> 15
		t *= 0x735a2d97
		t ^= t >> 15
		return t
	}
}

// Packed is the result of wrapping a file in the transfer container.
type Packed struct {
	Container       []byte
	Compression     string // "none" or "gzip"
	OriginalSize    int
	TransmittedSize int
}

var ctrlChars = regexp.MustCompile(`[\x00-\x1f\x7f]`)

// SafeFileName reduces a name to a bare basename with control characters
// stripped. Applied on both ends of the channel.
func SafeFileName(name string) string {
	base := name
	if i := strings.LastIndexAny(base, `\/`); i >= 0 {
		base = base[i+1:]
	}
	cleaned := strings.TrimSpace(ctrlChars.ReplaceAllString(base, ""))
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return "transfer.bin"
	}
	return cleaned
}

var precompressedTypes = map[string]bool{
	"application/gzip": true, "application/java-archive": true,
	"application/vnd.rar": true, "application/x-7z-compressed": true,
	"application/x-brotli": true, "application/x-bzip": true,
	"application/x-bzip2": true, "application/x-gzip": true,
	"application/x-lzma": true, "application/x-rar-compressed": true,
	"application/x-xz": true, "application/x-zip-compressed": true,
	"application/zip": true, "application/zstd": true,
}

var (
	compressibleImages = regexp.MustCompile(`^image/(bmp|x-ms-bmp|svg\+xml|tiff|x-icon|vnd\.microsoft\.icon)$`)
	compressibleAudio  = regexp.MustCompile(`^audio/(wav|x-wav|wave|vnd\.wave|aiff|x-aiff|basic|l16)$`)
)

// IsPrecompressedType reports whether gzip would be a waste of time on this
// media type. Deliberately a list, not a heuristic: a wrong "skip" costs a few
// percent of transfer size, a wrong "try" costs a whole buffer.
func IsPrecompressedType(t string) bool {
	media := strings.ToLower(strings.TrimSpace(strings.SplitN(t, ";", 2)[0]))
	switch {
	case strings.HasPrefix(media, "video/"):
		return true
	case strings.HasPrefix(media, "image/"):
		return !compressibleImages.MatchString(media)
	case strings.HasPrefix(media, "audio/"):
		return !compressibleAudio.MatchString(media)
	case strings.HasPrefix(media, "application/vnd.openxmlformats-officedocument."),
		strings.HasPrefix(media, "application/vnd.oasis.opendocument."),
		strings.HasSuffix(media, "+zip"):
		return true
	}
	return precompressedTypes[media]
}

// PackFile wraps bytes in the DCF2 container: name, media type, optional gzip
// (applied only when it shrinks the payload) and the SHA-256 of the original.
func PackFile(name, mediaType string, data []byte) (*Packed, error) {
	if len(data) == 0 {
		return nil, errors.New("file is empty")
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("file exceeds the %d MB limit", MaxFileBytes/1024/1024)
	}
	nameBytes := []byte(SafeFileName(name))
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	typeBytes := []byte(mediaType)
	if len(nameBytes) > 0xffff || len(typeBytes) > 0xffff {
		return nil, errors.New("file name or type too long")
	}

	sum := sha256.Sum256(data)

	// Too small to be worth a gzip header, or a format gzip cannot help with.
	transmitted := data
	compression := "none"
	if len(data) >= 768 && !IsPrecompressedType(mediaType) {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(data); err != nil {
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
		if buf.Len()+64 < len(data) {
			transmitted = buf.Bytes()
			compression = "gzip"
		}
	}

	out := make([]byte, fileHeaderLen+len(nameBytes)+len(typeBytes)+len(transmitted))
	copy(out, fileMagic[:])
	if compression == "gzip" {
		out[4] = 1
	}
	binary.LittleEndian.PutUint16(out[5:], uint16(len(nameBytes)))
	binary.LittleEndian.PutUint16(out[7:], uint16(len(typeBytes)))
	binary.LittleEndian.PutUint32(out[9:], uint32(len(data)))
	binary.LittleEndian.PutUint32(out[13:], uint32(len(transmitted)))
	copy(out[17:], sum[:])
	o := fileHeaderLen
	o += copy(out[o:], nameBytes)
	o += copy(out[o:], typeBytes)
	copy(out[o:], transmitted)

	return &Packed{
		Container:       out,
		Compression:     compression,
		OriginalSize:    len(data),
		TransmittedSize: len(transmitted),
	}, nil
}

// Unpacked mirrors PackFile's input, recovered from a container.
type Unpacked struct {
	Name        string
	Type        string
	Bytes       []byte
	SHA256      []byte
	Compression string
}

// UnpackFile is the inverse of PackFile, used by the round-trip tests.
func UnpackFile(c []byte) (*Unpacked, error) {
	if len(c) < fileHeaderLen {
		return nil, errors.New("container truncated")
	}
	if !bytes.Equal(c[:4], fileMagic[:]) {
		return nil, errors.New("container bad magic")
	}
	if c[4] > 1 {
		return nil, errors.New("container bad compression")
	}
	compression := "none"
	if c[4] == 1 {
		compression = "gzip"
	}
	nameLen := int(binary.LittleEndian.Uint16(c[5:]))
	typeLen := int(binary.LittleEndian.Uint16(c[7:]))
	fileLen := int(binary.LittleEndian.Uint32(c[9:]))
	txLen := int(binary.LittleEndian.Uint32(c[13:]))
	dataOffset := fileHeaderLen + nameLen + typeLen
	if fileLen == 0 || fileLen > MaxFileBytes || txLen == 0 || txLen > MaxFileBytes ||
		dataOffset+txLen != len(c) {
		return nil, errors.New("container length mismatch")
	}
	transmitted := c[dataOffset:]
	data := transmitted
	if compression == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(transmitted))
		if err != nil {
			return nil, err
		}
		// The declared size arrives over the optical channel like everything
		// else: a hint, never a bound. Read one byte past it and refuse.
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(newLimitReader(zr, fileLen+1)); err != nil {
			return nil, err
		}
		if buf.Len() > fileLen {
			return nil, errors.New("inflate overflow")
		}
		data = buf.Bytes()
	}
	if len(data) != fileLen {
		return nil, errors.New("decompressed length mismatch")
	}
	sum := sha256.Sum256(data)
	if !bytes.Equal(sum[:], c[17:49]) {
		return nil, errors.New("sha-256 mismatch")
	}
	name := SafeFileName(string(c[fileHeaderLen : fileHeaderLen+nameLen]))
	mediaType := string(c[fileHeaderLen+nameLen : dataOffset])
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return &Unpacked{
		Name: name, Type: mediaType, Bytes: data,
		SHA256: c[17:49], Compression: compression,
	}, nil
}

// MediaTypeForName guesses a container media type from a file extension.
func MediaTypeForName(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".txt", ".md", ".log":
		return "text/plain"
	case ".json":
		return "application/json"
	case ".pdf":
		return "application/pdf"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".zip":
		return "application/zip"
	}
	return "application/octet-stream"
}
