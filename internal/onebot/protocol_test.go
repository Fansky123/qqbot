package onebot

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestProtocolDecodeGroupMessage(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"post_type":"message",
		"message_type":"group",
		"message_id":9988,
		"group_id":"123456",
		"user_id":654321,
		"self_id":"111222",
		"message":[
			{"type":"text","data":{"text":"before "}},
			{"type":"at","data":{"qq":111222}},
			{"type":"text","data":{"text":" [orders] fix"}},
			{"type":"at","data":{"qq":"999888"}},
			{"type":"image","data":{"file":"ignored.jpg"}},
			{"type":"text","data":{"text":" duplicate submit"}}
		]
	}`)

	message, accepted, err := DecodeGroupMessage(raw, "111222")
	if err != nil {
		t.Fatal(err)
	}
	if !accepted {
		t.Fatal("group message was not accepted")
	}
	if message.MessageID != "9988" || message.GroupID != "123456" || message.UserID != "654321" || message.SelfID != "111222" {
		t.Fatalf("unexpected normalized IDs: %#v", message)
	}
	if !message.Mentioned {
		t.Fatal("self mention was not detected")
	}
	if want := "before  [orders] fix duplicate submit"; message.Text != want {
		t.Fatalf("Text = %q, want %q", message.Text, want)
	}
}

func TestProtocolDoesNotTreatOtherMentionsAsSelf(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"post_type":"message",
		"message_type":"group",
		"message_id":"9988",
		"group_id":"123456",
		"user_id":"654321",
		"self_id":"111222",
		"message":[
			{"type":"at","data":{"qq":"333444"}},
			{"type":"at","data":{"qq":"all"}},
			{"type":"text","data":{"text":"status"}}
		]
	}`)

	message, accepted, err := DecodeGroupMessage(raw, "111222")
	if err != nil {
		t.Fatal(err)
	}
	if !accepted {
		t.Fatal("group message was not accepted")
	}
	if message.Mentioned {
		t.Fatal("another user or @all must not count as the configured self mention")
	}
	if message.Text != "status" {
		t.Fatalf("Text = %q, want status", message.Text)
	}
}

func TestProtocolIgnoresEventsForAnotherSelfID(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"post_type":"message",
		"message_type":"group",
		"message_id":"9988",
		"group_id":"123456",
		"user_id":"654321",
		"self_id":"999000",
		"message":[
			{"type":"at","data":{"qq":"111222"}},
			{"type":"text","data":{"text":"[orders] ignored"}}
		]
	}`)

	message, accepted, err := DecodeGroupMessage(raw, "111222")
	if err != nil {
		t.Fatal(err)
	}
	if accepted || message != (GroupMessage{}) {
		t.Fatalf("event for another self ID was accepted: %#v, %v", message, accepted)
	}
}

func TestProtocolIgnoresNonGroupEvents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
	}{
		{name: "private message", raw: `{"post_type":"message","message_type":"private","message_id":1}`},
		{name: "notice", raw: `{"post_type":"notice","message_type":"group"}`},
		{name: "missing post type", raw: `{"message_type":"group"}`},
		{name: "missing message type", raw: `{"post_type":"message"}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			message, accepted, err := DecodeGroupMessage([]byte(test.raw), "111222")
			if err != nil {
				t.Fatal(err)
			}
			if accepted || message != (GroupMessage{}) {
				t.Fatalf("unexpected accepted event: %#v, %v", message, accepted)
			}
		})
	}
}

func TestProtocolIDAcceptsOnlyUnsignedDecimalIntegers(t *testing.T) {
	t.Parallel()

	valid := []struct {
		raw  string
		want ID
	}{
		{raw: `0`, want: "0"},
		{raw: `18446744073709551615`, want: "18446744073709551615"},
		{raw: `"9988"`, want: "9988"},
	}
	for _, test := range valid {
		var got ID
		if err := json.Unmarshal([]byte(test.raw), &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", test.raw, err)
		}
		if got != test.want {
			t.Fatalf("Unmarshal(%s) = %q, want %q", test.raw, got, test.want)
		}
	}

	invalid := []string{
		`null`, `true`, `{}`, `[]`, `1.0`, `1e3`, `-1`, `+1`,
		`""`, `"-1"`, `"1.0"`, `"1e3"`, `"+1"`, `" 1"`, `"1 "`, `"abc"`,
		`18446744073709551616`, `"18446744073709551616"`,
	}
	for _, raw := range invalid {
		var got ID
		if err := json.Unmarshal([]byte(raw), &got); err == nil {
			t.Errorf("Unmarshal(%s) unexpectedly succeeded with %q", raw, got)
		}
	}
}

