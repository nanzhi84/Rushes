package media

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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
	if label != "mixed_audio" || len(args) != 1 || inputIndex != 4 || len(filters) != 4 || !strings.Contains(filters[2], "amix=inputs=2") {
		t.Fatalf("label=%q args=%#v filters=%#v inputIndex=%d", label, args, filters, inputIndex)
	}
	faded := audioFilter(0, timeline.Clip{
		TimelineStartFrame: 15, TimelineEndFrame: 75,
		SourceStartFrame: 0, SourceEndFrame: 60, PlaybackRate: 1,
		FadeInFrames: 6, FadeOutFrames: 12,
	}, -3, document, "faded", audioSeam{})
	for _, expected := range []string{
		"afade=t=in:st=0:d=0.200000",
		"afade=t=out:st=1.600000:d=0.400000",
		"adelay=500:all=1",
	} {
		if !strings.Contains(faded, expected) {
			t.Fatalf("audio filter missing %q: %s", expected, faded)
		}
	}
}

func TestLinkedVideoFadesReachFinalVideoAndOriginalAudioFilters(t *testing.T) {
	document, err := timeline.ComposeInitial("linked_fade_render", 1, []timeline.Selection{{
		AssetID: "talk", AssetKind: "video", SourceEndFrame: 60, HasAudio: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	document, err = timeline.ApplyPatch(document, map[string]any{
		"kind": "set_clip_fades", "timeline_clip_id": document.Tracks[0].Clips[0].TimelineClipID,
		"fade_in_frames": 6, "fade_out_frames": 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	video := primaryVideoFilter(0, document.Tracks[0].Clips[0], document, RenderProfile{Width: 160, Height: 90}, 0, 2, 2)
	audio := audioFilter(0, document.Tracks[2].Clips[0], 0, document, "audio", audioSeam{})
	for _, expected := range []string{"fade=t=in:st=0:d=0.200000", "fade=t=out:st=1.600000:d=0.400000"} {
		if !strings.Contains(video, expected) {
			t.Fatalf("video filter missing %q: %s", expected, video)
		}
	}
	for _, expected := range []string{"afade=t=in:st=0:d=0.200000", "afade=t=out:st=1.600000:d=0.400000"} {
		if !strings.Contains(audio, expected) {
			t.Fatalf("audio filter missing %q: %s", expected, audio)
		}
	}
}

func TestBuildDuckingFilterGraphGroupsVoiceKeyAndKeepsFinalMix(t *testing.T) {
	tracks := []audioTrackLabel{
		{TrackID: "original_audio", Label: "original", DuckingKeyLabels: []string{"original_clip_duck_key"}},
		{TrackID: "voiceover", Label: "voice", DuckingKeyLabels: []string{"voice_clip_duck_key"}},
		{TrackID: "bgm", Label: "music"},
		{TrackID: "sfx", Label: "effects"},
	}
	labels, filters := buildDuckingFilterGraph(tracks, &timeline.TrackDucking{
		Enabled: true, DuckDB: -9, TriggerTracks: []string{"voiceover", "original_audio"},
	}, 4)
	joined := strings.Join(filters, ";")
	for _, expected := range []string{
		"[original_clip_duck_key]agate=threshold=0.003:ratio=9000:range=0:attack=15:release=250:knee=1:detection=rms,apad=whole_dur=4.000000,atrim=duration=4.000000[original_clip_duck_key_gated]",
		"[voice_clip_duck_key]agate=threshold=0.003:ratio=9000:range=0:attack=15:release=250:knee=1:detection=rms,apad=whole_dur=4.000000,atrim=duration=4.000000[voice_clip_duck_key_gated]",
		"[voice_clip_duck_key_gated][original_clip_duck_key_gated]amix=inputs=2",
		"[ducking_key]compand=attacks=0.015:decays=0.250:points=-90/-90|-50/0|0/0[ducking_key_normalized]",
		"[music][ducking_key_normalized]sidechaincompress=threshold=0.05:ratio=1.529412",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("ducking graph missing %q: %s", expected, joined)
		}
	}
	if !reflect.DeepEqual(labels, []string{"original", "voice", "bgm_ducked", "effects"}) {
		t.Fatalf("labels=%#v", labels)
	}
	unchanged, filters := buildDuckingFilterGraph(tracks, &timeline.TrackDucking{
		Enabled: false, DuckDB: -9, TriggerTracks: []string{"voiceover"},
	}, 4)
	if len(filters) != 0 || !reflect.DeepEqual(unchanged, []string{"original", "voice", "music", "effects"}) {
		t.Fatalf("disabled labels=%#v filters=%#v", unchanged, filters)
	}
}

func TestLinkOrCopyFileFallsBackWhenLinksAreUnavailable(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.ttf")
	destination := filepath.Join(t.TempDir(), "staged.ttf")
	want := []byte("portable font fixture")
	if err := os.WriteFile(source, want, 0o600); err != nil {
		t.Fatal(err)
	}
	linkFailure := func(string, string) error { return os.ErrPermission }
	if err := linkOrCopyFileWith(source, destination, linkFailure, linkFailure); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("staged font=%q want=%q", got, want)
	}
}

func TestSubtitleASSHeaderSanitizesFontFamily(t *testing.T) {
	header := subtitleASSHeader(PreviewProfile, "  My,\nFont\t ")
	if strings.Contains(header, "My,") || strings.Contains(header, "\nFont") ||
		!strings.Contains(header, "Style: default,My Font,") {
		t.Fatalf("unsafe font family in ASS header: %q", header)
	}
	if got := sanitizeASSFontName("\n,\t"); got != "Arial" {
		t.Fatalf("blank sanitized font=%q", got)
	}
}

func TestVideoFadeFiltersAndProfileOrientation(t *testing.T) {
	clip := timeline.Clip{FadeInFrames: 6, FadeOutFrames: 12}
	if got := videoFadeFilters(clip, 30, 2, false); !reflect.DeepEqual(got, []string{
		"fade=t=in:st=0:d=0.200000", "fade=t=out:st=1.600000:d=0.400000",
	}) {
		t.Fatalf("both fades=%#v", got)
	}
	if got := videoFadeFilters(timeline.Clip{FadeInFrames: 6}, 30, 2, false); len(got) != 1 || !strings.Contains(got[0], "t=in") {
		t.Fatalf("fade in=%#v", got)
	}
	if got := videoFadeFilters(timeline.Clip{FadeOutFrames: 6}, 30, 2, false); len(got) != 1 || !strings.Contains(got[0], "t=out") {
		t.Fatalf("fade out=%#v", got)
	}
	if got := videoFadeFilters(timeline.Clip{}, 30, 2, false); len(got) != 0 {
		t.Fatalf("no fades=%#v", got)
	}
	if got := videoFadeFilters(clip, 30, 2, true); !reflect.DeepEqual(got, []string{
		"fade=t=in:st=0:d=0.200000:alpha=1", "fade=t=out:st=1.600000:d=0.400000:alpha=1",
	}) {
		t.Fatalf("alpha fades=%#v", got)
	}
	portrait, err := ProfileForOrientation(FinalProfile, "portrait")
	if err != nil || portrait.Width != 1080 || portrait.Height != 1920 || portrait.AutoOrient {
		t.Fatalf("portrait=%#v err=%v", portrait, err)
	}
	landscape, err := ProfileForOrientation(FinalProfile, "landscape")
	if err != nil || landscape.Width != 1920 || landscape.Height != 1080 || landscape.AutoOrient {
		t.Fatalf("landscape=%#v err=%v", landscape, err)
	}
	automatic, err := ProfileForOrientation(FinalProfile, "auto")
	if err != nil || automatic != FinalProfile {
		t.Fatalf("auto=%#v err=%v", automatic, err)
	}
	if _, err := ProfileForOrientation(FinalProfile, "square"); err == nil {
		t.Fatal("unknown orientation should fail")
	}
}

func TestFinalizeInspectionSummaryUsesMergedIssuesAndDegradedState(t *testing.T) {
	inspection := Inspection{Issues: []InspectionIssue{{Check: "visual_crop", Severity: "warning"}}}
	FinalizeInspectionSummary(&inspection)
	if inspection.Summary != "成片检查完成：发现 1 项提示。" {
		t.Fatalf("visual summary=%q", inspection.Summary)
	}
	inspection.Degraded = true
	inspection.Issues = append(inspection.Issues, InspectionIssue{Check: "dependencies", Severity: "warning"})
	FinalizeInspectionSummary(&inspection)
	if inspection.Summary != "成片检查降级完成：发现 2 项提示。" {
		t.Fatalf("degraded summary=%q", inspection.Summary)
	}
}

func TestSubtitlePresetsScaleWithOutputAndASSIncludesEveryStyle(t *testing.T) {
	for _, style := range timeline.SubtitleStyleNames {
		preview := subtitlePreset(style, PreviewProfile)
		final := subtitlePreset(style, FinalProfile)
		if abs(float64(final.FontSize)/float64(FinalProfile.Height)-float64(preview.FontSize)/float64(PreviewProfile.Height)) > 0.001 ||
			final.Alignment == 0 || preview.Alignment != final.Alignment {
			t.Fatalf("style=%s preview=%#v final=%#v", style, preview, final)
		}
	}
	header := subtitleASSHeader(FinalProfile, "ImportedFont")
	for _, expected := range []string{"PlayResX: 1080", "PlayResY: 1920", "ImportedFont", "Style: default", "Style: large_center", "Style: top_bar", "Style: minimal", "Style: bold_bottom"} {
		if !strings.Contains(header, expected) {
			t.Fatalf("ASS header missing %q: %s", expected, header)
		}
	}
	if formatASSTime(61.239) != "0:01:01.24" || !strings.Contains(escapeASSText("a{b}\nc"), `\{b\}\Nc`) {
		t.Fatal("ASS formatting mismatch")
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

	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	fontPath := filepath.Join(t.TempDir(), "RushesImported.ttf")
	if err := os.WriteFile(fontPath, []byte("font fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO drafts(draft_id,name,created_at,updated_at) VALUES('render_subtitles','subtitles','2026-01-01','2026-01-01');
		INSERT INTO assets(asset_id,storage_mode,reference_path,kind,source,filename,hash,size,ingest_status,usable)
		VALUES('font_fixture','reference',?,'font','local_path','RushesImported.ttf','font',12,'ready',1);
		INSERT INTO draft_asset_links(draft_id,asset_id,linked_at) VALUES('render_subtitles','font_fixture','2026-01-01')
	`, fontPath); err != nil {
		t.Fatal(err)
	}
	filters := []string{}
	label, path, fontDirectory, err := appendSubtitles(t.Context(), database, document, PreviewProfile, "base", &filters)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	t.Cleanup(func() { _ = os.RemoveAll(fontDirectory) })
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if label != "subtitled_video" || len(filters) != 1 || !strings.Contains(filters[0], "fontsdir=") ||
		!strings.Contains(text, "RushesImported") || strings.Contains(text, "blank") || strings.Index(text, "first") > strings.Index(text, "later") {
		t.Fatalf("label=%q filters=%#v srt=%q", label, filters, text)
	}

	badDatabase := &storage.DB{Paths: storage.Paths{Temporary: database.Paths.Temporary + "/missing"}}
	if _, _, _, err := appendSubtitles(t.Context(), badDatabase, document, PreviewProfile, "base", &filters); err == nil {
		t.Fatal("missing temporary directory should fail subtitle creation")
	}
}
