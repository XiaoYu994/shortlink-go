//go:build devtest

package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"strconv"
)

type stringSliceFlag []string

func (f *stringSliceFlag) String() string { return fmt.Sprint([]string(*f)) }

func (f *stringSliceFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

func registerWorkerTestFlags(fs *flag.FlagSet, opts *workerCLIOptions) {
	fs.Var((*stringSliceFlag)(&opts.allowTestHosts), "allow-test-host", "仅测试/冒烟使用：放行指定的回环 host:port 供本地风控联调")
}

func validateWorkerTestFlags(opts workerCLIOptions) error {
	return validateLoopbackHostPorts(opts.allowTestHosts)
}

func validateLoopbackHostPorts(hosts []string) error {
	for _, hp := range hosts {
		if err := validateLoopbackHostPort(hp); err != nil {
			return fmt.Errorf("--allow-test-host %q: %w", hp, err)
		}
	}
	return nil
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
