package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// EnvConfigPath 指定共享 YAML 配置文件路径的环境变量名。
const EnvConfigPath = "SHORTLINK_CONFIG"

/*
Duration 封装 YAML 中的 Go 时长字符串（如 "5s"、"168h"）。
只接受字符串标量，拒绝无单位裸整数，避免把 5 静默解析成 5 纳秒。
*/
type Duration time.Duration

// UnmarshalYAML 实现 yaml.Unmarshaler，强制时长字段写成字符串。
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value == nil {
		return nil
	}
	for value.Kind == yaml.AliasNode && value.Alias != nil {
		value = value.Alias
	}
	if value.Tag == "!!null" {
		return nil
	}
	if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
		return errors.New("duration must be a string such as \"5s\"")
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(value.Value))
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// Std 转换回标准库 time.Duration。
func (d Duration) Std() time.Duration {
	return time.Duration(d)
}

/*
File 表示跨进程共享的 YAML 配置文件顶层骨架（ADR 0035）。
各节保留为 yaml.Node 延迟解码：redirector 只解码公共节与 redirector 节，
不引入 console/worker/smtp/outbound 的结构体或业务依赖（守住 ADR 0024）。
*/
type File struct {
	Features       yaml.Node `yaml:"features"`
	Database       yaml.Node `yaml:"database"`
	HTTP           yaml.Node `yaml:"http"`
	Cache          yaml.Node `yaml:"cache"`
	TrustedProxies yaml.Node `yaml:"trusted_proxies"`
	Outbound       yaml.Node `yaml:"outbound"`
	Log            yaml.Node `yaml:"log"`
	Tracing        yaml.Node `yaml:"tracing"`
	Redirector     yaml.Node `yaml:"redirector"`
	Console        yaml.Node `yaml:"console"`
	Worker         yaml.Node `yaml:"worker"`
	SMTP           yaml.Node `yaml:"smtp"`
}

// ResolvePath 按优先级解析配置文件路径：CLI --config 优先于 SHORTLINK_CONFIG。
func ResolvePath(flagPath string) string {
	if p := strings.TrimSpace(flagPath); p != "" {
		return p
	}
	return strings.TrimSpace(os.Getenv(EnvConfigPath))
}

/*
LoadFile 读取并校验共享 YAML 配置文件的顶层结构。
path 为空时返回空 *File，表示仅用代码默认值与环境变量启动；
存在路径时开启 KnownFields(true) 拒绝未知顶层键并拒绝多文档 YAML。
*/
func LoadFile(path string) (*File, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return &File{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %q: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f File
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return &File{}, nil
		}
		return nil, fmt.Errorf("parse config file %q: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config file %q must contain a single YAML document", path)
	}
	return &f, nil
}

/*
DecodeSection 把单个 yaml.Node 严格解码到目标结构体指针。
节点为空或 !!null 时直接返回，保留 dst 上已预填的代码默认值；
非空时重新编组并通过 yaml.NewDecoder 开启 KnownFields(true)，
确保嵌套字段拼写错误或误把密钥写进 YAML 时立即在启动阶段报错。
*/
func DecodeSection(node yaml.Node, dst any) error {
	for node.Kind == yaml.AliasNode && node.Alias != nil {
		node = *node.Alias
	}
	if node.Kind == 0 || node.Tag == "!!null" {
		return nil
	}
	raw, err := yaml.Marshal(&node)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

/*
LookupMappingString 从映射节点中读取指定字符串键（不解码同节其他字段），
用于 worker 跨节读取 console.public_url 而无需完整解码 console 节。
*/
func LookupMappingString(node yaml.Node, key string) (string, bool, error) {
	for node.Kind == yaml.AliasNode && node.Alias != nil {
		node = *node.Alias
	}
	if node.Kind == 0 || node.Tag == "!!null" {
		return "", false, nil
	}
	if node.Kind != yaml.MappingNode {
		return "", false, errors.New("section must be a YAML mapping")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		k := node.Content[i]
		for k.Kind == yaml.AliasNode && k.Alias != nil {
			k = k.Alias
		}
		if k.Kind != yaml.ScalarNode || k.Value != key {
			continue
		}
		v := node.Content[i+1]
		for v.Kind == yaml.AliasNode && v.Alias != nil {
			v = v.Alias
		}
		if v.Kind == 0 || v.Tag == "!!null" {
			return "", false, nil
		}
		if v.Kind != yaml.ScalarNode || v.Tag != "!!str" {
			return "", false, fmt.Errorf("%s must be a string", key)
		}
		return v.Value, true, nil
	}
	return "", false, nil
}
