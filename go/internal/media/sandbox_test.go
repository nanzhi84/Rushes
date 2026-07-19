package media

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// restoreSandboxProbes 覆盖平台/查找探测点并在测试结束还原，使「开启」包装路径可在任意
// 平台（含 ubuntu 覆盖率 run）被测：planSandbox 只构造命令与生成 profile、不真正执行
// sandbox-exec，故与 GOOS 无关。真正跑 sandbox-exec 的测试仍单独 darwin 门控。
func restoreSandboxProbes(t *testing.T, goos string, lookPath func(string) (string, error)) {
	t.Helper()
	oldGOOS, oldLook := sandboxGOOS, sandboxLookPath
	sandboxGOOS = goos
	sandboxLookPath = lookPath
	sandboxWarnOnce = sync.Once{}
	t.Cleanup(func() {
		sandboxGOOS = oldGOOS
		sandboxLookPath = oldLook
		sandboxWarnOnce = sync.Once{}
	})
}

func newWarnBuffer(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func TestSandboxInputPaths(t *testing.T) {
	got := sandboxInputPaths("ffmpeg", []string{"-protocol_whitelist", "file,pipe", "-i", "/a/in1.mp4", "-i", "/b/in2.wav", "-frames:v", "1", "/store/out.jpg"})
	if strings.Join(got, "|") != "/a/in1.mp4|/b/in2.wav" {
		t.Fatalf("ffmpeg inputs=%v", got)
	}
	got = sandboxInputPaths("ffprobe", []string{"-protocol_whitelist", "file,pipe", "-v", "error", "-show_format", "/store/clip.mp4"})
	if len(got) != 1 || got[0] != "/store/clip.mp4" {
		t.Fatalf("ffprobe inputs=%v", got)
	}
	got = sandboxInputPaths("ffmpeg", []string{"-i", "/a/x.mp4", "-f", "null", "-"})
	if len(got) != 1 || got[0] != "/a/x.mp4" {
		t.Fatalf("null inputs=%v", got)
	}
	got = sandboxInputPaths("ffmpeg", []string{"-i", "pipe:0", "-i", "-", "out.mp4"})
	if len(got) != 0 {
		t.Fatalf("管道输入不应计入文件: %v", got)
	}
	got = sandboxInputPaths("aubiotrack", []string{"-i", "/audio/in.wav"})
	if len(got) != 1 || got[0] != "/audio/in.wav" {
		t.Fatalf("aubio inputs=%v", got)
	}
	got = sandboxInputPaths("fc-scan", []string{"--format=%{family[0]}", "/fonts/untrusted.ttf"})
	if len(got) != 1 || got[0] != "/fonts/untrusted.ttf" {
		t.Fatalf("fc-scan inputs=%v", got)
	}
}

func TestBuildSeatbeltProfileStructure(t *testing.T) {
	store := t.TempDir()
	ConfigureFFmpegSandbox([]string{store}, []string{store})
	t.Cleanup(func() { ConfigureFFmpegSandbox(nil, nil) })
	in := filepath.Join(store, "in.mp4")
	if err := os.WriteFile(in, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := buildSeatbeltProfile("ffmpeg", []string{"-i", in, "-frames:v", "1", filepath.Join(store, "out.jpg")})
	for _, must := range []string{
		"(version 1)",
		"(import \"" + seatbeltSystemProfile + "\")",
		"(allow process-exec*)",
		"(deny network*)",
		"(deny file-write*)",
		"(allow file-read*",
	} {
		if !strings.Contains(profile, must) {
			t.Fatalf("profile 缺少 %q:\n%s", must, profile)
		}
	}
	if !strings.Contains(profile, sandboxQuote(resolveSandboxPath(store))) {
		t.Fatalf("profile 未放行对象库根:\n%s", profile)
	}
	if !strings.Contains(profile, "(literal "+sandboxQuote(resolveSandboxPath(in))) {
		t.Fatalf("profile 未放行输入文件:\n%s", profile)
	}
	if !strings.Contains(profile, "/opt/homebrew") {
		t.Fatalf("profile 未放行包管理器前缀:\n%s", profile)
	}
}

func TestPlanSandboxDisabledPassthrough(t *testing.T) {
	t.Setenv("RUSHES_FFMPEG_SANDBOX", "0")
	plan := planSandbox("ffmpeg", []string{"-i", "x", "out.mp4"})
	defer plan.cleanup()
	if plan.name != "ffmpeg" {
		t.Fatalf("关闭时应原样直跑，实际 name=%s", plan.name)
	}
}

func TestPlanSandboxUnrelatedCommandPassthrough(t *testing.T) {
	t.Setenv("RUSHES_FFMPEG_SANDBOX", "1")
	plan := planSandbox("osascript", []string{"-e", "1"})
	defer plan.cleanup()
	if plan.name != "osascript" {
		t.Fatalf("非媒体解析命令不应被沙箱，实际 name=%s", plan.name)
	}
}

func TestPlanSandboxDegradeNonDarwin(t *testing.T) {
	restoreSandboxProbes(t, "linux", func(string) (string, error) { return "", nil })
	t.Setenv("RUSHES_FFMPEG_SANDBOX", "1")
	buf := newWarnBuffer(t)
	plan := planSandbox("ffmpeg", []string{"-i", "x", "out.mp4"})
	defer plan.cleanup()
	if plan.name != "ffmpeg" {
		t.Fatalf("非 darwin 应降级直跑，实际 name=%s", plan.name)
	}
	if !strings.Contains(buf.String(), "降级") || !strings.Contains(buf.String(), "沙箱") {
		t.Fatalf("降级应记一条警告日志，实际: %q", buf.String())
	}
}

func TestPlanSandboxDegradeSandboxExecMissing(t *testing.T) {
	restoreSandboxProbes(t, "darwin", func(string) (string, error) { return "", errors.New("not found") })
	t.Setenv("RUSHES_FFMPEG_SANDBOX", "1")
	buf := newWarnBuffer(t)
	plan := planSandbox("ffmpeg", []string{"-i", "x", "out.mp4"})
	defer plan.cleanup()
	if plan.name != "ffmpeg" {
		t.Fatalf("缺 sandbox-exec 应降级，实际 name=%s", plan.name)
	}
	if !strings.Contains(buf.String(), "sandbox-exec") {
		t.Fatalf("应记 sandbox-exec 缺失日志: %q", buf.String())
	}
}

func TestPlanSandboxDegradeExecutableUnresolved(t *testing.T) {
	restoreSandboxProbes(t, "darwin", func(name string) (string, error) {
		if name == "sandbox-exec" {
			return "/usr/bin/sandbox-exec", nil
		}
		return "", errors.New("not found")
	})
	t.Setenv("RUSHES_FFMPEG_SANDBOX", "1")
	store := t.TempDir()
	ConfigureFFmpegSandbox([]string{store}, []string{store})
	t.Cleanup(func() { ConfigureFFmpegSandbox(nil, nil) })
	buf := newWarnBuffer(t)
	plan := planSandbox("ffmpeg", []string{"-i", "x", "out.mp4"})
	defer plan.cleanup()
	if plan.name != "ffmpeg" {
		t.Fatalf("无法解析可执行文件应降级，实际 name=%s", plan.name)
	}
	if !strings.Contains(buf.String(), "可执行文件") {
		t.Fatalf("应记可执行文件不可解析日志: %q", buf.String())
	}
}

func TestPlanSandboxUnconfiguredPassthrough(t *testing.T) {
	restoreSandboxProbes(t, "darwin", func(name string) (string, error) {
		if name == "sandbox-exec" {
			return "/usr/bin/sandbox-exec", nil
		}
		return "/opt/testbin/" + name, nil
	})
	t.Setenv("RUSHES_FFMPEG_SANDBOX", "1")
	ConfigureFFmpegSandbox(nil, nil)
	buf := newWarnBuffer(t)
	plan := planSandbox("ffmpeg", []string{"-i", "x", "out.mp4"})
	defer plan.cleanup()
	if plan.name != "ffmpeg" {
		t.Fatalf("未注入对象库根应直跑，实际 name=%s", plan.name)
	}
	if !strings.Contains(buf.String(), "尚未配置") {
		t.Fatalf("未配置沙箱根必须告警，实际: %q", buf.String())
	}
}

func TestPlanSandboxWrapsAubioAndFontScanWithPerToolProfiles(t *testing.T) {
	restoreSandboxProbes(t, "darwin", func(name string) (string, error) {
		if name == "sandbox-exec" {
			return "/usr/bin/sandbox-exec", nil
		}
		return "/opt/testbin/" + name, nil
	})
	t.Setenv("RUSHES_FFMPEG_SANDBOX", "1")
	store := t.TempDir()
	ConfigureFFmpegSandbox([]string{store}, []string{store})
	t.Cleanup(func() { ConfigureFFmpegSandbox(nil, nil) })

	for _, test := range []struct {
		name  string
		args  []string
		input string
	}{
		{name: "aubiotrack", args: []string{"-i", filepath.Join(store, "beat.wav")}, input: filepath.Join(store, "beat.wav")},
		{name: "aubioonset", args: []string{"-i", filepath.Join(store, "voice.wav")}, input: filepath.Join(store, "voice.wav")},
		{name: "fc-scan", args: []string{"--format=%{family[0]}", filepath.Join(store, "font.ttf")}, input: filepath.Join(store, "font.ttf")},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := planSandbox(test.name, test.args)
			defer plan.cleanup()
			if plan.name != "sandbox-exec" || len(plan.args) < 3 {
				t.Fatalf("%s 未走 Seatbelt: %#v", test.name, plan)
			}
			profileData, err := os.ReadFile(plan.args[1])
			if err != nil {
				t.Fatal(err)
			}
			profile := string(profileData)
			if !strings.Contains(profile, "(literal "+sandboxQuote(resolveSandboxPath(test.input))) {
				t.Fatalf("%s profile 未放行输入:\n%s", test.name, profile)
			}
			if test.name == "fc-scan" {
				writeSection := strings.Split(profile, "(deny file-read*")[0]
				if strings.Contains(writeSection, "(subpath "+sandboxQuote(resolveSandboxPath(store))) {
					t.Fatalf("fc-scan 只读 profile 不应放行对象库写入:\n%s", profile)
				}
			}
		})
	}
}

