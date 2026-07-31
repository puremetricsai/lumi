package capture

import (
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/puremetricsai/lumi/internal/store"
)

// EmitterObservation is one instant's answer to "what was producing sound".
type EmitterObservation struct {
	At         time.Time
	Processes  []AudioProcess
	Windows    []AudioMarkerWindow
	ProcessErr string
	WindowErr  string
}

// The two reads succeed and fail independently, and must be counted that way.
// A shared "did this observation work" flag lets one source's success mask the
// other's failure: with CoreAudio failing every sample and the window scan
// merely finding nothing, the chunk would be recorded as *sampled with nothing
// emitting* when no process list was ever read. That is precisely the
// absent-versus-empty conflation the source list exists to keep apart, and it is
// unrecoverable once stored.
func (o EmitterObservation) processOK() bool { return o.ProcessErr == "" }
func (o EmitterObservation) windowOK() bool  { return o.WindowErr == "" }

// ForegroundObservation is one instant's answer to "what was the user working
// in". It is a different question from EmitterObservation with a different
// answer, and the whole point of this change is that they stopped being conflated.
type ForegroundObservation struct {
	At      time.Time
	Context ScreenContext
	Err     error
}

// emitterTimeline holds recent observations so a finished chunk can ask what was
// emitting *while it recorded*, rather than at the instant it closed.
//
// A 30-second chunk can span several application switches, and the single
// close-of-chunk sample it replaces attributed the whole chunk to whatever state
// the machine happened to be in at the end. Two fixed-capacity rings rather than
// unbounded slices because this runs for the lifetime of the recording.
type emitterTimeline struct {
	mu         sync.Mutex
	emitters   []EmitterObservation
	foreground []ForegroundObservation
}

func newEmitterTimeline(emitterCapacity, foregroundCapacity int) *emitterTimeline {
	return &emitterTimeline{
		emitters:   make([]EmitterObservation, 0, emitterCapacity),
		foreground: make([]ForegroundObservation, 0, foregroundCapacity),
	}
}

// The observe/window methods tolerate a nil timeline so a Recorder driven
// directly — as the tests drive captureScreen — behaves as one that has observed
// nothing, which the chunk path already has a fallback for. Run always builds
// one before starting a goroutine that writes to it.
func (t *emitterTimeline) observeEmitters(observation EmitterObservation) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.emitters = appendRing(t.emitters, observation)
}

func (t *emitterTimeline) observeForeground(observation ForegroundObservation) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.foreground = appendRing(t.foreground, observation)
}

// appendRing keeps the newest cap(ring) entries, oldest first.
func appendRing[T any](ring []T, value T) []T {
	if len(ring) < cap(ring) {
		return append(ring, value)
	}
	copy(ring, ring[1:])
	ring[len(ring)-1] = value
	return ring
}

