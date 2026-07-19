package capture

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/puremetricsai/lumi/internal/config"
	"github.com/puremetricsai/lumi/internal/store"
)

type fakeScreen struct{ count atomic.Int64 }

func (f *fakeScreen) Capture(_ context.Context, path string) error {
	value := f.count.Add(1)
	return os.WriteFile(path, []byte(fmt.Sprintf("fake-jpeg-%d", value)), 0o600)
}

type fakeOCR struct{}

func (fakeOCR) Extract(context.Context, string) (string, error) {
	return "Lumi end to end screen text", nil
}

type fakeAudio struct{}

func (fakeAudio) Record(ctx context.Context, path string, duration time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(duration):
	}
	return os.WriteFile(path, []byte("fake-wave"), 0o600)
}

type fakeTranscriber struct{}

func (fakeTranscriber) Transcribe(context.Context, string) (string, error) {
	return "Lumi end to end audio transcript", nil
}

func TestRecorderCaptureProcessStoreSearch(t *testing.T) {
	ctx := context.Background()
	paths, err := config.FromRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	recorder := Recorder{
		Store: s, Paths: paths, CaptureScreen: true, CaptureAudio: true,
		ScreenInterval: 8 * time.Millisecond, AudioChunk: 8 * time.Millisecond,
		Screen: &fakeScreen{}, OCR: fakeOCR{}, Audio: fakeAudio{}, Transcriber: fakeTranscriber{},
		WindowContext: func(context.Context) (string, string) { return "Test App", "Test Window" },
	}
	recordCtx, cancel := context.WithTimeout(ctx, 35*time.Millisecond)
	defer cancel()
	if err := recorder.Run(recordCtx); err != nil {
		t.Fatal(err)
	}

	screen, err := s.Search(ctx, store.SearchOptions{Query: "screen text", Kind: store.KindScreen})
	if err != nil {
		t.Fatal(err)
	}
	if len(screen) == 0 || screen[0].App != "Test App" {
		t.Fatalf("screen pipeline did not produce a searchable event: %#v", screen)
	}
	if _, err := os.Stat(screen[0].MediaPath); err != nil {
		t.Fatalf("screen media was not preserved: %v", err)
	}

	audio, err := s.Search(ctx, store.SearchOptions{Query: "audio transcript", Kind: store.KindAudio})
	if err != nil {
		t.Fatal(err)
	}
	if len(audio) == 0 || audio[0].DurationMS != 8 {
		t.Fatalf("audio pipeline did not produce a searchable event: %#v", audio)
	}
	if filepath.Ext(audio[0].MediaPath) != ".wav" {
		t.Fatalf("unexpected audio media path: %s", audio[0].MediaPath)
	}
}
