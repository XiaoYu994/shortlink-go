package httpapi

import "net/http"

// middleware 在请求到达 handler 前后插入行为。它的签名和 http.Handler 套娃一致，chain 才能按同一方式组合。
type middleware func(http.Handler) http.Handler

/*
chain 把中间件套到 handler 外面。
列表里靠前的在更外层：请求先进入它，响应最后从它离开。
从最后一个开始套，这样参数顺序和请求经过的顺序一致。
*/
func chain(h http.Handler, mws ...middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
