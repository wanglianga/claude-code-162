package service

import "errors"

var (
	ErrInvalidCredentials = errors.New("手机号或密码错误")
	ErrNotFound           = errors.New("记录不存在")
	ErrConflict           = errors.New("状态冲突")
	ErrValidation         = errors.New("参数校验失败")
	ErrForbidden          = errors.New("无权操作")
)

type BizError struct {
	Code string
	Msg  string
}

func (e *BizError) Error() string { return e.Msg }

func bizError(code, msg string) error { return &BizError{Code: code, Msg: msg} }
