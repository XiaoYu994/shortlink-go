package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
)

type serveCLIOptions struct {
	allowTestHosts []string
}

type workerCLIOptions struct {
	allowTestHosts []string
}

type stringSliceFlag []string

func (f *stringSliceFlag) String() string { return fmt.Sprint([]string(*f)) }

func (f *stringSliceFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

func parseServeArgs(args []string, stderr io.Writer) (serveCLIOptions, bool, error) {
	hosts, ok, err := parseLoopbackTestHosts("serve", "仅测试/冒烟使用：允许注册指定的回环 host:port 作为 Webhook 或短信网关目标", args, stderr)
	if !ok {
		return serveCLIOptions{}, false, err
	}
	return serveCLIOptions{allowTestHosts: hosts}, true, nil
}

func parseWorkerArgs(args []string, stderr io.Writer) (workerCLIOptions, bool, error) {
	hosts, ok, err := parseLoopbackTestHosts("worker", "仅测试/冒烟使用：放行指定的回环 host:port 供本地风控或 Webhook 联调", args, stderr)
	if !ok {
		return workerCLIOptions{}, false, err
	}
	return workerCLIOptions{allowTestHosts: hosts}, true, nil
}

func parseLoopbackTestHosts(name, usage string, args []string, stderr io.Writer) ([]string, bool, error) {
	fs := newFlagSet(name, stderr)
	var hosts []string
	fs.Var((*stringSliceFlag)(&hosts), "allow-test-host", usage)
	ok, err := parseFlags(fs, args, stderr)
	if !ok {
		return nil, false, err
	}
	for _, hp := range hosts {
		if err := validateLoopbackHostPort(hp); err != nil {
			return nil, false, fmt.Errorf("--allow-test-host %q: %w", hp, err)
		}
	}
	return hosts, true, nil
}

func validateLoopbackHostPort(hp string) error {
	host, portStr, err := net.SplitHostPort(hp)
	if err != nil {
		return errors.New("must be a valid host:port")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("port must be in 1..65535")
	}
	if host == "localhost" {
		return nil
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !addr.IsLoopback() {
		return errors.New("must be a loopback address such as 127.0.0.1:18089")
	}
	return nil
}
