/*
Package logx 提供进程级结构化日志构造、context 关联字段注入、请求级宽事件收集（Canonical Log Line）、
动态日志级别与敏感信息脱敏防线。

本地 text 模式接入 github.com/lmittmann/tint 实现可读的终端彩色输出（仅当输出目标是终端 TTY 且未设 NO_COLOR 时启用颜色）；
生产 json 模式使用标准库 slog.JSONHandler。本包不依赖任何业务包，避免循环依赖。
*/
package logx

import (
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/lmittmann/tint"
)

const (
	// FormatText 输出人类可读的终端友好格式（本地开发默认，底层使用 tint.NewTextHandler）。
	FormatText = "text"
	// FormatJSON 输出单行 JSON 日志（生产采集推荐）。
	FormatJSON = "json"

	tintTimeFormat = "15:04:05.000"
)

// Options 控制根日志器的输出格式、初始级别、动态 LevelVar 与固定 service 标签。
type Options struct {
	Format  string
	Level   slog.Level
	Leveler *slog.LevelVar
	Service string
}

/*
New 构造带全局脱敏（ReplaceAttr）与 context 关联字段提取（contextHandler）的根 *slog.Logger，
同时返回可用于运行时热切换级别的 *slog.LevelVar。
*/
func New(w io.Writer, opts Options) (*slog.Logger, *slog.LevelVar) {
	if w == nil {
		w = io.Discard
	}
	levelVar := opts.Leveler
	if levelVar == nil {
		levelVar = new(slog.LevelVar)
		levelVar.Set(opts.Level)
	}

	var base slog.Handler
	if strings.EqualFold(opts.Format, FormatJSON) {
		base = slog.NewJSONHandler(w, &slog.HandlerOptions{
			Level:       levelVar,
			ReplaceAttr: replaceAttr,
		})
	} else {
		base = tint.NewTextHandler(w, &tint.Options{
			Level:       levelVar,
			TimeFormat:  tintTimeFormat,
			NoColor:     shouldDisableColor(w),
			ReplaceAttr: replaceTintAttr,
		})
	}

	logger := slog.New(WrapContext(base))
	if opts.Service != "" {
		logger = logger.With("service", opts.Service)
	}
	return logger, levelVar
}

/*
NewFallback 在配置加载失败（尚未拿到 BaseConfig）或纯 CLI 辅助路径中构造遵循环境变量
LOG_FORMAT / LOG_LEVEL 的兜底日志器，避免退回 slog 默认全局 text logger。
*/
func NewFallback(w io.Writer, service string) *slog.Logger {
	format := FormatText
	if strings.EqualFold(strings.TrimSpace(os.Getenv("LOG_FORMAT")), FormatJSON) {
		format = FormatJSON
	}
	level := slog.LevelInfo
	if raw := strings.TrimSpace(os.Getenv("LOG_LEVEL")); raw != "" {
		var parsed slog.Level
		if err := parsed.UnmarshalText([]byte(raw)); err == nil {
			level = parsed
		}
	}
	logger, _ := New(w, Options{
		Format:  format,
		Level:   level,
		Service: service,
	})
	return logger
}

func replaceTintAttr(groups []string, a slog.Attr) slog.Attr {
	a = replaceAttr(groups, a)
	if a.Key == "err" && a.Value.Kind() == slog.KindAny {
		if err, ok := a.Value.Any().(error); ok {
			return tint.Err(err)
		}
	}
	return a
}

func shouldDisableColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return true
	}
	f, ok := w.(*os.File)
	if !ok {
		return true
	}
	info, err := f.Stat()
	if err != nil {
		return true
	}
	return (info.Mode() & os.ModeCharDevice) == 0
}
