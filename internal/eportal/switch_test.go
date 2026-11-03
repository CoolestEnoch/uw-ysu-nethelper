package eportal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"ysunethelper/internal/httpkit"
)

type portalStep struct {
	method  string
	path    string
	session string
	payload string
	data    string
}

// 脚本严格约束请求顺序和内容；任何 CAS、logout 或重复 setService 都会失败。
type scriptedPortal struct {
	t     *testing.T
	steps []portalStep
}

func (s *scriptedPortal) next(method, rawURL string) portalStep {
	s.t.Helper()
	if len(s.steps) == 0 {
		s.t.Fatalf("unexpected %s %s", method, rawURL)
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	u, err := url.Parse(rawURL)
	if err != nil || method != step.method || u.Path != step.path {
		s.t.Fatalf("got %s %s; want %s %s", method, rawURL, step.method, step.path)
	}
	if method == "GET" && u.Query().Get("sessionId") != step.session {
		s.t.Fatalf("incorrect session: %s", rawURL)
	}
	return step
}

func (s *scriptedPortal) Follow(_ context.Context, rawURL string, _ int) (*httpkit.FollowResult, error) {
	step := s.next("FOLLOW", rawURL)
	return &httpkit.FollowResult{FinalURL: baseURL + "/portal/portal-main?sessionId=" + step.session}, nil
}

func response(data string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(data))}
}

func (s *scriptedPortal) Get(_ context.Context, rawURL string) (*http.Response, error) {
	return response(s.next("GET", rawURL).data), nil
}

func (s *scriptedPortal) PostJSON(_ context.Context, rawURL string, body io.Reader) (*http.Response, error) {
	step := s.next("POST", rawURL)
	var got, want any
	if err := json.NewDecoder(body).Decode(&got); err != nil {
		s.t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(step.payload), &want); err != nil {
		s.t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		s.t.Fatalf("payload = %#v, want %#v", got, want)
	}
	return response(step.data), nil
}

func envelopeData(data string) string { return `{"code":200,"data":` + data + `}` }

