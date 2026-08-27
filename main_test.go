package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path"
	"sort"
	"testing"
	"time"

	"github.com/cicdata-io/qr-server/internal/fountain"
	"github.com/cicdata-io/qr-server/internal/protocol"
	"github.com/cicdata-io/qr-server/internal/session"
)

// armRequest mirrors what the page sends: each file plus its relative path in
// a parallel "paths" field, since the multipart filename loses directories.
func armRequest(t *testing.T, files map[string][]byte, fields map[string]string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		w, err := mw.CreateFormFile("files", path.Base(name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(files[name]); err != nil {
			t.Fatal(err)
		}
		if err := mw.WriteField("paths", name); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/arm", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r
}

func newTestServer() *server { return &server{store: session.NewStore(time.Minute)} }

// TestArmAndDecodeStream is the end-to-end web path: upload, then reconstruct
// the file from nothing but the frames the server serves.
func TestArmAndDecodeStream(t *testing.T) {
	src := bytes.Repeat([]byte("web app optical payload "), 500)
	s := newTestServer()

	w := httptest.NewRecorder()
	s.handleArm(w, armRequest(t, map[string][]byte{"notes.txt": src},
		map[string]string{"frameBytes": "1000", "cycles": "1"}))
	if w.Code != http.StatusOK {
		t.Fatalf("arm returned %d: %s", w.Code, w.Body)
	}
	var got sessionInfo
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.FileName != "notes.txt" || got.Compression != "gzip" || got.FrameCount != 2*got.K {
		t.Fatalf("unexpected session info: %+v", got)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/sessions/{id}/wire/{n}", s.handleFrameBin)
	mux.HandleFunc("GET /api/sessions/{id}/frames/{n}", s.handleFramePNG)

	var dec *fountain.Decoder
	for n := 0; n < got.FrameCount; n++ {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET",
			fmt.Sprintf("/api/sessions/%s/wire/%d", got.ID, n), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("frame %d returned %d", n, rec.Code)
		}
		h, block, err := protocol.ParseFrame(rec.Body.Bytes())
		if err != nil {
			t.Fatalf("frame %d: %v", n, err)
		}
		if dec == nil {
			dec = fountain.NewDecoder(int(h.K), int(h.BlockLen), h.SessionID, int(h.TotalLen))
		}
		dec.AddFrame(h.Seq, block)
	}
	if !dec.IsComplete() {
		t.Fatal("a full cycle served over HTTP did not decode")
	}
	u, err := protocol.UnpackFile(dec.Assemble())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(u.Bytes, src) {
		t.Fatal("HTTP-served frames did not reconstruct the upload")
	}

	// And the PNG a browser fetches is a real image at the locked geometry.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET",
		fmt.Sprintf("/api/sessions/%s/frames/0", got.ID), nil))
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content type %q", ct)
	}
	img, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != b.Dy() || b.Dx() == 0 {
		t.Fatalf("frame image is %v", b)
	}
}

// TestArmBundlesFolder: several files become one zip, because the wire format
// carries exactly one container.
func TestArmBundlesFolder(t *testing.T) {
	s := newTestServer()
	w := httptest.NewRecorder()
	s.handleArm(w, armRequest(t, map[string][]byte{
		"pics/a.txt":     bytes.Repeat([]byte("a"), 900),
		"pics/sub/b.txt": bytes.Repeat([]byte("b"), 900),
	}, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("arm returned %d: %s", w.Code, w.Body)
	}
	var got sessionInfo
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !got.Bundled || got.FileCount != 2 || got.FileName != "pics.zip" {
		t.Fatalf("expected a pics.zip bundle of 2, got %+v", got)
	}
}

func TestArmRejectsEmptyRequest(t *testing.T) {
	s := newTestServer()
	w := httptest.NewRecorder()
	s.handleArm(w, armRequest(t, nil, nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// TestZipNameRefusesEscape: the relative path comes from the browser, so a
// bundle must not be able to write outside itself when unpacked.
func TestZipNameRefusesEscape(t *testing.T) {
	for in, want := range map[string]string{
		"pics/a.png":       "pics/a.png",
		"../../etc/passwd": "etc/passwd",
		`win\dir\file.txt`: "win/dir/file.txt",
		"./a/../b.txt":     "a/b.txt",
		"":                 "transfer.bin",
		"a\x00b/c\nd.txt":  "ab/cd.txt",
	} {
		if got := zipName(in); got != want {
			t.Errorf("zipName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOptionsEndpoint(t *testing.T) {
	w := httptest.NewRecorder()
	newTestServer().handleOptions(w, httptest.NewRequest("GET", "/api/options", nil))
	var body map[string]any
	if err := json.NewDecoder(io.Reader(w.Body)).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["wireVersion"].(float64) != protocol.WireVersion {
		t.Fatalf("wire version not advertised: %v", body["wireVersion"])
	}
}
