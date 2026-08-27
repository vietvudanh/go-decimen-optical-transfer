// qr-server serves the sending half of Decimen optical transfer as a web app:
// upload a file or a folder, and the browser plays back the QR carousel that a
// Decimen receiver (or any client speaking wire v3) decodes with a camera.
//
// The wire format, container and fountain code are ports of the desktop app's
// shared/ modules — see internal/protocol and internal/fountain. Frames are
// byte-identical to what the original sender emits.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cicdata-io/qr-server/internal/protocol"
	"github.com/cicdata-io/qr-server/internal/session"
	"github.com/cicdata-io/qr-server/web"
)

// maxUploadBytes bounds a single arm request. The container limit is 64 MB, and
// a folder upload arrives as its unzipped parts, so the request ceiling has to
// sit above the payload ceiling.
const maxUploadBytes = 96 << 20

type server struct {
	store *session.Store
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	ttl := flag.Duration("ttl", 2*time.Hour, "how long an armed session is kept in memory")
	flag.Parse()

	s := &server{store: session.NewStore(*ttl)}

	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(web.FS))
	mux.HandleFunc("GET /api/options", s.handleOptions)
	mux.HandleFunc("POST /api/arm", s.handleArm)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleSession)
	mux.HandleFunc("GET /api/sessions/{id}/frames/{n}", s.handleFramePNG)
	mux.HandleFunc("GET /api/sessions/{id}/wire/{n}", s.handleFrameBin)

	log.Printf("qr-server listening on %s", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *server) handleOptions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"frameBytesOptions": session.FrameBytesOptions,
		"fpsOptions":        session.TxFPSOptions,
		"defaults": map[string]int{
			"frameBytes": session.DefaultFrameBytes,
			"fps":        session.DefaultTxFPS,
			"cycles":     session.DefaultCycles,
			"scale":      session.DefaultScale,
		},
		"maxFileBytes": protocol.MaxFileBytes,
		"wireVersion":  protocol.WireVersion,
	})
}

type sessionInfo struct {
	ID              string `json:"id"`
	FileName        string `json:"fileName"`
	MediaType       string `json:"mediaType"`
	OriginalSize    int    `json:"originalSize"`
	TransmittedSize int    `json:"transmittedSize"`
	Compression     string `json:"compression"`
	K               int    `json:"k"`
	BlockLen        int    `json:"blockLen"`
	FrameBytes      int    `json:"frameBytes"`
	FrameCount      int    `json:"frameCount"`
	Cycles          int    `json:"cycles"`
	FPS             int    `json:"fps"`
	Scale           int    `json:"scale"`
	QRVersion       int    `json:"qrVersion"`
	WireVersion     int    `json:"wireVersion"`
	Bundled         bool   `json:"bundled"`
	FileCount       int    `json:"fileCount"`
}

func info(sess *session.Session, bundled bool, fileCount int) sessionInfo {
	return sessionInfo{
		ID: sess.ID, FileName: sess.FileName, MediaType: sess.MediaType,
		OriginalSize: sess.OriginalSize, TransmittedSize: sess.TransmittedSize,
		Compression: sess.Compression, K: sess.K, BlockLen: sess.BlockLen,
		FrameBytes: sess.Opts.FrameBytes, FrameCount: sess.FrameCount,
		Cycles: sess.Opts.Cycles, FPS: sess.Opts.FPS, Scale: sess.Opts.Scale,
		QRVersion: sess.QRVersion(), WireVersion: protocol.WireVersion,
		Bundled: bundled, FileCount: fileCount,
	}
}