func TestRunFFmpegLinesUsesSandboxPlan(t *testing.T) {
	bin := t.TempDir()
	capture := filepath.Join(t.TempDir(), "sandbox-args.txt")
	writeExecutable := func(name, script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable("sandbox-exec", "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$RUSHES_TEST_SANDBOX_CAPTURE\"\nshift 2\nexec \"$@\"\n")
	writeExecutable("ffmpeg", "#!/bin/sh\necho sandboxed-line\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RUSHES_TEST_SANDBOX_CAPTURE", capture)
	t.Setenv("RUSHES_FFMPEG_SANDBOX", "1")
	restoreSandboxProbes(t, "darwin", exec.LookPath)
	ConfigureFFmpegSandbox([]string{t.TempDir()}, []string{t.TempDir()})
	t.Cleanup(func() { ConfigureFFmpegSandbox(nil, nil) })
	var lines []string
	if err := RunFFmpegLines(t.Context(), "ffmpeg", []string{"-i", "pipe:0"}, func(line string) bool {
		lines = append(lines, line)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal("RunFFmpegLines 未经过 fake sandbox-exec")
	}
	if !strings.HasPrefix(string(args), "-f\n") || len(lines) != 1 || lines[0] != "sandboxed-line" {
		t.Fatalf("sandbox args=%q lines=%v", args, lines)
	}
}

func TestPlanSandboxEnabledWrapping(t *testing.T) {
	restoreSandboxProbes(t, "darwin", func(name string) (string, error) {
		if name == "sandbox-exec" {
			return "/usr/bin/sandbox-exec", nil
		}
		return "/opt/testbin/" + name, nil
	})
	t.Setenv("RUSHES_FFMPEG_SANDBOX", "1")
	store := t.TempDir()
	ConfigureFFmpegSandbox([]string{store}, []string{store})
	t.Cleanup(func() { ConfigureFFmpegSandbox(nil, nil) })
	plan := planSandbox("ffmpeg", []string{"-i", "/x/in.mp4", "/x/out.mp4"})
	defer plan.cleanup()
	if plan.name != "sandbox-exec" {
		t.Fatalf("开启时应包 sandbox-exec，实际 name=%s", plan.name)
	}
	if len(plan.args) < 4 || plan.args[0] != "-f" || !filepath.IsAbs(plan.args[2]) || !strings.HasSuffix(plan.args[2], "ffmpeg") {
		t.Fatalf("应以 ffmpeg 绝对路径交给 sandbox-exec，实际 args=%v", plan.args)
	}
	if plan.args[3] != "-i" {
		t.Fatalf("原始参数应原样跟随，实际 args=%v", plan.args)
	}
	profilePath := plan.args[1]
	data, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("profile 临时文件应存在: %v", err)
	}
	profile := string(data)
	for _, must := range []string{"(deny network*)", "(deny file-write*)", "(allow file-read*", sandboxQuote(resolveSandboxPath(store))} {
		if !strings.Contains(profile, must) {
			t.Fatalf("profile 缺少 %q:\n%s", must, profile)
		}
	}
	plan.cleanup()
	if _, err := os.Stat(profilePath); !os.IsNotExist(err) {
		t.Fatalf("cleanup 应删除 profile 临时文件")
	}
}

func TestResolveSandboxExecutable(t *testing.T) {
	restoreSandboxProbes(t, runtime.GOOS, func(name string) (string, error) { return "/opt/testbin/" + name, nil })
	got, ok := resolveSandboxExecutable("ffmpeg")
	if !ok || !filepath.IsAbs(got) || !strings.HasSuffix(got, "ffmpeg") {
		t.Fatalf("不存在路径应经 Abs 分支返回绝对路径，实际=%q ok=%v", got, ok)
	}
	restoreSandboxProbes(t, runtime.GOOS, func(string) (string, error) { return "/bin/sh", nil })
	if got, ok := resolveSandboxExecutable("sh"); !ok || got == "" {
		t.Fatalf("真实存在路径应经 EvalSymlinks 分支返回，实际=%q ok=%v", got, ok)
	}
	restoreSandboxProbes(t, runtime.GOOS, func(string) (string, error) { return "", errors.New("nope") })
	if _, ok := resolveSandboxExecutable("ffmpeg"); ok {
		t.Fatal("LookPath 失败应返回 !ok")
	}
}

func TestResolveSandboxPath(t *testing.T) {
	if resolveSandboxPath("") != "" {
		t.Fatal("空路径应返回空")
	}
	if got := resolveSandboxPath(t.TempDir()); got == "" {
		t.Fatal("已存在路径解析为空")
	}
	if got := resolveSandboxPath("/no/such/x2/path"); got == "" || !filepath.IsAbs(got) {
		t.Fatalf("不存在路径应退回 Abs，实际=%q", got)
	}
}

func TestSeatbeltDeniesOutsideStoreWrite(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt 仅 macOS")
	}
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("缺 sandbox-exec")
	}
	store, err := os.MkdirTemp("/tmp", "x2-store-")
	if err != nil {
		t.Fatal(err)
	}
	outside, err := os.MkdirTemp("/tmp", "x2-outside-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(store); _ = os.RemoveAll(outside) })
	ConfigureFFmpegSandbox([]string{store}, []string{store})
	t.Cleanup(func() { ConfigureFFmpegSandbox(nil, nil) })

	profile := buildSeatbeltProfile("ffmpeg", []string{"-i", filepath.Join(store, "in.mp4"), filepath.Join(store, "out.mp4")})
	profilePath := filepath.Join(t.TempDir(), "p.sb")
	if err := os.WriteFile(profilePath, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(target string) error {
		return exec.Command("sandbox-exec", "-f", profilePath, "/bin/sh", "-c", "echo ok > "+target).Run()
	}
	if err := run(filepath.Join(store, "inside.txt")); err != nil {
		t.Fatalf("对象库内写入应被允许，实际被拒: %v", err)
	}
	if err := run(filepath.Join(outside, "evil.txt")); err == nil {
		t.Fatal("对象库以外写入应被 Seatbelt 拒绝，实际成功")
	}
}

func TestSeatbeltAllowsRealFFmpegChain(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt 仅 macOS")
	}
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("缺 sandbox-exec")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("缺 ffmpeg")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("缺 ffprobe")
	}
	store, err := os.MkdirTemp("/tmp", "x2-real-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(store) })
	input := filepath.Join(store, "in.mp4")
	gen := exec.Command(ffmpeg, "-y", "-f", "lavfi", "-i", "testsrc=size=64x64:duration=1:rate=5", "-pix_fmt", "yuv420p", input)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("生成测试输入失败: %v\n%s", err, out)
	}
	ConfigureFFmpegSandbox([]string{store}, []string{store})
	t.Cleanup(func() { ConfigureFFmpegSandbox(nil, nil) })
	t.Setenv("RUSHES_FFMPEG_SANDBOX", "1")

	probe, err := ProbeFile(context.Background(), input)
	if err != nil {
		t.Fatalf("沙箱开启下真机 ffprobe 应成功，实际: %v", err)
	}
	if probe.DurationSec <= 0 {
		t.Fatalf("probe 时长异常: %+v", probe)
	}
	out := filepath.Join(store, "thumb.jpg")
	if err := RunFFmpegProgress(context.Background(), "ffmpeg", []string{"-y", "-i", input, "-frames:v", "1", out}, nil); err != nil {
		t.Fatalf("沙箱开启下真机缩略图应成功，实际: %v", err)
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		t.Fatalf("缩略图未生成: %v", err)
	}
}

