package bot

import (
	"fmt"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestAPIEndpointFormat(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", tgbotapi.APIEndpoint, false},
		{"   ", tgbotapi.APIEndpoint, false},
		{"http://localhost:8081", "http://localhost:8081/bot%s/%s", false},
		{"http://telegram-bot-api:8081/", "http://telegram-bot-api:8081/bot%s/%s", false},
		{"https://bot-api.example.com", "https://bot-api.example.com/bot%s/%s", false},
		{"localhost:8081", "", true},
		{"ftp://host", "", true},
		{"http://", "", true},
	}
	for _, tc := range cases {
		got, err := apiEndpointFormat(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("apiEndpointFormat(%q) should fail, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("apiEndpointFormat(%q) failed: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("apiEndpointFormat(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAPIEndpointFormatBuildsRealURLs(t *testing.T) {
	format, err := apiEndpointFormat("http://localhost:8081")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprintf(format, "TOKEN", "getMe"), "http://localhost:8081/botTOKEN/getMe"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDefaultMaxFileSizeMB(t *testing.T) {
	if got := defaultMaxFileSizeMB(""); got != 50 {
		t.Errorf("public Bot API default = %d, want 50", got)
	}
	if got := defaultMaxFileSizeMB("  "); got != 50 {
		t.Errorf("blank endpoint default = %d, want 50", got)
	}
	if got := defaultMaxFileSizeMB("http://localhost:8081"); got != 2000 {
		t.Errorf("local Bot API default = %d, want 2000", got)
	}
}

// Everything the bot sends must go to the configured endpoint, not to api.telegram.org.
func TestBotTalksToConfiguredEndpoint(t *testing.T) {
	f := newFakeTG(t)
	b := newTestBot(t, f)

	if _, err := b.send(42, "hello"); err != nil {
		t.Fatal(err)
	}
	calls := f.callsTo("sendMessage")
	if len(calls) != 1 {
		t.Fatalf("got %d sendMessage calls on the fake server, want 1", len(calls))
	}
	if calls[0].Form.Get("chat_id") != "42" || calls[0].Form.Get("text") != "hello" {
		t.Fatalf("unexpected parameters: %v", calls[0].Form)
	}
}
