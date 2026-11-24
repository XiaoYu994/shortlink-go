//go:build !devtest

package main

import "flag"

// 生产构建不编译 --allow-test-host 开关，从编译期守住 ADR 0020「绝不提供线上放行内网地址的开关」。
func registerWorkerTestFlags(*flag.FlagSet, *workerCLIOptions) {}

func validateWorkerTestFlags(workerCLIOptions) error { return nil }