func sessionSteps(session, service string, online bool) []portalStep {
	data := `{"result":"fail"}`
	if online {
		data = `{"result":"success","userName":"test-user","realServiceName":` + strconvQuote(service) + `}`
	}
	return []portalStep{
		{method: "FOLLOW", path: "/eportal/redirect.jsp", session: session},
		{method: "GET", path: "/eportal/adaptor/getOnlineUserInfo", session: session,
			data: envelopeData(`{"portalOnlineUserInfo":` + data + `}`)},
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func serviceListStep(data string) portalStep {
	return portalStep{method: "POST", path: "/eportal/network/serviceSelection",
		payload: `{"sessionId":"before","selectType":1}`, data: envelopeData(data)}
}

func switchStep(data string) portalStep {
	return portalStep{method: "POST", path: "/eportal/network/setService",
		payload: `{"sessionId":"before","serviceName":"中国联通","rememberService":false,"browserFgr":null}`, data: data}
}

func newScriptedClient(t *testing.T, steps []portalStep) *Client {
	t.Helper()
	script := &scriptedPortal{t: t, steps: steps}
	t.Cleanup(func() {
		if len(script.steps) != 0 {
			t.Errorf("%d expected requests were not made", len(script.steps))
		}
	})
	return &Client{hc: script}
}

const twoServices = `[{"key":"校园网","value":"校园网","order":1},{"key":"中国联通","value":"中国联通","order":2}]`

func TestSwitchServiceSelectors(t *testing.T) {
	for number, names := range [][]string{
		{"0", "campus", "校园网", "校园"},
		{"1", "telecom", "电信", "中国电信"},
		{"2", "mobile", "移动", "中国移动"},
		{"3", "unicom", "联通", "中国联通", " UNICOM "},
	} {
		for _, name := range names {
			if got := ResolveSwitchService(name); got != switchServiceNames[number] {
				t.Errorf("ResolveSwitchService(%q) = %q", name, got)
			}
			if got, ok := ServiceNumber(name); !ok || got != number {
				t.Errorf("ServiceNumber(%q) = %d, %v", name, got, ok)
			}
		}
	}
}

func TestSwitchServiceReusesOnlineSession(t *testing.T) {
	steps := sessionSteps("before", "校园网", true)
	steps = append(steps, serviceListStep(twoServices), switchStep(envelopeData(`{"switchResult":"success"}`)))
	steps = append(steps, sessionSteps("after", "中国联通", true)...)
	status, err := newScriptedClient(t, steps).SwitchService(context.Background(), "unicom")
	if err != nil || status.Service != "中国联通" || !status.Online {
		t.Fatalf("status = %+v, error = %v", status, err)
	}
}

func TestSwitchServiceAlreadySelected(t *testing.T) {
	c := newScriptedClient(t, sessionSteps("before", "中国联通", true))
	if _, err := c.SwitchService(context.Background(), "unicom"); err != nil {
		t.Fatal(err)
	}
}

func TestSwitchServiceRejectsBeforeMutation(t *testing.T) {
	for _, tt := range []struct {
		name, target string
		online       bool
		list         bool
	}{
		{"offline", "unicom", false, false},
		{"unavailable carrier", "telecom", true, true},
		{"empty target", " ", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var steps []portalStep
			if strings.TrimSpace(tt.target) != "" {
				steps = sessionSteps("before", "校园网", tt.online)
			}
			if tt.list {
				steps = append(steps, serviceListStep(twoServices))
			}
			_, err := newScriptedClient(t, steps).SwitchService(context.Background(), tt.target)
			if !errors.Is(err, ErrBusiness) {
				t.Fatalf("expected business error, got %v", err)
			}
		})
	}
}

func TestSwitchServiceFailureDoesNotRetryOrReportSuccess(t *testing.T) {
	for _, tt := range []struct {
		name, data, message string
		kind                error
	}{
		{"offline failure", envelopeData(`{"switchResult":"fail","failType":"offlineFail","message":"下线失败"}`), "下线失败", ErrBusiness},
		{"login failure", envelopeData(`{"switchResult":"fail","failType":"loginFail","message":"未绑定运营商"}`), "原服务可能已下线", ErrBusiness},
		{"envelope failure", `{"code":500,"message":"会话过期"}`, "会话过期", ErrBusiness},
		{"missing result", envelopeData(`{}`), "missing switchResult", ErrProtocol},
		{"invalid response", envelopeData(`[]`), "bad setService data", ErrProtocol},
	} {
		t.Run(tt.name, func(t *testing.T) {
			steps := sessionSteps("before", "校园网", true)
			steps = append(steps, serviceListStep(twoServices), switchStep(tt.data))
			_, err := newScriptedClient(t, steps).SwitchService(context.Background(), "unicom")
			if !errors.Is(err, tt.kind) || !strings.Contains(err.Error(), tt.message) {
				t.Fatalf("error = %v, want %v containing %q", err, tt.kind, tt.message)
			}
		})
	}
}

func TestSwitchServiceVerifiesActualCarrier(t *testing.T) {
	for _, online := range []bool{true, false} {
		steps := sessionSteps("before", "校园网", true)
		steps = append(steps, serviceListStep(twoServices), switchStep(envelopeData(`{"switchResult":"success"}`)))
		steps = append(steps, sessionSteps("after", "校园网", online)...)
		if _, err := newScriptedClient(t, steps).SwitchService(context.Background(), "unicom"); err == nil {
			t.Fatal("reported success without actually connecting to requested service")
		}
	}
}

func TestServicesUsesServerValuesAndOrder(t *testing.T) {
	steps := sessionSteps("before", "校园网", true)
	steps = append(steps, serviceListStep(`[{"key":"联通显示名","value":"unicom-id"},{"key":"校园网","value":"campus-id","order":1}]`))
	_, services, err := newScriptedClient(t, steps).Services(context.Background())
	if err != nil || len(services) != 2 || services[0].Value != "campus-id" || services[1].Key != "联通显示名" {
		t.Fatalf("services = %+v, error = %v", services, err)
	}
}

func TestServicesRejectsMissingOrMalformedChoices(t *testing.T) {
	for _, data := range []string{`null`, `[]`, `{}`, `[{"key":"校园网"}]`} {
		t.Run(data, func(t *testing.T) {
			steps := sessionSteps("before", "中国联通", true)
			steps = append(steps, serviceListStep(data))
			if _, _, err := newScriptedClient(t, steps).Services(context.Background()); err == nil {
				t.Fatal("invalid service list should not be offered for switching")
			}
		})
	}
}

func TestSwitchServiceSubmitsValueAndVerifiesDisplayName(t *testing.T) {
	steps := sessionSteps("before", "校园网", true)
	steps = append(steps, serviceListStep(`[{"key":"中国联通","value":"unicom-id"}]`))
	step := switchStep(envelopeData(`{"switchResult":"success"}`))
	step.payload = `{"sessionId":"before","serviceName":"unicom-id","rememberService":false,"browserFgr":null}`
	steps = append(steps, step)
	steps = append(steps, sessionSteps("after", "中国联通", true)...)
	if _, err := newScriptedClient(t, steps).SwitchService(context.Background(), "unicom"); err != nil {
		t.Fatal(err)
	}
}
