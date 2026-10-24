package main

import "testing"

func TestDirectProviderCallsInAllowedCorePackageFail(t *testing.T) {
	for _, source := range []string{
		`package processors; import "net/http"; func run() { http.Post(endpoint, "application/json", body) }`,
		`package processors; func run() { payload := map[string]any{"model": m, "messages": msgs}; _ = payload }`,
		`package processors; func run() { s.transport.client.Do(request) }`,
		"package processors; const endpoint = `http://local/v1/chat/completions`",
		`package processors; func calibrateBuiltinRegex() {}`,
		"package processors; const cfg = `json:\"system_prompt\"`",
	} {
		violations, err := sourceViolations("internal/processors/builtin.go", source)
		if err != nil || len(violations) == 0 {
			t.Fatalf("direct inference escaped boundary: %s; violations=%v err=%v", source, violations, err)
		}
	}
}

func TestProviderCallsInTransportAlsoFail(t *testing.T) {
	violations, err := sourceViolations("internal/processors/transport.go", `package processors; const route = "/v1/chat/completions"`)
	if err != nil || len(violations) == 0 {
		t.Fatalf("inference hidden in permitted transport: %v %v", violations, err)
	}
}

func TestGenericProcessorTransportPasses(t *testing.T) {
	violations, err := sourceViolations("internal/processors/transport.go", `package processors; import "net/http"; func send() { http.NewRequest("POST", endpoint+"/v1/process", body) }; const feedback = "/v1/feedback"`)
	if err != nil || len(violations) != 0 {
		t.Fatalf("generic contract transport rejected: %v %v", violations, err)
	}
}
