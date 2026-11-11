/*
Package config 从环境变量加载配置，并在启动时校验。

要做的事（第 8 课）：

  - Config 结构体和 LoadConfig() (Config, error)。
  - 缺少必需配置时直接返回错误（fail fast）。
  - 超时类配置用 time.ParseDuration 解析。
  - 用 t.Setenv 给 LoadConfig 写测试。
*/
package config
