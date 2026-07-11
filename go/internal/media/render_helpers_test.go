package media

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

func TestRenderFilterHelpersCoverTempoSoloAndFormatting(t *testing.T) {
	for _, item := range []struct {
		rate float64
		want []string
	}{
		{1, nil},
		{1.5, []string{"atempo=1.500000"}},
		{8, []string{"atempo=2", "atempo=2", "atempo=2.000000"}},
		{0.125, []string{"atempo=0.5", "atempo=0.5", "atempo=0.500000"}},
	} {
		got := atempoFilters(item.rate)
		if strings.Join(got, ",") != strings.Join(item.want, ",") {
			t.Fatalf("atempoFilters(%v)=%#v want=%#v", item.rate, got, item.want)
		}
	}
	if effectivePlaybackRate(0) != 1 || effectivePlaybackRate(2) != 2 {
		t.Fatal("effective playback rate mismatch")
	}
	if formatSRTTime(-1) != "00:00:00,000" || formatSRTTime(3661.234) != "01:01:01,234" {
		t.Fatal("SRT time formatting mismatch")
	}
	if escaped := escapeFilterPath(`C:\media\it's.mp4`); !strings.Contains(escaped, `C\:`) || !strings.Contains(escaped, `it\'s`) {
		t.Fatalf("escaped path=%q", escaped)
	}

	document := timeline.Empty("render_tracks", 1)
	document.Tracks[2].Solo = true
	document.Tracks[3].Muted = true
	audioTracks := renderableTracks(document, "audio", true)
	if len(audioTracks) != 1 || audioTracks[0].TrackID != "original_audio" {
		t.Fatalf("solo audio tracks=%#v", audioTracks)
	}
	visualTracks := renderableTracks(document, "visual", false)
	if len(visualTracks) != 2 {
		t.Fatalf("visual tracks=%#v", visualTracks)
	}
	for _, item := range []struct {
		track timeline.Track
		want  string
	}{
		{timeline.Track{TrackID: "visual_overlay"}, "visual"},
		{timeline.Track{TrackID: "sfx"}, "audio"},
		{timeline.Track{TrackID: "subtitles"}, "text"},
		{timeline.Track{TrackID: "custom", TrackType: "effects"}, "effects"},
	} {
		if got := trackFamilyForRender(item.track); got != item.want {
			t.Fatalf("trackFamilyForRender(%#v)=%q", item.track, got)
		}
	}
}

func TestAppendAudioMixUsesPrimaryAudioFallback(t *testing.T) {
	document := timeline.Empty("render_audio_fallback", 1)
	document.DurationFrames = 60
	inputs := []preparedPrimaryInput{
		{clip: timeline.Clip{TimelineStartFrame: 0, TimelineEndFrame: 15, SourceEndFrame: 15}, inputIndex: 0, kind: "image"},
		{clip: timeline.Clip{TimelineStartFrame: 0, TimelineEndFrame: 15, SourceEndFrame: 15}, inputIndex: 1, kind: "video", probe: Probe{}},
		{clip: timeline.Clip{TimelineStartFrame: 0, TimelineEndFrame: 30, SourceEndFrame: 30, PlaybackRate: 4}, inputIndex: 2, kind: "video", probe: Probe{HasAudio: true}},
		{clip: timeline.Clip{TimelineStartFrame: 30, TimelineEndFrame: 60, SourceEndFrame: 30, PlaybackRate: 0}, inputIndex: 3, kind: "video", probe: Probe{HasAudio: true}},
	}
	label, args, filters, inputIndex, err := appendAudioMix(
		context.Background(), nil, document, inputs, []string{"-y"}, nil, 4,
	)
	if err != nil {
		t.Fatal(err)
	}
	if label != "mixed_audio" || len(args) != 1 || inputIndex != 4 || len(filters) != 3 || !strings.Contains(filters[2], "amix=inputs=2") {
		t.Fatalf("label=%q args=%#v filters=%#v inputIndex=%d", label, args, filters, inputIndex)
	}
}

func TestAppendSubtitlesSortsSkipsBlankAndPropagatesCreateError(t *testing.T) {
	document := timeline.Empty("render_subtitles", 1)
	document.DurationFrames = 90
	document.Tracks[5].Clips = []timeline.Clip{
		{TimelineClipID: "later", TrackID: "subtitles", Text: "later\r\nline", TimelineStartFrame: 30, TimelineEndFrame: 60},
		{TimelineClipID: "blank", TrackID: "subtitles", Text: " \r ", TimelineStartFrame: 20, TimelineEndFrame: 25},
		{TimelineClipID: "first", TrackID: "subtitles", Text: "first", TimelineStartFrame: 0, TimelineEndFrame: 15},
	}

	paths, err := storage.NewPaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Initialize(); err != nil {
		t.Fatal(err)
	}
	database := &storage.DB{Paths: paths}
	filters := []string{}
	label, path, err := appendSubtitles(database, document, "base", &filters)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if label != "subtitled_video" || len(filters) != 1 || strings.Contains(text, "blank") || strings.Index(text, "first") > strings.Index(text, "later") {
		t.Fatalf("label=%q filters=%#v srt=%q", label, filters, text)
	}

	badDatabase := &storage.DB{Paths: storage.Paths{Temporary: paths.Temporary + "/missing"}}
	if _, _, err := appendSubtitles(badDatabase, document, "base", &filters); err == nil {
		t.Fatal("missing temporary directory should fail subtitle creation")
	}
}
