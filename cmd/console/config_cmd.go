package main

import (
	"fmt"
	"io"
	"log/slog"
	"strings"

	"shortlink/internal/logx"
)

type configDumpOptions struct {
	configPath string
	format     string
}

func parseConfigCommand(args []string, stderr io.Writer) (configDumpOptions, bool, error) {
	if len(args) == 0 {
		writeUsage(stderr)
		return configDumpOptions{}, false, errUsage
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		writeUsage(stderr)
		return configDumpOptions{}, false, nil
	}
	if args[0] != "dump" {
		writeUsage(stderr)
		return configDumpOptions{}, false, errUsage
	}

	fs := newFlagSet("config dump", stderr)
	configFlag := fs.String("config", "", "YAML 配置文件路径（覆盖 SHORTLINK_CONFIG）")
	formatFlag := fs.String("format", logx.FormatJSON, "输出格式：json 或 text")
	ok, err := parseFlags(fs, args[1:], stderr)
	if !ok {
		return configDumpOptions{}, false, err
	}
	format := strings.ToLower(strings.TrimSpace(*formatFlag))
	if format != logx.FormatJSON && format != logx.FormatText {
		_, _ = fmt.Fprintf(stderr, "console config dump: --format must be json or text, got %q\n", format)
		return configDumpOptions{}, false, errUsage
	}
	return configDumpOptions{
		configPath: strings.TrimSpace(*configFlag),
		format:     format,
	}, true, nil
}

/*
runConfigDump 同时加载并校验 ConsoleConfig 与 WorkerConfig，
通过既有 LogValue() 脱敏输出最终生效配置，供部署前核对与故障排查。
*/
func runConfigDump(opts configDumpOptions, stdout io.Writer) error {
	consoleCfg, err := loadConsoleConfig(opts.configPath)
	if err != nil {
		return err
	}
	workerCfg, err := loadWorkerConfig(opts.configPath)
	if err != nil {
		return err
	}

	format := opts.format
	if format == "" {
		format = logx.FormatJSON
	}
	logger, _ := logx.New(stdout, logx.Options{
		Format: format,
		Level:  slog.LevelInfo,
	})
	logger.Info("effective config",
		"console", consoleCfg.LogValue(),
		"worker", workerCfg.LogValue(),
	)
	return nil
}
