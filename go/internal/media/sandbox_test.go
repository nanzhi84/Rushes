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

func TestPlanSandboxNonFFmpegPassthrough(t *testing.T) {
	t.Setenv("RUSHES_FFMPEG_SANDBOX", "1")
	plan := planSandbox("aubiotrack", []string{"-i", "x"})
	defer plan.cleanup()
	if plan.name != "aubiotrack" {
		t.Fatalf("非 ffmpeg 命令不应被沙箱，实际 name=%s", plan.name)
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
	plan := planSandbox("ffmpeg", []string{"-i", "x", "out.mp4"})
	defer plan.cleanup()
	if plan.name != "ffmpeg" {
		t.Fatalf("未注入对象库根应直跑，实际 name=%s", plan.name)
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
