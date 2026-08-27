package fountain

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/cicdata-io/qr-server/internal/protocol"
)

func joinInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ",")
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// TestFrameCompositionGolden holds the carousel to the JavaScript sender's
// output. Sender and receiver derive these subsets independently and never
// compare notes, so any disagreement here is a silent, total failure.
func TestFrameCompositionGolden(t *testing.T) {
	for _, line := range lines(t, "testdata/composition.txt") {
		f := strings.Fields(line)
		k, _ := strconv.Atoi(f[0])
		sid, _ := strconv.Atoi(f[1])
		seq, _ := strconv.ParseUint(f[2], 10, 32)
		want := ""
		if len(f) > 3 {
			want = f[3]
		}
		got := joinInts(FrameComposition(k, uint16(sid), uint32(seq)))
		if got != want {
			t.Errorf("FrameComposition(k=%d, sid=%d, seq=%d) = %s, want %s", k, sid, seq, got, want)
		}
	}
}

// TestSolitonGolden pins dlog, the CDF and the v1 soliton subsets. dlog exists
// because Math.log is implementation-approximated: a 1-ulp disagreement shifts
// a CDF entry and flips a sampled degree.
func TestSolitonGolden(t *testing.T) {
	cdfs := map[int][]float64{}
	for _, line := range lines(t, "testdata/soliton.txt") {
		f := strings.Fields(line)
		switch f[0] {
		case "dlog":
			x, _ := strconv.ParseFloat(f[1], 64)
			want, _ := strconv.ParseFloat(f[2], 64)
			if got := DLog(x); got != want {
				t.Errorf("DLog(%v) = %v, want %v (bit-exact required)", x, got, want)
			}
		case "cdf":
			k, _ := strconv.Atoi(f[1])
			cdf := SolitonCdf(k)
			cdfs[k] = cdf
			for i, idx := range []int{0, k / 2, k - 1} {
				want, _ := strconv.ParseFloat(f[2+i], 64)
				if cdf[idx] != want {
					t.Errorf("SolitonCdf(%d)[%d] = %v, want %v", k, idx, cdf[idx], want)
				}
			}
		case "idx":
			k, _ := strconv.Atoi(f[1])
			seq, _ := strconv.ParseUint(f[2], 10, 32)
			want := ""
			if len(f) > 3 {
				want = f[3]
			}
			got := joinInts(FrameIndices(k, cdfs[k], 7, uint32(seq)))
			if got != want {
				t.Errorf("FrameIndices(k=%d, seq=%d) = %s, want %s", k, seq, got, want)
			}
		}
	}
}

func TestCycleLength(t *testing.T) {
	if CycleLength(179) != 358 {
		t.Fatal("a cycle is a sweep of k plus k repair frames")
	}
}

// TestSystematicSweep: the first k frames of a cycle carry exactly block i, so
// a receiver catching a whole sweep completes in k frames — zero overhead.
func TestSystematicSweep(t *testing.T) {
	const k = 40
	for i := 0; i < k; i++ {
		got := FrameComposition(k, 1234, uint32(i))
		if len(got) != 1 || got[0] != i {
			t.Fatalf("frame %d covers %v, want [%d]", i, got, i)
		}
	}
	for seq := k; seq < 2*k; seq++ {
		d := len(FrameComposition(k, 1234, uint32(seq)))
		if d < repairDegreeMin || d > repairDegreeMax {
			t.Fatalf("repair frame %d has degree %d, want %d..%d",
				seq, d, repairDegreeMin, repairDegreeMax)
		}
	}
}

// TestRepairFramesVaryPerCycle: repair frames seed from the ABSOLUTE seq, so
// re-watching the carousel never replays them.
func TestRepairFramesVaryPerCycle(t *testing.T) {
	const k = 60
	a := joinInts(FrameComposition(k, 7, uint32(k+3)))
	b := joinInts(FrameComposition(k, 7, uint32(3*k+3)))
	if a == b {
		t.Fatalf("cycle 1 and cycle 2 repair frames identical: %s", a)
	}
}