func TestProtocolRejectsMalformedTargetGroupEvents(t *testing.T) {
	t.Parallel()

	tests := []string{
		`{"post_type":"message","message_type":"group","group_id":2,"user_id":3,"self_id":4,"message":[]}`,
		`{"post_type":"message","message_type":"group","message_id":1,"user_id":3,"self_id":4,"message":[]}`,
		`{"post_type":"message","message_type":"group","message_id":1,"group_id":2,"self_id":4,"message":[]}`,
		`{"post_type":"message","message_type":"group","message_id":1,"group_id":2,"user_id":3,"message":[]}`,
		`{"post_type":"message","message_type":"group","message_id":1.5,"group_id":2,"user_id":3,"self_id":4,"message":[]}`,
		`{"post_type":"message","message_type":"group","message_id":1,"group_id":2,"user_id":3,"self_id":4}`,
		`{"post_type":"message","message_type":"group","message_id":1,"group_id":2,"user_id":3,"self_id":4,"message":null}`,
		`{"post_type":"message","message_type":"group","message_id":1,"group_id":2,"user_id":3,"self_id":4,"message":"text mode"}`,
		`{"post_type":"message","message_type":"group","message_id":1,"group_id":2,"user_id":3,"self_id":4,"message":[{"type":"text","data":{}}]}`,
		`{"post_type":"message","message_type":"group","message_id":1,"group_id":2,"user_id":3,"self_id":4,"message":[{"type":"text","data":{"text":null}}]}`,
		`{"post_type":"message","message_type":"group","message_id":1,"group_id":2,"user_id":3,"self_id":4,"message":[{"type":"text","data":{"text":true}}]}`,
		`{"post_type":"message","message_type":"group","message_id":1,"group_id":2,"user_id":3,"self_id":4,"message":[{"type":"text","data":{"text":42}}]}`,
		`{"post_type":"message","message_type":"group","message_id":1,"group_id":2,"user_id":3,"self_id":4,"message":[{"type":"text","data":{"text":{}}}]}`,
	}

	for _, raw := range tests {
		if _, accepted, err := DecodeGroupMessage([]byte(raw), "4"); err == nil || accepted {
			t.Errorf("DecodeGroupMessage(%s) = accepted %v, error %v", raw, accepted, err)
		}
	}
	if _, accepted, err := DecodeGroupMessage([]byte(`{`), "4"); err == nil || accepted {
		t.Fatalf("invalid JSON = accepted %v, error %v", accepted, err)
	}
}

func TestProtocolActionRequestAndResponse(t *testing.T) {
	t.Parallel()

	action, err := SendGroupAction("123456", `a&b[c],d`, "echo-1")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(action)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"action": "send_group_msg",
		"echo":   "echo-1",
		"params": map[string]any{
			"group_id": "123456",
			"message": []any{
				map[string]any{
					"type": "text",
					// Array-format text is already structured data. Escaping it here
					// would make CQ entities visible in the delivered message.
					"data": map[string]any{"text": `a&b[c],d`},
				},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("action JSON = %#v, want %#v", got, want)
	}
	if _, err := SendGroupAction("not-an-id", "text", "echo-2"); err == nil {
		t.Fatal("invalid group ID was accepted")
	}

	responseRaw := []byte(`{"status":"failed","retcode":1404,"data":{"message_id":9988},"message":"bad request","wording":"invalid params","echo":"echo-1"}`)
	var response ActionResponse
	if err := json.Unmarshal(responseRaw, &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "failed" || response.RetCode != 1404 || response.Message != "bad request" || response.Wording != "invalid params" || response.Echo != "echo-1" {
		t.Fatalf("unexpected action response: %#v", response)
	}
	var data map[string]any
	if err := json.Unmarshal(response.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["message_id"] != float64(9988) {
		t.Fatalf("response data was not preserved: %#v", data)
	}
}

func TestProtocolEscapeCQText(t *testing.T) {
	t.Parallel()

	if got, want := EscapeCQText(`&amp;[,],`), `&amp;amp;&#91;&#44;&#93;&#44;`; got != want {
		t.Fatalf("EscapeCQText() = %q, want %q", got, want)
	}
}

func TestProtocolSplitMessage(t *testing.T) {
	t.Parallel()

	text := strings.Repeat("界", 1199) + "🙂" + strings.Repeat("文", 1201)
	parts := SplitMessage(text, 1200)
	if len(parts) != 3 {
		t.Fatalf("len(parts) = %d, want 3", len(parts))
	}
	for i, part := range parts {
		if !utf8.ValidString(part) {
			t.Fatalf("part %d is not valid UTF-8", i)
		}
		if count := utf8.RuneCountInString(part); count > 1200 {
			t.Fatalf("part %d contains %d runes", i, count)
		}
	}
	if got := strings.Join(parts, ""); got != text {
		t.Fatal("split parts did not reconstruct the input")
	}
	if got := SplitMessage("text", 0); got != nil {
		t.Fatalf("non-positive maximum returned %#v, want nil", got)
	}
	if got := SplitMessage("", 1200); !reflect.DeepEqual(got, []string{""}) {
		t.Fatalf("empty message returned %#v", got)
	}
}
