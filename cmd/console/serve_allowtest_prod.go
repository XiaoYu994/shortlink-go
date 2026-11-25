//go:build !devtest

package main

import "io"

type serveCLIOptions struct {
	allowTestHosts []string
}

// 生产构建的 console serve 不接收任何放行内网开关，守住 ADR 0027 生产入库校验边界。
func parseServeArgs(args []string, stderr io.Writer) (serveCLIOptions, bool, error) {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		writeUsage(stderr)
		return serveCLIOptions{}, false, nil
	}
	if len(args) > 0 {
		writeUsage(stderr)
		return serveCLIOptions{}, false, errUsage
	}
	return serveCLIOptions{}, true, nil
}
