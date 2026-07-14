package media

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestParseSRTKeepsExactIntegerFrameEvidence(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	video := filepath.Join(directory, "口播.mp4")
	if err := os.WriteFile(video, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	subtitle := filepath.Join(directory, "口播.srt")
	if err := os.WriteFile(subtitle, []byte(
		"1\n00:00:00,233 --> 00:00:01,100\n第一句\n\n"+
			"2\n00:00:01.500 --> 00:00:03.000\n第二句\n仍是第二句\n\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	if found := FindSidecarSRT(video); found != subtitle {
		t.Fatalf("sidecar=%q", found)
	}
	cues, err := ParseSRT(subtitle, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(cues) != 2 || cues[0].StartFrame != 7 || cues[0].EndFrame != 33 ||
		cues[1].StartFrame != 45 || cues[1].EndFrame != 90 ||
		cues[1].Text != "第二句 仍是第二句" {
		t.Fatalf("cues=%#v", cues)
	}
}

func TestParseSRTRejectsInvalidOrEmptyEvidence(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "bad.srt")
	if err := os.WriteFile(path, []byte("1\n00:00:02,000 --> 00:00:01,000\n反向\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSRT(path, 30); err == nil {
		t.Fatal("反向时间范围应失败")
	}
	if _, err := ParseSRT(path, 0); err == nil {
		t.Fatal("无效 fps 应失败")
	}
}

func TestExtractAudioSegmentMP3RejectsInvalidRangesAndTemporaryDirectory(t *testing.T) {
	t.Parallel()
	if _, err := ExtractAudioSegmentMP3(t.Context(), t.TempDir(), "missing.wav", 10, 10, 30); err == nil {
		t.Fatal("空范围应失败")
	}
	if _, err := ExtractAudioSegmentMP3(
		context.Background(), filepath.Join(t.TempDir(), "missing-directory"), "missing.wav", 0, 30, 30,
	); err == nil {
		t.Fatal("不存在的临时目录应失败")
	}
	if _, err := ExtractAudioSegmentMP3(
		t.Context(), t.TempDir(), filepath.Join(t.TempDir(), "missing-source.wav"), 0, 30, 30,
	); err == nil {
		t.Fatal("不存在的音频源应失败")
	}
}
