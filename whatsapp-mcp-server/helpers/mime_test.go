package helpers

import "testing"

func TestMimeForFilenameVoiceNotes(t *testing.T) {
	for _, name := range []string{"voice.ogg", "VOICE.OGG", "reply.opus"} {
		if got := mimeForFilename(name); got != "audio/ogg; codecs=opus" {
			t.Errorf("mimeForFilename(%q) = %q, want audio/ogg; codecs=opus", name, got)
		}
	}
}
