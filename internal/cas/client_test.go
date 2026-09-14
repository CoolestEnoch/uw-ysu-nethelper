package cas

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func respWithBody(code int, body string) *http.Response {
	return &http.Response{
		StatusCode: code,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// 金智 CAS 第一重失败返回 401 + 登录页 HTML（错误提示在页面内）。
func TestClassifyStep1Unauthorized(t *testing.T) {
	c := &Client{}
	page := `<html><div id="showErrorTip">您提供的用户名或者密码有误</div></html>`
	err := c.classifyStep1Response(context.Background(), respWithBody(http.StatusUnauthorized, page))
	if !errors.Is(err, ErrLoginFailed) {
		t.Fatalf("401 with error message: got %v, want ErrLoginFailed", err)
	}
	if !strings.Contains(err.Error(), "您提供的用户名或者密码有误") {
		t.Fatalf("error should carry server message, got %v", err)
	}
}

// 账号累计失败达阈值后 needCaptcha="true"，无论错误文案如何都应判 ErrNeedCaptcha。
func TestClassifyStep1NeedsCaptcha(t *testing.T) {
	c := &Client{}
	page := `<html><script>var needCaptcha = "true";</script>` +
		`<div id="showErrorTip">您提供的用户名或者密码有误</div></html>`
	err := c.classifyStep1Response(context.Background(), respWithBody(http.StatusUnauthorized, page))
	if !errors.Is(err, ErrNeedCaptcha) {
		t.Fatalf("401 with needCaptcha=true: got %v, want ErrNeedCaptcha", err)
	}
}

// 200 错误页是历史观测到的路径，保持原有分类行为。
func TestClassifyStep1OKErrorPage(t *testing.T) {
	c := &Client{}
	page := `<html><div id="showErrorTip">您提供的用户名或者密码有误</div></html>`
	if err := c.classifyStep1Response(context.Background(), respWithBody(http.StatusOK, page)); !errors.Is(err, ErrLoginFailed) {
		t.Fatalf("200 with error message: got %v, want ErrLoginFailed", err)
	}
	captchaMsg := `<html><div id="showErrorTip">请输入验证码</div></html>`
	if err := c.classifyStep1Response(context.Background(), respWithBody(http.StatusOK, captchaMsg)); !errors.Is(err, ErrNeedCaptcha) {
		t.Fatalf("200 with captcha message: got %v, want ErrNeedCaptcha", err)
	}
}

func TestClassifyStep1OtherStatuses(t *testing.T) {
	c := &Client{}
	if err := c.classifyStep1Response(context.Background(), respWithBody(http.StatusInternalServerError, "")); !errors.Is(err, ErrProtocol) {
		t.Fatalf("500: got %v, want ErrProtocol", err)
	}
	frozen := `<html><body>IP被冻结</body></html>`
	if err := c.classifyStep1Response(context.Background(), respWithBody(http.StatusUnauthorized, frozen)); !errors.Is(err, ErrIPBlocked) {
		t.Fatalf("401 frozen page: got %v, want ErrIPBlocked", err)
	}
	// 无错误文案的 401 也按凭据失败处理，而非协议错误（熔断会兜底）
	if err := c.classifyStep1Response(context.Background(), respWithBody(http.StatusUnauthorized, "<html></html>")); !errors.Is(err, ErrLoginFailed) {
		t.Fatalf("401 without message: got %v, want ErrLoginFailed", err)
	}
}
