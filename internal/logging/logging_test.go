// Package logging — rule 71 规范层的单元测试
//
// -----------------------------------------------------------------------------
// Purpose:        证明 memora 的规范层确实满足 rules/71-log-standard.md 的关键约束：
//                 必填字段齐全、扁平单层、级别数值符合 OTel 表、敏感信息脱敏、
//                 保留名唯一、规范层早装。
// Replaces:       (无)
// Replaced-by:    (无)
// Status:         active
//
// Changelog:
//   2026-11-09  v1.0  首版。随 rule 71 落地。
// -----------------------------------------------------------------------------

package logging

import (
	"encoding/json"
	stdlog "log"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// captureStdout 捕获 os.Stdout 的写入，返回取内容的函数。
func captureStdout(t *testing.T) func() string {
	t.Helper()
	saved := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = saved
		_ = w.Close()
		_ = r.Close()
	})
	return func() string {
		_ = w.Close()
		data := make([]byte, 1<<20)
		n, _ := r.Read(data)
		return string(data[:n])
	}
}

// installForTest 装规范层并返回首行日志。
func installForTest(t *testing.T, opts Options) (string, map[string]any) {
	t.Helper()
	read := captureStdout(t)
	if err := Init(opts); err != nil {
		t.Fatalf("Init: %v", err)
	}
	slog.Info("test line")
	out := read()
	if strings.TrimSpace(out) == "" {
		t.Fatal("无输出")
	}
	line := strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]
	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		t.Fatalf("不是合法 JSON: %q -- %v", line, err)
	}
	return line, obj
}

// assertNoDuplicateKeys 断言一行 JSON 里没有重复键。
func assertNoDuplicateKeys(t *testing.T, line string) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(line))
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("不是合法 JSON: %q -- %v", line, err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		t.Fatalf("顶层不是 JSON 对象: %q", line)
	}
	seen := map[string]bool{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			t.Fatalf("读键失败: %q -- %v", line, err)
		}
		key, ok := kt.(string)
		if !ok {
			t.Fatalf("键不是字符串: %q", line)
		}
		if seen[key] {
			t.Errorf("重复键 %q: %s", key, line)
		}
		seen[key] = true
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("读值失败: %q -- %v", line, err)
		}
	}
}

// TestRequiredFieldsPresent 验证 9 个必填字段全部自动注入。
func TestRequiredFieldsPresent(t *testing.T) {
	_, obj := installForTest(t, Options{
		Service: ServiceName,
		Version: "1.2.3",
		Env:     "test",
	})
	for _, key := range []string{
		"timestamp", "severity_text", "severity_number", "level",
		"msg", "service", "version", "env", "instance_id",
	} {
		v, ok := obj[key]
		if !ok {
			t.Errorf("缺必填字段 %q", key)
			continue
		}
		if v == nil || v == "" {
			t.Errorf("必填字段 %q 为空", key)
		}
	}
	if obj["service"] != ServiceName {
		t.Errorf("service = %v, want %v", obj["service"], ServiceName)
	}
	if obj["version"] != "1.2.3" {
		t.Errorf("version = %v, want 1.2.3", obj["version"])
	}
	if obj["env"] != "test" {
		t.Errorf("env = %v, want test", obj["env"])
	}
}

// TestLevelIsLowercaseAlias 验证 level 是 severity_text 的小写别名。
//
// 下游 LogQL 用 level=~"error|fatal" 过滤；大写会静默漏掉全部错误日志。
func TestLevelIsLowercaseAlias(t *testing.T) {
	_, obj := installForTest(t, Options{
		Service: ServiceName, Version: "1.0.0", Env: "test", Level: slog.LevelInfo,
	})
	if obj["severity_text"] != "INFO" {
		t.Errorf("severity_text = %v, want INFO", obj["severity_text"])
	}
	if obj["level"] != "info" {
		t.Errorf("level = %v, want info（小写）", obj["level"])
	}
	if obj["severity_number"] != float64(9) {
		t.Errorf("severity_number = %v, want 9", obj["severity_number"])
	}
}

// TestReservedNameStripped 验证业务侧占用保留名会被剥离。
//
// 回归背景：cmd/gateway 的 preflight 此前打的是
// `slog.String("env", issue.EnvVar)` —— "env" 既是 rule 71 的部署环境名保留名，
// 又是这里的环境变量名。规范层注入 "env":"local" 后，同一行会有两个 "env" 键。
func TestReservedNameStripped(t *testing.T) {
	read := captureStdout(t)
	if err := Init(Options{Service: ServiceName, Version: "1.0.0", Env: "test"}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	slog.Info("preflight", slog.String("env", "SOME_ENV_VAR"), slog.String("keep", "yes"))
	out := read()
	line := strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]

	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		t.Fatalf("不是合法 JSON: %q -- %v", line, err)
	}
	assertNoDuplicateKeys(t, line)

	if obj["env"] != "test" {
		t.Errorf("env = %v, want test（规范层权威值）", obj["env"])
	}
	if strings.Contains(line, "SOME_ENV_VAR") {
		t.Errorf("业务侧 env 值仍在顶层: %s", line)
	}
	if obj["keep"] != "yes" {
		t.Errorf("非保留名字段被误删: %s", line)
	}
	conflicts, ok := obj["reserved_name_conflicts"].([]any)
	if !ok {
		t.Fatalf("缺 reserved_name_conflicts 诊断字段: %s", line)
	}
	if len(conflicts) == 0 || conflicts[0] != "env" {
		t.Errorf("reserved_name_conflicts = %v, want 含 env", conflicts)
	}
}