// TestTransferRoundTrip is the end-to-end harness, and the only test that
// catches a header field read from the wrong offset: per-layer tests pass
// happily when PackFrame and ParseFrame agree with each other but not with the
// wire. It drives incompressible data through container -> fountain -> framed
// wire -> back over deterministic ~15% frame loss.
func TestTransferRoundTrip(t *testing.T) {
	src := make([]byte, 300*1024)
	rng := rand.New(rand.NewSource(1))
	rng.Read(src)

	packed, err := protocol.PackFile("random.bin", "application/octet-stream", src)
	if err != nil {
		t.Fatal(err)
	}
	container := packed.Container
	if packed.Compression != "none" {
		t.Fatalf("incompressible data was gzipped: %s", packed.Compression)
	}

	const frameBytes = 2953
	blockLen := frameBytes - protocol.HeaderLen
	sessionID := uint16(0xbeef)
	enc := NewEncoder(container, blockLen, sessionID)
	header := protocol.Header{
		SessionID: sessionID, K: uint16(enc.K), BlockLen: uint16(blockLen),
		TotalLen: uint32(len(container)), PayloadFNV: protocol.FNV1a(container),
	}

	// The receiver learns everything from the frames alone: no handshake, no
	// shared state with the encoder.
	var dec *Decoder
	loss := rand.New(rand.NewSource(2))
	sent := 0
	for seq := uint32(0); !(dec != nil && dec.IsComplete()); seq++ {
		if seq > uint32(20*enc.K) {
			t.Fatal("did not converge")
		}
		h := header
		h.Seq = seq
		wire := protocol.PackFrame(h, enc.Encode(seq))
		if len(wire) != frameBytes {
			t.Fatalf("frame %d is %d bytes, want the %d-byte budget", seq, len(wire), frameBytes)
		}
		if loss.Float64() < 0.15 {
			continue
		}
		sent++
		gotHeader, block, err := protocol.ParseFrame(wire)
		if err != nil {
			t.Fatal(err)
		}
		if dec == nil {
			dec = NewDecoder(int(gotHeader.K), int(gotHeader.BlockLen),
				gotHeader.SessionID, int(gotHeader.TotalLen))
		}
		dec.AddFrame(gotHeader.Seq, block)
	}

	got := dec.Assemble()
	if protocol.FNV1a(got) != header.PayloadFNV {
		t.Fatal("recovered container does not match payloadFnv")
	}
	unpacked, err := protocol.UnpackFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(unpacked.Bytes, src) {
		t.Fatal("recovered bytes differ from the source file")
	}
	if overhead := float64(sent) / float64(enc.K); overhead > 1.3 {
		t.Fatalf("fountain overhead %.2fx exceeds 1.3x", overhead)
	}
	fmt.Fprintf(os.Stderr, "round trip: k=%d, %d frames accepted (%.2fx)\n",
		enc.K, sent, float64(sent)/float64(enc.K))
}

func TestEncoderXORsItsSubset(t *testing.T) {
	payload := make([]byte, 1000)
	rand.New(rand.NewSource(3)).Read(payload)
	const blockLen = 128
	enc := NewEncoder(payload, blockLen, 42)
	if want := int(math.Ceil(1000.0 / blockLen)); enc.K != want {
		t.Fatalf("k = %d, want %d", enc.K, want)
	}
	seq := uint32(enc.K + 2) // a repair frame
	want := make([]byte, blockLen)
	blocks := make([]byte, enc.K*blockLen)
	copy(blocks, payload)
	for _, b := range FrameComposition(enc.K, 42, seq) {
		for i := range want {
			want[i] ^= blocks[b*blockLen+i]
		}
	}
	if !bytes.Equal(enc.Encode(seq), want) {
		t.Fatal("encoded block is not the XOR of its subset")
	}
}
