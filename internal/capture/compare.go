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
// is interacting. MaxSilence guarantees a frame is periodically retained.
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
	maxSilence := c.MaxSilence
	if maxSilence <= 0 {
		maxSilence = 10 * time.Second
	}
	forceKeep := exists && capturedAt.Sub(previous.lastKept) >= maxSilence
	if exists && !forceKeep && hash == previous.hash {
		c.mu.Unlock()
		return true, 1, nil
	}
	c.mu.Unlock()

	hist, err := sampledHistogram(bytes.NewReader(contents))
	if err != nil {
		return false, 0, fmt.Errorf("decode frame for comparison: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	previous, exists = c.states[displayID]
	forceKeep = exists && capturedAt.Sub(previous.lastKept) >= maxSilence
	similarity := 0.0
	duplicate := false
	if exists && !forceKeep {
		similarity = histogramSimilarity(previous.hist, hist)
		threshold := c.IdleThreshold
		if threshold <= 0 || threshold > 1 {
			threshold = 0.985
		}
		if inputActive {
			threshold = c.ActiveThreshold
			if threshold <= 0 || threshold > 1 {
				threshold = 0.995
			}
		}
		duplicate = similarity >= threshold
	}
	if !duplicate {
		c.states[displayID] = frameState{hash: hash, hist: hist, lastKept: capturedAt}
	}
	return duplicate, similarity, nil
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
