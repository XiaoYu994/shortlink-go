//go:build devtest

package main

import (
	"io"
)

type serveCLIOptions struct {
	allowTestHosts []string
}

func parseServeArgs(args []string, stderr io.Writer) (serveCLIOptions, bool, error) {
	fs := newFlagSet("serve", stderr)
	var opts serveCLIOptions
	fs.Var((*stringSliceFlag)(&opts.allowTestHosts), "allow-test-host", "仅测试/冒烟使用：允许注册指定的回环 host:port 作为 Webhook 或短信网关目标")
	ok, err := parseFlags(fs, args, stderr)
	if !ok {
		return serveCLIOptions{}, false, err
	}
	if err := validateLoopbackHostPorts(opts.allowTestHosts); err != nil {
		return serveCLIOptions{}, false, err
	}
	return opts, true, nil
}
