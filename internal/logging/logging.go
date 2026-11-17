// Package logging — rule 71 规范层的安装入口
//
// -----------------------------------------------------------------------------
// Purpose:        决定「什么时候装规范层」「装成什么样」：
//
//	Init（完整安装）与 InstallEarly（配置加载前的早装窗口）。
//
// Replaces:       (无)
// Replaced-by:    (无)
// Status:         active
//
// Changelog:
//
//	2026-11-09  v1.0  首版。随 vibe-coding rule 71 落地。
//
// -----------------------------------------------------------------------------
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// ServiceName 是服务标识。
//
// 必须与 deploy/platform/services.json 的服务键一致（kebab-case）——
// AI 侧按 service 分组、Loki 按 service 打标签，两边靠这个键对齐。
const ServiceName = "goacos"

// EnvVar 是**部署环境**的环境变量名（与 internal/config 读的同一个）。
const EnvVar = "GOACOS_ENV"

// LogLevelVar 是**日志级别**的环境变量名（与 internal/config 读的同一个）。
const LogLevelVar = "GOACOS_LOG_LEVEL"

// Options 是规范层的初始化参数。
type Options struct {
	// Service 是服务标识。留空则用 ServiceName。
	Service string
	// Version 是服务版本，必须由构建期注入（ldflags -X），禁止硬编码。
	Version string
	// Env 是部署环境：local / dev / test / staging / prod。
	Env string
	// Level 是最低日志级别。
	Level slog.Level
	// InstanceID 可选；留空则用进程级 InstanceID()。
	InstanceID string
	// DisableRedact 关闭脱敏。**仅允许在测试中使用**。
	DisableRedact bool
	// AddSource 输出调用位置（平铺成 "file.go:123"）。
	//
	// 不用 slog 的 AddSource：那个字段是嵌套对象，违反扁平单层约束。
	AddSource bool
}

// validEnvs 是允许的环境取值（rule 71 §4.1）。
var validEnvs = map[string]bool{
	"local": true, "dev": true, "test": true, "staging": true, "prod": true,
}

// EnvNames 返回允许的环境取值（错误信息与文档用）。
func EnvNames() []string {
	return []string{"local", "dev", "test", "staging", "prod"}
}

// NormalizeEnv 把部署环境归一到 rule 71 的允许集合。
//
// 各仓历史取值不一（"development" / "production" / "PROD" …），
// 不归一会让 AI 按 env 分组时出现 dev 与 development 两个桶。
// 无法识别时回落到 "local"——写入非法值会让日志被门禁判为不合规，
// 而「服务起不来」比「环境标记不准」严重得多。
func NormalizeEnv(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "production", "prod":
		return "prod"
	case "staging", "stage":
		return "staging"
	case "development", "dev":
		return "dev"
	case "testing", "test":
		return "test"
	case "local":
		return "local"
	default:
		return "local"
	}
}

// resolve 校验并归一参数，构造资源字段。
func (opts Options) resolve() (Resource, error) {
	service := opts.Service
	if service == "" {
		service = ServiceName
	}
	if opts.Version == "" {
		return Resource{}, fmt.Errorf("logging: Version 不能为空（须构建期注入）")
	}
	env := NormalizeEnv(opts.Env)
	if !validEnvs[env] {
		return Resource{}, fmt.Errorf("logging: Env=%q 非法，允许 %v", opts.Env, EnvNames())
	}
	instanceID := opts.InstanceID
	if instanceID == "" {
		instanceID = InstanceID()
	}
	return Resource{
		Service:    service,
		Version:    opts.Version,
		Env:        env,
		InstanceID: instanceID,
	}, nil
}

// install 把规范层装到 slog 默认 logger（输出 stdout）。
//
// **所有**安装 handler 的路径都必须走这里——否则某条路径漏了 ReplaceAttr，
// 运行期就会退回旧格式（字段名 time、级别大写、无必填字段）。
func install(opts Options) error {
	res, err := opts.resolve()
	if err != nil {
		return err
	}
	level := opts.Level
	if level == 0 {
		level = slog.LevelInfo
	}
	slog.SetDefault(slog.New(&standardHandler{
		inner:     newJSONHandler(os.Stdout, level),
		res:       res,
		redact:    !opts.DisableRedact,
		addSource: opts.AddSource,
	}))
	return nil
}

// NewStandardHandler 返回符合 rule 71 的 slog.Handler（新建 JSON 输出）。
//
// 用途：需要**自己构造** *slog.Logger 的场景——
//   - 把 logger 传给第三方/自研 SDK（goacos 的 registrar 就是这样）
//   - 多路输出时自己组装 io.MultiWriter
//
// 与 Init 的区别：Init 装到 slog 的**默认 logger**（全局生效），
// NewStandardHandler 只返回一个 handler，**不碰全局状态**。
func NewStandardHandler(w io.Writer, opts Options) (slog.Handler, error) {
	res, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	level := opts.Level
	if level == 0 {
		level = slog.LevelInfo
	}
	return &standardHandler{
		inner:  newJSONHandler(w, level),
		res:    res,
		redact: !opts.DisableRedact,
	}, nil
}

// Init 安装规范层（输出 stdout）。
//
// 必须在**配置加载之后**调用——因为 Level/Env 来自配置。
// 但配置加载期间本身会打日志 ⇒ 那段窗口用 InstallEarly 覆盖。
func Init(opts Options) error {
	return install(opts)
}

// InstallEarly 在**配置加载之前**装上规范层（输出 stdout）。
//
// 为什么需要（rule 71 §4.5 ①）：配置加载过程本身就会打日志——
// 本仓 internal/config/feature.go 在加载期用标准库 log.Print 打
// "FEATURE_UNIFIED_SEARCH is deprecated"。若等 Init（在 config.Load() 之后）
// 才装规范层，那行就是旧格式（纯文本、缺必填字段）⇒ 同一进程两种格式。
//
// 标准库 log 的输出会被 slog.SetDefault 接走 ⇒ 早装之后那行也自动合规。
//
// 后续 Init 会整体替换它，因此这里不需要处理文件句柄，也不会泄漏。
func InstallEarly(opts Options) error {
	if opts.Env == "" {
		opts.Env = os.Getenv(EnvVar)
	}
	if opts.Level == 0 {
		opts.Level = LevelFromEnv(os.Getenv(LogLevelVar))
	}
	return install(opts)
}

// LevelFromEnv 从环境变量取日志级别。
//
// 早装窗口只有几行日志，级别取不准无所谓——但**默认值必须是 info**：
// 用 debug 会让早期窗口静默，用 trace 则会先装一个比最终级别更宽的 handler。
func LevelFromEnv(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
