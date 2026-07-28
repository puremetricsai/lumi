package capture

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"image"
	_ "image/jpeg"
	"io"
	"os"
	"sync"
	"time"
)

const histogramBins = 16

type FrameComparer struct {
	IdleThreshold   float64
	ActiveThreshold float64
	MaxSilence      time.Duration
	ExactSilence    time.Duration

	mu     sync.Mutex
	states map[uint32]frameState
}

type frameState struct {
	hash     [sha256.Size]byte
	hist     [histogramBins * 3]float64
	lastKept time.Time
}

// Duplicate reports whether a frame is visually redundant. It combines a
// byte-hash fast path with a downsampled RGB histogram. Active input raises
// the similarity threshold, retaining more subtle UI changes while the user
// is interacting. A frame is still periodically retained: MaxSilence bounds the
// gap for a frame that changed but scored as similar, and the longer
// ExactSilence bounds it for a frame whose bytes are identical.
func (c *FrameComparer) Duplicate(path string, displayID uint32, inputActive bool, capturedAt time.Time) (bool, float64, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return false, 0, fmt.Errorf("read frame for comparison: %w", err)
	}
	hash := sha256.Sum256(contents)

	c.mu.Lock()
	if c.states == nil {
		c.states = make(map[uint32]frameState)
	}
	previous, exists := c.states[displayID]
	if exists && hash == previous.hash && capturedAt.Sub(previous.lastKept) < c.exactSilence() {
		c.mu.Unlock()
		return true, 1, nil
	}
	c.mu.Unlock()

	hist, err := sampledHistogram(bytes.NewReader(contents))
	if err != nil {
		return false, 0, fmt.Errorf("decode frame for comparison: %w", err)
	}

	// The lock was released for the decode, so the identity check is repeated
	// rather than assumed: FrameComparer is shared across displays and its state
	// may have advanced. Identity is re-tested first so an unchanged frame is
	// never force-kept by the shorter near-duplicate deadline.
	c.mu.Lock()
	defer c.mu.Unlock()
	previous, exists = c.states[displayID]
	identical := exists && hash == previous.hash
	similarity := 0.0
	duplicate := false
	if exists && capturedAt.Sub(previous.lastKept) < c.silence(identical) {
		if identical {
			similarity = 1
			duplicate = true
		} else {
			similarity = histogramSimilarity(previous.hist, hist)
			duplicate = similarity >= c.threshold(inputActive)
		}
	}
	if !duplicate {
		c.states[displayID] = frameState{hash: hash, hist: hist, lastKept: capturedAt}
	}
	return duplicate, similarity, nil
}

func (c *FrameComparer) maxSilence() time.Duration {
	if c.MaxSilence <= 0 {
		return 10 * time.Second
	}
	return c.MaxSilence
}

// exactSilence bounds how long a display may go unrecorded while its frames are
// byte-identical. Identical bytes carry no new information, so re-indexing them
// on the near-duplicate deadline is pure waste; the longer deadline still leaves
// a periodic presence marker for later queries. It never falls below MaxSilence, so
// an unchanged frame is never retained more eagerly than a changed one.
func (c *FrameComparer) exactSilence() time.Duration {
	silence := c.ExactSilence
	if silence <= 0 {
		silence = 5 * time.Minute
	}
	return max(silence, c.maxSilence())
}

func (c *FrameComparer) silence(identical bool) time.Duration {
	if identical {
		return c.exactSilence()
	}
	return c.maxSilence()
}

func (c *FrameComparer) threshold(inputActive bool) float64 {
	if inputActive {
		if c.ActiveThreshold <= 0 || c.ActiveThreshold > 1 {
			return 0.995
		}
		return c.ActiveThreshold
	}
	if c.IdleThreshold <= 0 || c.IdleThreshold > 1 {
		return 0.985
	}
	return c.IdleThreshold
}

func sampledHistogram(reader io.Reader) ([histogramBins * 3]float64, error) {
	var histogram [histogramBins * 3]float64
	img, _, err := image.Decode(reader)
	if err != nil {
		return histogram, err
	}
	bounds := img.Bounds()
	stepX := max(1, bounds.Dx()/160)
	stepY := max(1, bounds.Dy()/90)
	samples := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y += stepY {
		for x := bounds.Min.X; x < bounds.Max.X; x += stepX {
			r, g, b, _ := img.At(x, y).RGBA()
			histogram[int(r>>12)]++
			histogram[histogramBins+int(g>>12)]++
			histogram[2*histogramBins+int(b>>12)]++
			samples++
		}
	}
	if samples == 0 {
		return histogram, fmt.Errorf("image has empty bounds %v", bounds)
	}
	denominator := float64(samples * 3)
	for i := range histogram {
		histogram[i] /= denominator
	}
	return histogram, nil
}

func histogramSimilarity(left, right [histogramBins * 3]float64) float64 {
	similarity := 0.0
	for i := range left {
		similarity += min(left[i], right[i])
	}
	return similarity
}