// TestSourceIsFlatString 验证 source 是平铺字符串而非嵌套对象。
//
// slog 的 AddSource 会输出 {"source":{"function":..,"file":..,"line":..}}
// —— 嵌套对象违反 rule 71 的扁平单层约束；本实现改成 "file.go:123"。
func TestSourceIsFlatString(t *testing.T) {
	line, obj := installForTest(t, Options{
		Service: ServiceName, Version: "1.0.0", Env: "test", AddSource: true,
	})
	src, ok := obj["source"].(string)
	if !ok {
		t.Fatalf("source 不是字符串: %s", line)
	}
	if !strings.Contains(src, ":") {
		t.Errorf("source = %q, want file.go:123 形态", src)
	}
	// 顶层必须是单层：没有嵌套对象。
	for k, v := range obj {
		if _, nested := v.(map[string]any); nested {
			t.Errorf("顶层出现嵌套对象 %q: %s", k, line)
		}
	}
}

// TestSensitiveKeyRedacted 验证敏感键被脱敏。
func TestSensitiveKeyRedacted(t *testing.T) {
	read := captureStdout(t)
	if err := Init(Options{Service: ServiceName, Version: "1.0.0", Env: "test"}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	slog.Info("login", slog.String("password", "hunter2"))
	out := read()
	if strings.Contains(out, "hunter2") {
		t.Errorf("敏感键未脱敏: %q", out)
	}
}

// TestInstallEarlyEmitsStandardLine 验证早装窗口也是规范格式。
//
// 回归背景：internal/config/feature.go 在配置加载期用标准库 log.Print 打日志。
// 若等 config.Load() 之后才装规范层，那行是旧格式 ⇒ 同一进程两种格式。
func TestInstallEarlyEmitsStandardLine(t *testing.T) {
	read := captureStdout(t)
	if err := InstallEarly(Options{Version: "1.0.0"}); err != nil {
		t.Fatalf("InstallEarly: %v", err)
	}
	slog.Info("early window")
	out := read()
	line := strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]

	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		t.Fatalf("早装窗口不是合法 JSON: %q -- %v", line, err)
	}
	for _, key := range []string{
		"timestamp", "severity_text", "severity_number", "level",
		"msg", "service", "version", "env", "instance_id",
	} {
		if obj[key] == nil || obj[key] == "" {
			t.Errorf("早装窗口缺必填字段 %q: %s", key, line)
		}
	}
}

// TestStdlibLogIsCapturedAfterEarlyInstall 验证早装后标准库 log 也走规范层。
//
// 这是本仓的真实场景：internal/config/feature.go 在**配置加载期间**用标准库
// log.Print 打 FEATURE_UNIFIED_SEARCH 的 deprecation 提示。
//
// slog.SetDefault 会把标准库 log 的输出重定向到 slog handler ⇒ 早装之后，
// 那行自动带上全部必填字段，不需要改那个调用点。
func TestStdlibLogIsCapturedAfterEarlyInstall(t *testing.T) {
	read := captureStdout(t)
	if err := InstallEarly(Options{Version: "1.0.0"}); err != nil {
		t.Fatalf("InstallEarly: %v", err)
	}
	// 标准库 log —— 与 internal/config/feature.go 同一个用法。
	stdlog.Print("config: some feature is deprecated")

	out := read()
	line := strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]
	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		t.Fatalf("标准库 log 未被规范层接管（不是合法 JSON）: %q -- %v", line, err)
	}
	for _, key := range []string{
		"timestamp", "severity_text", "severity_number", "level",
		"msg", "service", "version", "env", "instance_id",
	} {
		if obj[key] == nil || obj[key] == "" {
			t.Errorf("标准库 log 输出缺必填字段 %q: %s", key, line)
		}
	}
}

// TestInstanceIDIsProcessConstant 验证 instance_id 是进程级常量。
func TestInstanceIDIsProcessConstant(t *testing.T) {
	first := InstanceID()
	if first == "" {
		t.Fatal("InstanceID 为空")
	}
	for i := 0; i < 5; i++ {
		if got := InstanceID(); got != first {
			t.Fatalf("InstanceID 第 %d 次变化: %q != %q", i, got, first)
		}
	}
}

// TestNormalizeEnv 验证环境名归一。
func TestNormalizeEnv(t *testing.T) {
	cases := map[string]string{
		"production": "prod", "prod": "prod", "PROD": "prod",
		"staging": "staging", "development": "dev", "dev": "dev",
		"testing": "test", "test": "test", "local": "local",
		"": "local", "unknown-value": "local",
	}
	for raw, want := range cases {
		if got := NormalizeEnv(raw); got != want {
			t.Errorf("NormalizeEnv(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestInitRejectsInvalidParams 验证非法参数 fail-loud。
func TestInitRejectsInvalidParams(t *testing.T) {
	if err := Init(Options{Version: "", Env: "test"}); err == nil {
		t.Error("Version 为空应报错")
	}
}
