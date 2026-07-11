package understanding

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

type visionModel struct {
	parts int
}

type failingVisionModel struct{ visionModel }

func (modelValue *failingVisionModel) Generate(
	context.Context,
	[]*schema.Message,
	...model.Option,
) (*schema.Message, error) {
	return nil, errors.New("vision failed")
}

func (modelValue *visionModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return modelValue, nil
}

func (modelValue *visionModel) Generate(
	_ context.Context,
	messages []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	if len(messages) != 1 {
		return nil, errors.New("VLM 输入消息数量错误")
	}
	modelValue.parts = len(messages[0].UserInputMultiContent)
	return schema.AssistantMessage("人物在室内展示产品，画面稳定。", nil), nil
}

func (modelValue *visionModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := modelValue.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func TestMiniLoopExtractsFramesCallsVLMAndEmitsDegradedSummary(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	source := filepath.Join(database.Paths.Temporary, "understand.mp4")
	if _, err := media.RunCommand(t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30:duration=1", "-c:v", "libx264", "-pix_fmt", "yuv420p", source); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(source)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,mtime,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES('asset','reference',?,'video','local_path','u.mp4','hash',?,?,'{"duration_sec":1}','ready','none',1)`,
		source, info.ModTime().UnixNano(), info.Size()); err != nil {
		t.Fatal(err)
	}
	asset, err := storage.GetAsset(t.Context(), database.Read(), "asset")
	if err != nil {
		t.Fatal(err)
	}
	vision := &visionModel{}
	notes := []string{}
	summary, err := NewAnalyzer(vision).Analyze(t.Context(), database, asset, "产品", func(note string) {
		notes = append(notes, note)
	})
	if err != nil {
		t.Fatal(err)
	}
	if vision.parts != 4 || summary.Overall != "人物在室内展示产品，画面稳定。" ||
		len(summary.Segments) != 1 || len(summary.Degraded) != 1 || len(notes) < 4 {
		t.Fatalf("parts=%d summary=%#v notes=%#v", vision.parts, summary, notes)
	}
}

func TestMiniLoopHonorsCancelledContext(t *testing.T) {
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = NewAnalyzer(nil).Analyze(ctx, database, storage.Asset{ID: "missing"}, "", nil)
	if err == nil {
		t.Fatal("取消 context 应终止理解")
	}
}

func TestStaticAndAudioKindsDegradeDeterministically(t *testing.T) {
	t.Parallel()
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	for _, item := range []struct {
		id       string
		kind     string
		expected string
	}{
		{"image", "image", "still"}, {"audio", "audio", "audio"}, {"font", "font", "visual"},
	} {
		path := filepath.Join(database.Paths.Temporary, item.id+".bin")
		if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Write().ExecContext(t.Context(), `
			INSERT INTO assets(asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
				probe_json,ingest_status,understanding_status,usable)
			VALUES(?, 'reference', ?, ?, 'local_path', ?, ?, 7, '{"duration_sec":1}', 'ready', 'none', 1)`,
			item.id, path, item.kind, item.id, item.id); err != nil {
			t.Fatal(err)
		}
		asset, _ := storage.GetAsset(t.Context(), database.Read(), item.id)
		summary, err := NewAnalyzer(nil).Analyze(t.Context(), database, asset, "", nil)
		if err != nil || summary.SemanticRole != item.expected || summary.Model != "deterministic-local" || summary.Segments[0].EndSec != 1 {
			t.Fatalf("item=%s summary=%#v err=%v", item.id, summary, err)
		}
	}

	image, _ := storage.GetAsset(t.Context(), database.Read(), "image")
	if _, err := NewAnalyzer(&failingVisionModel{}).Analyze(t.Context(), database, image, "失败", nil); err == nil {
		t.Fatal("vision failure should propagate")
	}
	if _, err := extractFrames(t.Context(), database.Paths, "/missing", "image", 1); err == nil {
		t.Fatal("missing image should fail")
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := extractFrames(cancelled, database.Paths, "/missing", "video", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err=%v", err)
	}
	for _, kind := range []string{"audio", "font"} {
		frames, err := extractFrames(t.Context(), database.Paths, "/unused", kind, 1)
		if err != nil || frames != nil {
			t.Fatalf("kind=%s frames=%v err=%v", kind, frames, err)
		}
	}
	for _, value := range []any{float64(1), float32(2), 3, "bad"} {
		_ = numeric(value)
	}
	for _, kind := range []string{"video", "audio", "image", "font", "unknown"} {
		_ = kindLabel(kind)
		_ = semanticRole(kind)
	}
	if stringPointer("") != nil || stringPointer("x") == nil {
		t.Fatal("string pointer mismatch")
	}
}
