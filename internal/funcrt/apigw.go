package funcrt

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-lambda-go/events"
)

// ToEvent converts r into an API Gateway HTTP API payload v2.0 event. It reads
// r.Body to the end; wrap it with http.MaxBytesReader to bound the size.
func ToEvent(r *http.Request, requestID string, now time.Time) (events.APIGatewayV2HTTPRequest, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return events.APIGatewayV2HTTPRequest{}, err
	}

	headers := make(map[string]string, len(r.Header))
	for k, v := range r.Header {
		if strings.EqualFold(k, "Cookie") {
			continue // delivered via Cookies, as API Gateway does
		}
		headers[strings.ToLower(k)] = strings.Join(v, ",")
	}

	if r.Host != "" {
		headers["host"] = r.Host // Go strips Host from r.Header
	}

	var query map[string]string
	if q := r.URL.Query(); len(q) > 0 {
		query = make(map[string]string, len(q))
		for k, v := range q {
			query[k] = strings.Join(v, ",")
		}
	}

	var cookies []string
	for _, line := range r.Header.Values("Cookie") {
		for c := range strings.SplitSeq(line, ";") {
			if c = strings.TrimSpace(c); c != "" {
				cookies = append(cookies, c)
			}
		}
	}

	sourceIP := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		sourceIP = host
	}

	path := r.URL.EscapedPath()
	if path == "" {
		path = "/"
	}

	ev := events.APIGatewayV2HTTPRequest{
		Version:               "2.0",
		RouteKey:              "$default",
		RawPath:               path,
		RawQueryString:        r.URL.RawQuery,
		Cookies:               cookies,
		Headers:               headers,
		QueryStringParameters: query,
		RequestContext: events.APIGatewayV2HTTPRequestContext{
			DomainName: r.Host,
			RequestID:  requestID,
			Stage:      "$default",
			TimeEpoch:  now.UnixMilli(),
			HTTP: events.APIGatewayV2HTTPRequestContextHTTPDescription{
				Method:    r.Method,
				Path:      path,
				Protocol:  r.Proto,
				SourceIP:  sourceIP,
				UserAgent: r.UserAgent(),
			},
		},
	}
	if isTextContent(r.Header.Get("Content-Type")) && utf8.Valid(body) {
		ev.Body = string(body)
	} else if len(body) > 0 {
		ev.Body = base64.StdEncoding.EncodeToString(body)
		ev.IsBase64Encoded = true
	}
	return ev, nil
}

// isTextContent reports whether bodies of this Content-Type are passed as text.
// A missing Content-Type counts as text.
func isTextContent(contentType string) bool {
	if contentType == "" {
		return true
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	switch {
	case strings.HasPrefix(mt, "text/"),
		mt == "application/json", mt == "application/xml",
		mt == "application/x-www-form-urlencoded",
		strings.HasSuffix(mt, "+json"), strings.HasSuffix(mt, "+xml"):
		return true
	}
	return false
}

// WriteResponse writes an invocation Result as an HTTP response.
//
// A 200 whose payload is a JSON object with "statusCode" is decoded as an
// APIGatewayV2HTTPResponse. Any other 200 payload is a v2 "simple response"
// and is written verbatim as application/json.
//
// Failures never expose the Lambda error document to the client, as API
// Gateway does (ruling R17): a handler error (500) answers
// {"message":"Internal Server Error"}, a crash or invalid response (502)
// {"message":"Bad Gateway"} and a timeout (504) {"message":"Gateway
// Timeout"}. The error document is logged at warn with the request id.
func WriteResponse(w http.ResponseWriter, res Result) {
	if res.Status != http.StatusOK {
		logFailure(res.RequestID, res.Status, res.Payload)
		writeGeneric(w, res.Status)
		return
	}
	var probe struct {
		StatusCode *int `json:"statusCode"`
	}
	if json.Unmarshal(res.Payload, &probe) == nil && probe.StatusCode != nil {
		var resp events.APIGatewayV2HTTPResponse
		if err := json.Unmarshal(res.Payload, &resp); err == nil {
			writeStructured(w, res.RequestID, resp)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Payload)
}

func logFailure(requestID string, status int, errorDoc []byte) {
	slog.Default().Warn("invocation failed", "component", "funcrt", "requestId", requestID,
		"status", status, "error", string(errorDoc))
}

// writeGeneric writes API Gateway's generic error body for status.
func writeGeneric(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(struct {
		Message string `json:"message"`
	}{http.StatusText(status)})
	_, _ = w.Write(b)
}

func writeStructured(w http.ResponseWriter, requestID string, resp events.APIGatewayV2HTTPResponse) {
	body := []byte(resp.Body)
	if resp.IsBase64Encoded {
		dec, err := base64.StdEncoding.DecodeString(resp.Body)
		if err != nil {
			logFailure(requestID, http.StatusBadGateway, errorPayload("Function.InvalidResponse", "isBase64Encoded set but body is not valid base64"))
			writeGeneric(w, http.StatusBadGateway)
			return
		}
		body = dec
	}
	h := w.Header()
	for k, v := range resp.Headers {
		h.Set(k, v)
	}
	for k, vs := range resp.MultiValueHeaders {
		h.Del(k)
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", "application/json") // API Gateway's default
	}
	for _, c := range resp.Cookies {
		h.Add("Set-Cookie", c)
	}
	if resp.StatusCode < 100 || resp.StatusCode > 999 {
		logFailure(requestID, http.StatusBadGateway, errorPayload("Function.InvalidResponse", "invalid statusCode"))
		for k := range h {
			delete(h, k) // drop the function's headers from the generic error
		}
		writeGeneric(w, http.StatusBadGateway)
		return
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}