// window returns everything observed in [start, end], oldest first.
func (t *emitterTimeline) window(start, end time.Time) ([]EmitterObservation, []ForegroundObservation) {
	if t == nil {
		return nil, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	emitters := make([]EmitterObservation, 0, len(t.emitters))
	for _, observation := range t.emitters {
		if !observation.At.Before(start) && !observation.At.After(end) {
			emitters = append(emitters, observation)
		}
	}
	foreground := make([]ForegroundObservation, 0, len(t.foreground))
	for _, observation := range t.foreground {
		if !observation.At.Before(start) && !observation.At.After(end) {
			foreground = append(foreground, observation)
		}
	}
	return emitters, foreground
}

// foldEmitters reduces a chunk's observations to the two source lists the
// attribution decision reads, plus the counts that say how much of the chunk each
// application was present for.
//
// The result is the *union* across the window rather than the single most
// prominent application. The system track is a tap on the whole output graph, so
// naming only the dominant emitter would claim the others were absent from a file
// that demonstrably contains them. Ordering carries the prominence instead.
func foldEmitters(observations []EmitterObservation, start time.Time) AttributionInput {
	input := AttributionInput{Attempts: len(observations)}
	processes := newSourceFold()
	markers := newSourceFold()
	var lastProcessErr, lastWindowErr string
	// Counted per source, never shared. Each SourceApp's Observations is the
	// denominator for its own evidence kind, so "present in 3 of 12 process
	// reads" cannot be diluted by window scans that read fine.
	var processObservations, windowObservations int
	for _, observation := range observations {
		offset := observation.At.Sub(start).Milliseconds()
		if observation.processOK() {
			processObservations++
			for _, process := range observation.Processes {
				processes.add(store.SourceApp{
					PID: process.PID, BundleID: process.BundleID, Name: process.Name,
					Evidence: store.EvidenceProcess,
				}, offset)
			}
		} else {
			lastProcessErr = observation.ProcessErr
		}
		if observation.windowOK() {
			windowObservations++
			for _, window := range observation.Windows {
				markers.add(store.SourceApp{
					PID: window.PID, BundleID: window.BundleID, Name: window.Name,
					Window: window.Window, Evidence: store.EvidenceWindowMarker,
				}, offset)
			}
		} else {
			lastWindowErr = observation.WindowErr
		}
	}
	input.Processes = processes.result(processObservations)
	input.Markers = markers.result(windowObservations)
	// An error is reported when *that source* never read successfully, whatever
	// the other one did. A partial failure of one source still produced evidence
	// and is not reported, since discarding it would lose a real emitter to a
	// transient.
	if processObservations == 0 && lastProcessErr != "" {
		input.ProcessErr = lastProcessErr
	}
	if windowObservations == 0 && lastWindowErr != "" {
		input.MarkerErr = lastWindowErr
	}
	// Observations is what decides "sampled and nothing was emitting" against
	// "could not sample", and only a sample that read *every* source can support
	// the first claim: with the process list unread, "nothing held an output
	// stream" is not a finding, it is a guess.
	input.Observations = processObservations
	if windowObservations < input.Observations {
		input.Observations = windowObservations
	}
	return input
}

// sourceFold accumulates one evidence kind, keyed so that one application seen
// across many samples stays one entry.
type sourceFold struct {
	order []string
	byKey map[string]*store.SourceApp
}

func newSourceFold() *sourceFold {
	return &sourceFold{byKey: make(map[string]*store.SourceApp)}
}

// identity prefers the bundle id, because a process that restarts mid-chunk
// keeps its bundle and not its pid.
func identity(app store.SourceApp) string {
	if app.BundleID != "" {
		return "bundle:" + app.BundleID
	}
	return "pid:" + strconv.Itoa(int(app.PID))
}

func (f *sourceFold) add(app store.SourceApp, offsetMS int64) {
	key := identity(app)
	existing, seen := f.byKey[key]
	if !seen {
		app.Samples = 1
		app.FirstOffsetMS, app.LastOffsetMS = offsetMS, offsetMS
		f.byKey[key] = &app
		f.order = append(f.order, key)
		return
	}
	existing.Samples++
	if offsetMS < existing.FirstOffsetMS {
		existing.FirstOffsetMS = offsetMS
	}
	if offsetMS > existing.LastOffsetMS {
		existing.LastOffsetMS = offsetMS
	}
	// A later sample may resolve a name or title an earlier one could not.
	if existing.Name == "" {
		existing.Name = app.Name
	}
	if existing.Window == "" {
		existing.Window = app.Window
	}
}

// result orders by how much of the chunk each application was present for, then
// by when it first appeared, then by key, so the ordering is total and stable
// rather than dependent on map iteration.
func (f *sourceFold) result(observations int) []store.SourceApp {
	if len(f.order) == 0 {
		return nil
	}
	apps := make([]store.SourceApp, 0, len(f.order))
	for _, key := range f.order {
		app := *f.byKey[key]
		app.Observations = observations
		apps = append(apps, app)
	}
	sort.SliceStable(apps, func(i, j int) bool {
		if apps[i].Samples != apps[j].Samples {
			return apps[i].Samples > apps[j].Samples
		}
		if apps[i].FirstOffsetMS != apps[j].FirstOffsetMS {
			return apps[i].FirstOffsetMS < apps[j].FirstOffsetMS
		}
		return identity(apps[i]) < identity(apps[j])
	})
	return apps
}

// lastForeground picks the application to record as focused for a chunk.
//
// It is the last observation in the window rather than the duration-weighted
// dominant one, which keeps events.app meaning exactly what it has always meant —
// "sampled once as the chunk closed" — so this change moves the *source* fields
// without quietly redefining the focus field beside them. What the chunk saw
// besides that is recorded in metadata instead.
func lastForeground(observations []ForegroundObservation) (ForegroundObservation, bool) {
	for i := len(observations) - 1; i >= 0; i-- {
		if observations[i].Err == nil {
			return observations[i], true
		}
	}
	if len(observations) > 0 {
		return observations[len(observations)-1], true
	}
	return ForegroundObservation{}, false
}

// distinctForegroundApps counts how many different applications held focus
// across the chunk. It is what makes "the focus field is a single sample of a
// thirty-second window" visible in the data rather than only in a doc comment.
func distinctForegroundApps(observations []ForegroundObservation) int {
	seen := make(map[string]bool, len(observations))
	for _, observation := range observations {
		if observation.Err != nil {
			continue
		}
		seen[observation.Context.App] = true
	}
	return len(seen)
}