// handleArm packs the upload into a container and arms a session. Several files
// (a folder pick) are bundled into one zip first: the wire format carries
// exactly one container, and the receiver unpacks the zip on the far side.
func (s *server) handleArm(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("no files in the request"))
		return
	}

	// Go's multipart reader strips directories from the filename field, so the
	// browser sends webkitRelativePath separately, in upload order — that is
	// the only way a folder pick keeps its tree.
	paths := r.MultipartForm.Value["paths"]

	entries := make([]session.ZipEntry, 0, len(files))
	for i, fh := range files {
		f, err := fh.Open()
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		data, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		name := fh.Filename
		if i < len(paths) && paths[i] != "" {
			name = paths[i]
		}
		entries = append(entries, session.ZipEntry{Name: zipName(name), Data: data})
	}
	// A folder pick arrives in whatever order the browser walked it; sorting
	// makes the same folder produce the same bundle bytes twice running.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

	var (
		name      string
		mediaType string
		payload   []byte
		bundled   bool
	)
	if len(entries) == 1 {
		name = path.Base(entries[0].Name)
		payload = entries[0].Data
		mediaType = protocol.MediaTypeForName(name)
	} else {
		bundled = true
		name = bundleName(entries) + ".zip"
		mediaType = "application/zip"
		var err error
		payload, err = session.BundleZip(entries)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}

	opts := session.Options{
		FrameBytes: formInt(r, "frameBytes", session.DefaultFrameBytes),
		Cycles:     formInt(r, "cycles", session.DefaultCycles),
		FPS:        formInt(r, "fps", session.DefaultTxFPS),
		Scale:      formInt(r, "scale", session.DefaultScale),
	}
	sess, err := session.New(name, mediaType, payload, opts)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.store.Put(sess)
	writeJSON(w, http.StatusOK, info(sess, bundled, len(entries)))
}

func (s *server) handleSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.store.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no such session"))
		return
	}
	writeJSON(w, http.StatusOK, info(sess, strings.HasSuffix(sess.FileName, ".zip"), 0))
}

func (s *server) handleFramePNG(w http.ResponseWriter, r *http.Request) {
	sess, n, ok := s.frameRequest(w, r)
	if !ok {
		return
	}
	png, err := sess.FramePNG(n)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	// Frames are immutable for the life of the session, so the browser can
	// keep every one it has already fetched.
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	_, _ = w.Write(png)
}

// handleFrameBin serves a frame's raw wire bytes — the golden-vector view, for
// checking this server against another implementation without a camera.
func (s *server) handleFrameBin(w http.ResponseWriter, r *http.Request) {
	sess, n, ok := s.frameRequest(w, r)
	if !ok {
		return
	}
	if n >= sess.FrameCount {
		writeErr(w, http.StatusNotFound, fmt.Errorf("frame %d out of range", n))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(sess.WireFrame(n))
}

func (s *server) frameRequest(w http.ResponseWriter, r *http.Request) (*session.Session, int, bool) {
	sess, ok := s.store.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no such session"))
		return nil, 0, false
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return nil, 0, false
	}
	return sess, n, true
}

func formInt(r *http.Request, key string, fallback int) int {
	if v, err := strconv.Atoi(r.FormValue(key)); err == nil {
		return v
	}
	return fallback
}

// zipName keeps the relative path a directory pick reports (webkitRelativePath)
// so the bundle rebuilds the folder tree, while refusing anything that could
// escape it when unpacked.
func zipName(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	parts := make([]string, 0, 8)
	for _, p := range strings.Split(name, "/") {
		if p == "" || p == "." || p == ".." {
			continue
		}
		parts = append(parts, protocol.SafeFileName(p))
	}
	if len(parts) == 0 {
		return "transfer.bin"
	}
	return strings.Join(parts, "/")
}

// bundleName names the zip after the common folder, or after the first file.
func bundleName(entries []session.ZipEntry) string {
	if i := strings.Index(entries[0].Name, "/"); i > 0 {
		root := entries[0].Name[:i]
		all := true
		for _, e := range entries {
			if !strings.HasPrefix(e.Name, root+"/") {
				all = false
				break
			}
		}
		if all {
			return root
		}
	}
	base := path.Base(entries[0].Name)
	return strings.TrimSuffix(base, filepath.Ext(base)) + "-bundle"
}
