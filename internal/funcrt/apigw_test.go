package funcrt

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestToEventBasic(t *testing.T) {
	r := httptest.NewRequest("GET", "http://svc.example/orders/7?a=1&a=2", nil)
	r.Header.Set("X-Foo", "bar")
	r.Header.Add("X-Multi", "1")
	r.Header.Add("X-Multi", "2")
	r.Header.Set("Cookie", "a=1; b=2")
	r.Header.Set("User-Agent", "ua/1")
	r.RemoteAddr = "10.1.2.3:5555"
	now := time.UnixMilli(1700000000123)

	ev, err := ToEvent(r, "req-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Version != "2.0" || ev.RouteKey != "$default" {
		t.Errorf("version/routeKey = %q/%q", ev.Version, ev.RouteKey)
	}
	if ev.RawPath != "/orders/7" || ev.RawQueryString != "a=1&a=2" {
		t.Errorf("raw = %q ? %q", ev.RawPath, ev.RawQueryString)
	}
	if got := ev.QueryStringParameters["a"]; got != "1,2" {
		t.Errorf("query a = %q", got)
	}
	if ev.Headers["host"] != "svc.example" {
		t.Errorf("host header = %q", ev.Headers["host"])
	}
	if ev.Headers["x-foo"] != "bar" || ev.Headers["x-multi"] != "1,2" {
		t.Errorf("headers = %v", ev.Headers)
	}
	if len(ev.Cookies) != 2 || ev.Cookies[0] != "a=1" || ev.Cookies[1] != "b=2" {
		t.Errorf("cookies = %v", ev.Cookies)
	}
	h := ev.RequestContext.HTTP
	if h.Method != "GET" || h.Path != "/orders/7" || h.Protocol != "HTTP/1.1" || h.SourceIP != "10.1.2.3" || h.UserAgent != "ua/1" {
		t.Errorf("http ctx = %+v", h)
	}
	rc := ev.RequestContext
	if rc.RequestID != "req-1" || rc.TimeEpoch != 1700000000123 || rc.Stage != "$default" || rc.DomainName != "svc.example" {
		t.Errorf("request ctx = %+v", rc)
	}
	if ev.IsBase64Encoded {
		t.Error("empty body should not be base64")
	}
}

func TestToEventBinaryBody(t *testing.T) {
	raw := []byte{0xff, 0x00}
	r := httptest.NewRequest("POST", "/up", strings.NewReader(string(raw)))
	r.Header.Set("Content-Type", "image/png")
	ev, err := ToEvent(r, "id", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !ev.IsBase64Encoded {
		t.Fatal("want base64")
	}
	got, err := base64.StdEncoding.DecodeString(ev.Body)
	if err != nil || string(got) != string(raw) {
		t.Errorf("decoded = %v, %v", got, err)
	}
}

func TestToEventJSONBody(t *testing.T) {
	for _, ct := range []string{"application/json", "application/vnd.api+json; charset=utf-8", "text/plain"} {
		r := httptest.NewRequest("POST", "/x", strings.NewReader(`{"a":1}`))
		r.Header.Set("Content-Type", ct)
		ev, err := ToEvent(r, "id", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if ev.IsBase64Encoded || ev.Body != `{"a":1}` {
			t.Errorf("%s: base64=%v body=%q", ct, ev.IsBase64Encoded, ev.Body)
		}
	}
}

func TestToEventInvalidUTF8TextBody(t *testing.T) {
	r := httptest.NewRequest("POST", "/x", strings.NewReader("\xff\xfe"))
	r.Header.Set("Content-Type", "text/plain")
	ev, _ := ToEvent(r, "id", time.Now())
	if !ev.IsBase64Encoded {
		t.Error("invalid UTF-8 must be base64")
	}
}

func TestWriteResponseStructured(t *testing.T) {
	w := httptest.NewRecorder()
	WriteResponse(w, Result{Status: 200, Payload: []byte(`{"statusCode":201,"headers":{"x-a":"b"},"multiValueHeaders":{"x-m":["1","2"]},"cookies":["c=1","d=2"],"body":"ok"}`)})
	if w.Code != 201 || w.Header().Get("X-A") != "b" || w.Body.String() != "ok" {
		t.Errorf("code=%d hdr=%v body=%q", w.Code, w.Header(), w.Body.String())
	}
	if got := w.Header().Values("Set-Cookie"); len(got) != 2 || got[0] != "c=1" {
		t.Errorf("set-cookie = %v", got)
	}
	if got := w.Header().Values("X-M"); len(got) != 2 {
		t.Errorf("x-m = %v", got)
	}
}

func TestWriteResponseBase64(t *testing.T) {
	w := httptest.NewRecorder()
	enc := base64.StdEncoding.EncodeToString([]byte{0xff, 0x00, 0x01})
	WriteResponse(w, Result{Status: 200, Payload: []byte(`{"statusCode":200,"isBase64Encoded":true,"body":"` + enc + `"}`)})
	if w.Body.String() != "\xff\x00\x01" {
		t.Errorf("body = %v", w.Body.Bytes())
	}
}

func TestWriteResponseSimple(t *testing.T) {
	w := httptest.NewRecorder()
	WriteResponse(w, Result{Status: 200, Payload: []byte(`{"hello":"x"}`)})
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" || w.Body.String() != `{"hello":"x"}` {
		t.Errorf("code=%d hdr=%v body=%q", w.Code, w.Header(), w.Body.String())
	}
}

// R17: failures keep their status but carry API Gateway's generic body, never
// the Lambda error document.
func TestWriteResponseErrors(t *testing.T) {
	for st, body := range map[int]string{
		http.StatusInternalServerError: `{"message":"Internal Server Error"}`,
		http.StatusBadGateway:          `{"message":"Bad Gateway"}`,
		http.StatusGatewayTimeout:      `{"message":"Gateway Timeout"}`,
	} {
		w := httptest.NewRecorder()
		WriteResponse(w, Result{Status: st, Payload: []byte(`{"errorMessage":"secret detail"}`), RequestID: "r1"})
		if w.Code != st || w.Header().Get("Content-Type") != "application/json" || w.Body.String() != body {
			t.Errorf("status %d: code=%d ct=%q body=%q; want body %s", st, w.Code, w.Header().Get("Content-Type"), w.Body.String(), body)
		}
	}
}

func TestWriteResponseInvalidStatusCodeIsGeneric502(t *testing.T) {
	w := httptest.NewRecorder()
	WriteResponse(w, Result{Status: 200, Payload: []byte(`{"statusCode":5,"headers":{"x-a":"b"},"body":"x"}`)})
	if w.Code != 502 || w.Body.String() != `{"message":"Bad Gateway"}` || w.Header().Get("x-a") != "" {
		t.Errorf("code=%d hdr=%v body=%q", w.Code, w.Header(), w.Body.String())
	}
}

func TestWriteResponseDefaultContentType(t *testing.T) {
	w := httptest.NewRecorder()
	WriteResponse(w, Result{Status: 200, Payload: []byte(`{"statusCode":200,"body":"{\"a\":1}"}`)})
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("default content-type = %q", ct)
	}
	for _, p := range []string{
		`{"statusCode":200,"headers":{"content-type":"text/plain"},"body":"x"}`,
		`{"statusCode":200,"multiValueHeaders":{"Content-Type":["text/plain"]},"body":"x"}`,
	} {
		w = httptest.NewRecorder()
		WriteResponse(w, Result{Status: 200, Payload: []byte(p)})
		if ct := w.Header().Get("Content-Type"); ct != "text/plain" {
			t.Errorf("%s: content-type = %q", p, ct)
		}
	}
}
