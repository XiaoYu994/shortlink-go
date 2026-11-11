/*
Package link 是短链的核心业务：Link 结构体、业务错误、校验规则。

要做的事（第 3、4、6 课）：

  - Link 结构体和 NewLink 构造函数。
  - 哨兵错误 ErrNotFound，自定义错误类型 ValidationError。
  - 校验 URL：只允许 http 和 https。
  - 内存版的 Store（map 加 sync.RWMutex），先让服务跑起来，也留给单元测试用。
*/
package link
