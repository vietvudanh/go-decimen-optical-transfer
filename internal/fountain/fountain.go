// Package fountain is a Go port of the systematic-carousel fountain code
// (shared/fountain.ts) that makes a one-way optical channel practical.
//
// The sender emits an endless carousel: a systematic sweep of all k blocks,
// then k mid-degree repair frames (XORs of pseudorandom block subsets derived
// deterministically from seq), then the next cycle. A receiver locking on
// anywhere rebuilds the file from ~k distinct frames at low loss, and repair
// frames patch what loss takes, in any order.
//
// Sender and receiver derive block subsets independently and never compare
// notes, so anything in this file is wire format: a change here is a breaking
// change against every receiver already in the field.
package fountain

import (
	"math"

	"github.com/cicdata-io/qr-server/internal/protocol"
)

const ln2 = 0.6931471805599453

// DLog is a deterministic natural log: exact-ops range reduction plus an
// atanh series. The JavaScript sender cannot use Math.log here — it is
// implementation-approximated, and a 1-ulp disagreement between two engines
// shifts a CDF entry and silently desynchronises the streams. Ported as-is so
// this implementation stays pinned to the same golden vectors.
func DLog(x float64) float64 {
	e := 0.0
	m := x
	for m >= 1.5 {
		m /= 2
		e++
	}
	for m < 0.75 {
		m *= 2
		e--
	}
	z := (m - 1) / (m + 1)
	z2 := z * z
	term := z
	sum := 0.0
	for n := 1; n <= 21; n += 2 {
		sum += term / float64(n)
		term *= z2
	}
	return e*ln2 + 2*sum
}

const (
	solitonC     = 0.1
	solitonDelta = 0.5
)

// SolitonCdf is the robust-soliton degree CDF for k source blocks. The v1
// stream used it; the carousel no longer emits it, but it stays pinned by its
// golden vectors in case a future format wants it back.
func SolitonCdf(k int) []float64 {
	cdf := make([]float64, k)
	if k == 1 {
		cdf[0] = 1
		return cdf
	}
	r := math.Max(1, solitonC*DLog(float64(k)/solitonDelta)*math.Sqrt(float64(k)))
	spike := math.Min(float64(k), math.Ceil(float64(k)/r))
	total := 0.0
	for d := 1; d <= k; d++ {
		var rho float64
		if d == 1 {
			rho = 1 / float64(k)
		} else {
			rho = 1 / float64(d*(d-1))
		}
		tau := 0.0
		if float64(d) < spike {
			tau = r / (float64(d) * float64(k))
		} else if float64(d) == spike {
			tau = (r * math.Max(0, DLog(r/solitonDelta))) / float64(k)
		}
		total += rho + tau
		cdf[d-1] = total
	}
	for i := range cdf {
		cdf[i] /= total
	}
	cdf[k-1] = 1
	return cdf
}

func frameSeed(sessionID uint16, seq uint32) uint32 {
	h := (uint32(sessionID) + 1) * 0x9e3779b1
	h ^= seq + 0x85ebca6b
	h = (h ^ (h >> 13)) * 0xc2b2ae35
	return h ^ (h >> 16)
}

