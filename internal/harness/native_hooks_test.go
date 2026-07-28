package harness

import "testing"

func TestParseNativeHookSignalMappings(t *testing.T) {
	tests := []struct {
		name    string
		harness string
		payload string
		want    string
	}{
		{name: "session start", harness: Harness, payload: `{"hook_event_name":"SessionStart"}`, want: StateWorking},
		{name: "prompt", harness: Harness, payload: `{"hook_event_name":"UserPromptSubmit"}`, want: StateWorking},
		{name: "pre tool", harness: Harness, payload: `{"hook_event_name":"PreToolUse"}`, want: StateWorking},
		{name: "stop", harness: Harness, payload: `{"hook_event_name":"Stop"}`, want: StateWaiting},
		{name: "post tool", harness: Harness, payload: `{"hook_event_name":"PostToolUse"}`, want: SignalActivity},
		{name: "pre compact", harness: Harness, payload: `{"hook_event_name":"PreCompact"}`, want: SignalActivity},
		{name: "post compact", harness: Harness, payload: `{"hook_event_name":"PostCompact"}`, want: SignalActivity},
		{name: "unknown event", harness: Harness, payload: `{"hook_event_name":"UnknownEvent"}`, want: SignalActivity},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signal, err := ParseNativeHook(NativeHookInput{
				Harness: tt.harness,
				RawJSON: []byte(tt.payload),
			})
			if err != nil {
				t.Fatalf("ParseNativeHook err = %v", err)
			}
			if signal.Signal != tt.want {
				t.Fatalf("Signal = %q, want %q", signal.Signal, tt.want)
			}
			if signal.Harness != tt.harness {
				t.Fatalf("Harness = %q, want %q", signal.Harness, tt.harness)
			}
			if signal.HookEventName == "" {
				t.Fatalf("HookEventName = empty")
			}
		})
	}
}

func TestParseNativeHookEventNameFallbacks(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{name: "camel", payload: `{"hookEventName":"Stop"}`, want: "Stop"},
		{name: "hook name", payload: `{"hookName":"PreToolUse"}`, want: "PreToolUse"},
		{name: "type fallback", payload: `{"type":"PostToolUse"}`, want: "PostToolUse"},
		{name: "explicit event", payload: `{`, want: "Stop"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := NativeHookInput{
				Harness:       Harness,
				RawJSON:       []byte(tt.payload),
				ExplicitEvent: tt.want,
			}
			if tt.name != "explicit event" {
				input.ExplicitEvent = ""
			}
			signal, err := ParseNativeHook(input)
			if err != nil {
				t.Fatalf("ParseNativeHook err = %v", err)
			}
			if signal.HookEventName != tt.want {
				t.Fatalf("HookEventName = %q, want %q", signal.HookEventName, tt.want)
			}
		})
	}
}

func TestParseNativeHookRejectsInvalidPayloadWithoutFallback(t *testing.T) {
	if _, err := ParseNativeHook(NativeHookInput{Harness: Harness, RawJSON: []byte(`{`)}); err == nil {
		t.Fatal("ParseNativeHook invalid JSON err = nil, want error")
	}
}

func TestParseNativeHookRejectsUnsupportedHarness(t *testing.T) {
	if _, err := ParseNativeHook(NativeHookInput{Harness: "bogus", RawJSON: []byte(`{"hook_event_name":"Stop"}`)}); err == nil {
		t.Fatal("ParseNativeHook unsupported harness err = nil, want error")
	}
}
