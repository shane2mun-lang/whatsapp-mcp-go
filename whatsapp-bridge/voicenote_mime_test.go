package main

import "testing"

func TestNormalizeVoiceNoteMimetype(t *testing.T) {
	cases := map[string]string{
		"":                       voiceNoteMimetype,
		"audio/ogg":              voiceNoteMimetype,
		"Audio/OGG":              voiceNoteMimetype,
		"audio/opus":             voiceNoteMimetype,
		"application/ogg":        voiceNoteMimetype,
		"audio/ogg; codecs=opus": voiceNoteMimetype,
		"audio/mpeg":             "audio/mpeg",
	}
	for in, want := range cases {
		if got := normalizeVoiceNoteMimetype(in); got != want {
			t.Errorf("normalizeVoiceNoteMimetype(%q) = %q, want %q", in, got, want)
		}
	}
}
