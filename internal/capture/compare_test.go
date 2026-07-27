package capture

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkSampledHistogram(b *testing.B) {
	img := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 82}); err != nil {
		b.Fatal(err)
	}
	contents := encoded.Bytes()
	b.ReportAllocs()
	b.SetBytes(int64(len(contents)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sampledHistogram(bytes.NewReader(contents)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFrameComparerExactDuplicate(b *testing.B) {
	path := filepath.Join(b.TempDir(), "frame.jpg")
	img := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		b.Fatal(err)
	}
	if err := jpeg.Encode(file, img, &jpeg.Options{Quality: 82}); err != nil {
		file.Close()
		b.Fatal(err)
	}
	if err := file.Close(); err != nil {
		b.Fatal(err)
	}
	comparer := &FrameComparer{MaxSilence: time.Hour}
	if _, _, err := comparer.Duplicate(path, 1, false, time.Unix(100, 0)); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		duplicate, _, err := comparer.Duplicate(path, 1, false, time.Unix(101, 0))
		if err != nil || !duplicate {
			b.Fatalf("duplicate=%v err=%v", duplicate, err)
		}
	}
}

func TestFrameComparerExactDuplicateAndMaxSilence(t *testing.T) {
	directory := t.TempDir()
	first := filepath.Join(directory, "first.jpg")
	second := filepath.Join(directory, "second.jpg")
	writeSplitJPEG(t, first, 200)
	contents, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	// A near-duplicate: it scores above the idle threshold, so only MaxSilence
	// can retain it.
	changed := filepath.Join(directory, "changed.jpg")
	writeSplitJPEG(t, changed, 199)

	comparer := &FrameComparer{MaxSilence: 10 * time.Second, ExactSilence: 5 * time.Minute}
	now := time.Unix(100, 0)
	if duplicate, _, err := comparer.Duplicate(first, 7, false, now); err != nil || duplicate {
		t.Fatalf("first frame duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, similarity, err := comparer.Duplicate(second, 7, false, now.Add(time.Second)); err != nil || !duplicate || similarity != 1 {
		t.Fatalf("exact frame duplicate=%v similarity=%v err=%v", duplicate, similarity, err)
	}
	// MaxSilence still guarantees a periodic frame for content that changed but
	// scored as a near-duplicate -- the subtitle/video/slide case.
	if duplicate, _, err := comparer.Duplicate(changed, 7, false, now.Add(10*time.Second)); err != nil || duplicate {
		t.Fatalf("max-silence frame duplicate=%v err=%v", duplicate, err)
	}
}

// A byte-identical frame carries no new information, so the near-duplicate
// deadline must not force it back into the index; only the longer ExactSilence
// checkpoint may.
func TestFrameComparerIdenticalFrameOutlivesMaxSilence(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "frame.jpg")
	writeSolidJPEG(t, path, color.RGBA{R: 30, G: 60, B: 90, A: 255})

	comparer := &FrameComparer{MaxSilence: 10 * time.Second, ExactSilence: 5 * time.Minute}
	now := time.Unix(100, 0)
	if duplicate, _, err := comparer.Duplicate(path, 7, false, now); err != nil || duplicate {
		t.Fatalf("seed frame duplicate=%v err=%v", duplicate, err)
	}
	for _, elapsed := range []time.Duration{10 * time.Second, time.Minute, 299 * time.Second} {
		duplicate, similarity, err := comparer.Duplicate(path, 7, false, now.Add(elapsed))
		if err != nil || !duplicate || similarity != 1 {
			t.Fatalf("identical frame after %s: duplicate=%v similarity=%v err=%v",
				elapsed, duplicate, similarity, err)
		}
	}
	if duplicate, _, err := comparer.Duplicate(path, 7, false, now.Add(5*time.Minute)); err != nil || duplicate {
		t.Fatalf("exact-silence checkpoint duplicate=%v err=%v", duplicate, err)
	}
	// The checkpoint re-anchors lastKept, so the gap stays bounded rather than
	// collapsing back to one frame per interval.
	if duplicate, _, err := comparer.Duplicate(path, 7, false, now.Add(5*time.Minute+2*time.Second)); err != nil || !duplicate {
		t.Fatalf("frame after checkpoint duplicate=%v err=%v", duplicate, err)
	}
}

// After an identical stretch outlives MaxSilence, near-duplicate suppression
// must resume normally instead of the stale deadline force-keeping every
// subsequent frame.
func TestFrameComparerResumesSuppressionAfterIdenticalStretch(t *testing.T) {
	directory := t.TempDir()
	base := filepath.Join(directory, "base.jpg")
	nearby := filepath.Join(directory, "nearby.jpg")
	alsoNearby := filepath.Join(directory, "also-nearby.jpg")
	writeSplitJPEG(t, base, 200)
	writeSplitJPEG(t, nearby, 199)
	writeSplitJPEG(t, alsoNearby, 198)

	comparer := &FrameComparer{MaxSilence: 10 * time.Second, ExactSilence: 5 * time.Minute}
	now := time.Unix(100, 0)
	if duplicate, _, err := comparer.Duplicate(base, 1, false, now); err != nil || duplicate {
		t.Fatalf("seed frame duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, _, err := comparer.Duplicate(base, 1, false, now.Add(30*time.Second)); err != nil || !duplicate {
		t.Fatalf("identical frame past MaxSilence duplicate=%v err=%v", duplicate, err)
	}
	// lastKept is still the seed, so this changed frame is past MaxSilence and
	// force-kept -- which is what re-anchors the deadline.
	if duplicate, _, err := comparer.Duplicate(nearby, 1, false, now.Add(32*time.Second)); err != nil || duplicate {
		t.Fatalf("changed frame past MaxSilence duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, _, err := comparer.Duplicate(alsoNearby, 1, false, now.Add(34*time.Second)); err != nil || !duplicate {
		t.Fatalf("suppression did not resume: duplicate=%v err=%v", duplicate, err)
	}
}

// A misconfigured ExactSilence must never retain an unchanged frame more
// eagerly than a changed one.
func TestFrameComparerExactSilenceNeverBelowMaxSilence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frame.jpg")
	writeSolidJPEG(t, path, color.RGBA{G: 120, A: 255})

	comparer := &FrameComparer{MaxSilence: time.Minute, ExactSilence: time.Second}
	now := time.Unix(100, 0)
	if duplicate, _, err := comparer.Duplicate(path, 1, false, now); err != nil || duplicate {
		t.Fatalf("seed frame duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, _, err := comparer.Duplicate(path, 1, false, now.Add(30*time.Second)); err != nil || !duplicate {
		t.Fatalf("identical frame duplicate=%v err=%v", duplicate, err)
	}
}

func TestFrameComparerKeepsDisplayStateIndependent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frame.jpg")
	writeSolidJPEG(t, path, color.RGBA{R: 10, A: 255})
	comparer := &FrameComparer{}
	now := time.Unix(100, 0)
	if duplicate, _, err := comparer.Duplicate(path, 1, false, now); err != nil || duplicate {
		t.Fatalf("display 1 duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, _, err := comparer.Duplicate(path, 2, false, now); err != nil || duplicate {
		t.Fatalf("display 2 inherited state: duplicate=%v err=%v", duplicate, err)
	}
}

func TestFrameComparerActivityUsesStricterThreshold(t *testing.T) {
	directory := t.TempDir()
	first := filepath.Join(directory, "first.jpg")
	second := filepath.Join(directory, "second.jpg")
	writeSplitJPEG(t, first, 200)
	writeSplitJPEG(t, second, 196)
	now := time.Unix(100, 0)

	idle := &FrameComparer{}
	if duplicate, _, err := idle.Duplicate(first, 1, false, now); err != nil || duplicate {
		t.Fatalf("seed idle comparer: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, similarity, err := idle.Duplicate(second, 1, false, now.Add(time.Second)); err != nil || !duplicate {
		t.Fatalf("idle comparison duplicate=%v similarity=%v err=%v", duplicate, similarity, err)
	}

	active := &FrameComparer{}
	if duplicate, _, err := active.Duplicate(first, 1, true, now); err != nil || duplicate {
		t.Fatalf("seed active comparer: duplicate=%v err=%v", duplicate, err)
	}
	if duplicate, similarity, err := active.Duplicate(second, 1, true, now.Add(time.Second)); err != nil || duplicate {
		t.Fatalf("active comparison duplicate=%v similarity=%v err=%v", duplicate, similarity, err)
	}
}

func writeSolidJPEG(t *testing.T, path string, fill color.Color) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 36))
	for y := 0; y < 36; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, fill)
		}
	}
	writeJPEG(t, path, img)
}

func writeSplitJPEG(t *testing.T, path string, split int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 400, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 400; x++ {
			fill := color.RGBA{R: 20, G: 20, B: 20, A: 255}
			if x < split {
				fill = color.RGBA{R: 230, G: 230, B: 230, A: 255}
			}
			img.Set(x, y, fill)
		}
	}
	writeJPEG(t, path, img)
}

func writeJPEG(t *testing.T, path string, img image.Image) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(file, img, &jpeg.Options{Quality: 90}); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