// FrameIndices is the v1 soliton subset for frame seq. Kept, and exported, for
// the golden-vector tests; the carousel does not call it.
func FrameIndices(k int, cdf []float64, sessionID uint16, seq uint32) []int {
	rnd := protocol.Splitmix32(frameSeed(sessionID, seq))
	u := float64(rnd()) * math.Pow(2, -32)
	lo, hi := 0, k-1
	for lo < hi {
		mid := (lo + hi) >> 1
		if cdf[mid] >= u {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	d := min(k, lo+1)
	if d > k>>3 {
		// large degree: partial Fisher-Yates over an identity array
		scratch := make([]int, k)
		for i := range scratch {
			scratch[i] = i
		}
		out := make([]int, d)
		for i := 0; i < d; i++ {
			j := i + int(rnd()%uint32(k-i))
			scratch[i], scratch[j] = scratch[j], scratch[i]
			out[i] = scratch[i]
		}
		return out
	}
	return distinctSample(rnd, k, d)
}

// CycleLength is the frames per carousel cycle: one systematic sweep of all k
// blocks, then k repair frames for whatever the sweep dropped.
func CycleLength(k int) int { return 2 * k }

const (
	repairDegreeMin = 4
	repairDegreeMax = 24
)

// repairIndices draws a uniform mid-degree (4-24) subset, NOT robust-soliton.
// After a sweep the receiver holds most blocks, so a repair frame's effective
// degree is what remains after XORing the solved ones out; soliton's heavy
// degree-1/2 mass just re-sends blocks the sweep already delivered.
func repairIndices(k int, sessionID uint16, seq uint32) []int {
	rnd := protocol.Splitmix32(frameSeed(sessionID, seq))
	d := min(k, repairDegreeMin+int(rnd()%uint32(repairDegreeMax-repairDegreeMin+1)))
	return distinctSample(rnd, k, d)
}

// distinctSample draws d distinct indices below k, consuming the PRNG in the
// same order (and rejecting duplicates the same way) as the JS Set loop.
func distinctSample(rnd func() uint32, k, d int) []int {
	seen := make(map[int]bool, d)
	out := make([]int, 0, d)
	for len(out) < d {
		v := int(rnd() % uint32(k))
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// FrameComposition is the block subset for frame seq: systematic during the
// sweep, mid-degree repair after. Repair frames seed from the ABSOLUTE seq, so
// every cycle draws different subsets and re-watching never replays them.
func FrameComposition(k int, sessionID uint16, seq uint32) []int {
	pos := int(seq % uint32(CycleLength(k)))
	if pos < k {
		return []int{pos}
	}
	return repairIndices(k, sessionID, seq)
}

// Encoder turns a container into an endless stream of fountain blocks.
type Encoder struct {
	K         int
	BlockLen  int
	SessionID uint16
	blocks    []byte // K * BlockLen, zero-padded
}

// NewEncoder splits payload into ceil(len/blockLen) zero-padded source blocks.
func NewEncoder(payload []byte, blockLen int, sessionID uint16) *Encoder {
	k := max(1, (len(payload)+blockLen-1)/blockLen)
	blocks := make([]byte, k*blockLen)
	copy(blocks, payload)
	return &Encoder{K: k, BlockLen: blockLen, SessionID: sessionID, blocks: blocks}
}

// Encode returns the XOR of frame seq's block subset.
func (e *Encoder) Encode(seq uint32) []byte {
	out := make([]byte, e.BlockLen)
	for _, b := range FrameComposition(e.K, e.SessionID, seq) {
		src := e.blocks[b*e.BlockLen : (b+1)*e.BlockLen]
		for i := range out {
			out[i] ^= src[i]
		}
	}
	return out
}

// Decoder peels a payload back out of frames, in any order. The server does
// not receive, but the round-trip test is the only check that catches a header
// field read from the wrong offset, so the decoder ships beside the encoder.
type Decoder struct {
	K, BlockLen int
	SessionID   uint16
	TotalLen    int

	solved  [][]byte
	byBlock map[int]map[*pendingFrame]bool
	seen    map[uint32]bool

	SolvedCount int
	FramesNew   int
	FramesDup   int
	// FramesRedundant counts new seqs that carried no new information — every
	// block they covered was already solved. A progress bar fed raw FramesNew
	// inflates by exactly that fraction.
	FramesRedundant int
}

type pendingFrame struct {
	idx  map[int]bool
	data []byte
}

// NewDecoder builds a decoder for the stream a frame header describes.
func NewDecoder(k, blockLen int, sessionID uint16, totalLen int) *Decoder {
	return &Decoder{
		K: k, BlockLen: blockLen, SessionID: sessionID, TotalLen: totalLen,
		solved:  make([][]byte, k),
		byBlock: map[int]map[*pendingFrame]bool{},
		seen:    map[uint32]bool{},
	}
}

// IsComplete reports whether every source block has been recovered.
func (d *Decoder) IsComplete() bool { return d.SolvedCount >= d.K }

// AddFrame folds one received frame into the decoder.
func (d *Decoder) AddFrame(seq uint32, block []byte) {
	if d.seen[seq] {
		d.FramesDup++
		return
	}
	d.seen[seq] = true
	d.FramesNew++
	if d.IsComplete() {
		return
	}
	idx := map[int]bool{}
	for _, b := range FrameComposition(d.K, d.SessionID, seq) {
		idx[b] = true
	}
	data := make([]byte, d.BlockLen)
	copy(data, block[:d.BlockLen])
	for b := range idx {
		if s := d.solved[b]; s != nil {
			xorInto(data, s)
			delete(idx, b)
		}
	}
	switch len(idx) {
	case 0:
		d.FramesRedundant++
		return
	case 1:
		for b := range idx {
			d.resolve(b, data)
		}
		return
	}
	pf := &pendingFrame{idx: idx, data: data}
	for b := range idx {
		if d.byBlock[b] == nil {
			d.byBlock[b] = map[*pendingFrame]bool{}
		}
		d.byBlock[b][pf] = true
	}
}

// resolve runs the peeling cascade: solve a block, reduce every frame waiting
// on it, repeat. The cascade back-loads — blocks solved hockey-stick near the
// end while frame arrival is linear, so progress UX shows frames, not blocks.
func (d *Decoder) resolve(b0 int, w0 []byte) {
	queue := [][2]any{{b0, w0}}
	for len(queue) > 0 {
		item := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		b, w := item[0].(int), item[1].([]byte)
		if d.solved[b] != nil {
			continue
		}
		d.solved[b] = w
		d.SolvedCount++
		waiting := d.byBlock[b]
		if waiting == nil {
			continue
		}
		delete(d.byBlock, b)
		for pf := range waiting {
			xorInto(pf.data, w)
			delete(pf.idx, b)
			if len(pf.idx) == 1 {
				for r := range pf.idx {
					delete(d.byBlock[r], pf)
					if d.solved[r] == nil {
						queue = append(queue, [2]any{r, pf.data})
					}
				}
			}
		}
	}
}

// Assemble returns the recovered payload, or nil while blocks are missing.
func (d *Decoder) Assemble() []byte {
	if !d.IsComplete() {
		return nil
	}
	out := make([]byte, d.TotalLen)
	for b := 0; b < d.K; b++ {
		start := b * d.BlockLen
		n := min(d.BlockLen, d.TotalLen-start)
		if n > 0 {
			copy(out[start:], d.solved[b][:n])
		}
	}
	return out
}

func xorInto(dst, src []byte) {
	for i := range dst {
		dst[i] ^= src[i]
	}
}
