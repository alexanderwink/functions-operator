// Command echo is a Lambda test handler for funcrt. Its behaviour is driven by
// the invocation payload:
//
//	{"echo":X}       returns X
//	{"sleep":"2s"}   sleeps for the duration, then returns null
//	{"error":"boom"} returns errors.New("boom")
//	{"exit":1}       calls os.Exit(1)
//	{"rawPath":"/p",...} (an API Gateway v2 event) returns a response whose
//	                 body is the rawPath; rawPath "/sleep/<duration>" sleeps first,
//	                 rawPath "/error/<msg>" returns errors.New(msg)
//
// With ECHO_INIT_FAIL=1 in the environment it exits with code 2 before calling
// lambda.Start.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
)

type request struct {
	Echo  json.RawMessage `json:"echo,omitempty"`
	Sleep string          `json:"sleep,omitempty"`
	Error string          `json:"error,omitempty"`
	Exit  *int            `json:"exit,omitempty"`

	RawPath *string `json:"rawPath,omitempty"`
}

func handle(_ context.Context, in json.RawMessage) (json.RawMessage, error) {
	var req request
	if err := json.Unmarshal(in, &req); err != nil {
		return nil, err
	}
	if req.RawPath != nil {
		if msg, ok := strings.CutPrefix(*req.RawPath, "/error/"); ok {
			return nil, errors.New(msg)
		}
		if d, err := time.ParseDuration(strings.TrimPrefix(*req.RawPath, "/sleep/")); err == nil {
			time.Sleep(d)
		}
		return json.Marshal(events.APIGatewayV2HTTPResponse{StatusCode: 200, Body: *req.RawPath})
	}
	if req.Exit != nil {
		os.Exit(*req.Exit)
	}
	if req.Error != "" {
		return nil, errors.New(req.Error)
	}
	if req.Sleep != "" {
		d, err := time.ParseDuration(req.Sleep)
		if err != nil {
			return nil, err
		}
		// Deliberately ignores ctx: funcrt, not the handler, enforces timeouts.
		time.Sleep(d)
	}
	if req.Echo != nil {
		return req.Echo, nil
	}
	return json.RawMessage("null"), nil
}

func main() {
	if os.Getenv("ECHO_INIT_FAIL") == "1" {
		os.Exit(2)
	}
	lambda.Start(handle)
}
