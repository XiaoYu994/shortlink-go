/*
Package idgen 负责短码的生成与解析。

要做的事（第 2、6 课）：

  - EncodeBase62(n int64) string
  - DecodeBase62(s string) (int64, error)：短码来自用户输入，
    要校验非法字符和 int64 溢出。
  - 表驱动测试，覆盖 0、61、62、最大值和非法输入。
*/
package idgen
