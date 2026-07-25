package cli

import (
	"strings"
	"unicode"

	"github.com/puremetricsai/lumi/internal/store"
)

// audioModalityWords name the *recording modality* rather than its content: a
// question containing one is asking what Lumi heard, not what it saw.
//
// They are also stopwords (enforced by TestAudioModalityWordsAreStopwords),
// and that pairing is the whole point. A speech transcript never contains the
// word "microphone" — but a terminal screenshot of the user typing `lumi ask
// "...on the microphone..."` does, because Lumi indexes its own terminal. So
// matching these tokens against text ranks Lumi's own UI above the recordings
// the user asked about; the signal only pays off when it selects the corpus.
var audioModalityWords = map[string]struct{}{}

// screenModalityWords do not route anything. They act only as a veto: a
// question naming both modalities ("what was on screen while I was talking")
// is relating them, and answering it from audio alone drops half the subject.
// Screen records dominate the corpus, so an unrestricted search already
// favors them — a screen router would add risk and buy nothing.
var screenModalityWords = map[string]struct{}{}

func init() {
	for _, word := range strings.Fields(`
		microphone microphones mic mics
		audio sound sounds
		conversation conversations discussion discussions
		talk talks talked talking
		said say says saying spoke spoken speak speaks speaking speech
		heard hear hears hearing overheard listen listens listened listening
		transcript transcripts transcribed transcription transcriptions
		voice voices aloud verbally
	`) {
		audioModalityWords[word] = struct{}{}
	}
	for _, word := range strings.Fields(`
		screen screens screenshot screenshots onscreen
		monitor monitors display displays
		window windows tab tabs
		read reading watched watching viewed viewing looked
		typed typing wrote writing clicked browsing browsed
	`) {
		screenModalityWords[word] = struct{}{}
	}
}

// questionModality reports the corpus a question is asking about, or "" when
// the question does not name one and retrieval should stay unrestricted.
//
// The restriction it returns is a preference, not a hard filter: a false
// positive ("what audio settings did I change" is about a settings pane, not a
// recording) is caught downstream, where an empty restricted result widens
// back to the full corpus. That is the right place for it — the corpus knows
// what exists and the wording does not.
func questionModality(question string) store.Kind {
	audio := false
	for _, field := range strings.FieldsFunc(strings.ToLower(question), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if _, screen := screenModalityWords[field]; screen {
			return ""
		}
		if _, ok := audioModalityWords[field]; ok {
			audio = true
		}
	}
	if audio {
		return store.KindAudio
	}
	return ""
}
