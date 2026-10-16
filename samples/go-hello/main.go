// go-hello is a sample Lambda function (provided.al2023 style bootstrap) used to exercise the operator.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
)

// build is stamped at link time: -ldflags "-X main.build=<id>".
var build = "dev"

type probe struct {
	DetailType string `json:"detail-type"`
	Source     string `json:"source"`
}

func handler(_ context.Context, in json.RawMessage) (events.APIGatewayV2HTTPResponse, error) {
	var p probe
	_ = json.Unmarshal(in, &p)
	if p.DetailType != "" || p.Source == "aws.events" {
		fmt.Println("scheduled tick")
		return ok(map[string]any{"lang": "go", "scheduled": true, "build": build})
	}

	var req events.APIGatewayV2HTTPRequest
	if err := json.Unmarshal(in, &req); err != nil {
		return events.APIGatewayV2HTTPResponse{StatusCode: 400, Body: err.Error()}, nil
	}
	if s := req.QueryStringParameters["sleep"]; s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return events.APIGatewayV2HTTPResponse{StatusCode: 400, Body: "bad sleep: " + err.Error()}, nil
		}
		time.Sleep(d)
	}
	return ok(map[string]any{
		"lang":    "go",
		"path":    req.RawPath,
		"version": os.Getenv("AWS_LAMBDA_FUNCTION_VERSION"),
		"build":   build,
	})
}

func ok(v any) (events.APIGatewayV2HTTPResponse, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return events.APIGatewayV2HTTPResponse{}, err
	}
	return events.APIGatewayV2HTTPResponse{
		StatusCode: 200,
		Headers:    map[string]string{"content-type": "application/json"},
		Body:       string(b),
	}, nil
}

func main() { lambda.Start(handler) }
