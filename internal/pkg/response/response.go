package response

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"gowms/internal/pkg/errcode"
)

// Body 统一响应结构 {code, msg, data}。
type Body struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data"`
}

type pageData struct {
	List  any   `json:"list"`
	Total int64 `json:"total"`
}

func OK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, Body{Code: 0, Msg: "success", Data: data})
}

func OKPage(c *gin.Context, list any, total int64) {
	c.JSON(http.StatusOK, Body{Code: 0, Msg: "success", Data: pageData{List: list, Total: total}})
}

// Fail 将 error 归一化后输出；*errcode.Error 使用其 code/msg，其他按 500 处理。
func Fail(c *gin.Context, err error) {
	var e *errcode.Error
	if !errors.As(err, &e) {
		e = errcode.Internal
	}
	c.JSON(http.StatusOK, Body{Code: e.Code, Msg: e.Msg, Data: nil})
}
