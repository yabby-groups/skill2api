package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestMarshalJobResponseAlwaysReturnsJSONOnEncodingFailure(t *testing.T) {
	for name, payload := range map[string]any{
		"unsupported value": map[string]any{"secret": "private-fixture", "value": make(chan int)},
		"invalid number":    math.NaN(),
		"cycle": func() any {
			value := map[string]any{}
			value["self"] = value
			return value
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			data := marshalJobResponse("request-\"fixture", payload)
			var response map[string]string
			if err := json.Unmarshal(data, &response); err != nil {
				t.Fatalf("non-JSON failure reply: %q, %v", data, err)
			}
			if response["request_id"] != "request-\"fixture" || response["status"] != "failed" || response["error"] == "" {
				t.Fatalf("incomplete failure reply: %s", data)
			}
			if strings.Contains(string(data), "private-fixture") {
				t.Fatal("failed payload leaked into reply")
			}
		})
	}
}