func TestSeatbeltAllowsRealPeaksBeatsAndFontScan(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt 仅 macOS")
	}
	for _, binary := range []string{"sandbox-exec", "ffmpeg", "aubiotrack", "aubioonset", "fc-scan"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("缺 %s", binary)
		}
	}
	store, err := os.MkdirTemp("/tmp", "x3-real-media-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(store) })
	audio := filepath.Join(store, "clicks.wav")
	clickSource := `aevalsrc=if(lt(mod(t\,0.5)\,0.05)\,sin(2*PI*1000*t)\,0):s=44100:d=4`
	generate := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", clickSource, audio)
	if output, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("生成节拍测试音频失败: %v\n%s", err, output)
	}
	fontSource := "/System/Library/Fonts/SFNS.ttf"
	fontData, err := os.ReadFile(fontSource)
	if err != nil {
		t.Skipf("缺系统测试字体: %v", err)
	}
	font := filepath.Join(store, "untrusted.ttf")
	if err := os.WriteFile(font, fontData, 0o600); err != nil {
		t.Fatal(err)
	}

	ConfigureFFmpegSandbox([]string{store}, []string{store})
	t.Cleanup(func() { ConfigureFFmpegSandbox(nil, nil) })
	t.Setenv("RUSHES_FFMPEG_SANDBOX", "1")

	peaks, err := AnalyzeWaveformPeaks(t.Context(), audio, 4)
	if err != nil || len(peaks.Peaks) == 0 {
		t.Fatalf("沙箱开启下 peaks 全链路失败: peaks=%d err=%v", len(peaks.Peaks), err)
	}
	beats, err := AnalyzeBeatGrid(t.Context(), audio, 30, 64)
	if err != nil || len(beats.BeatFrames) < 2 {
		t.Fatalf("沙箱开启下 aubio 全链路失败: beats=%v err=%v", beats.BeatFrames, err)
	}
	if family := subtitleFontFamily(t.Context(), font); family == "" || family == "untrusted" {
		t.Fatalf("沙箱开启下 fc-scan 未读取字体 family: %q", family)
	}
}
