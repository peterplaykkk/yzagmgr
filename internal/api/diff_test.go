package api

import (
	"strings"
	"testing"
)

func TestMaskContentPreservesFollowingSettingLines(t *testing.T) {
	const secret = "synthetic-gemini-key-91ac"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "dotenv LF keeps model and base URL after API key",
			in:   "GEMINI_API_KEY=" + secret + "\nGEMINI_MODEL=deepseek-flash\nGOOGLE_GEMINI_BASE_URL=https://gateway.example/v1beta\n",
			want: "GEMINI_API_KEY=" + maskedValueContent + "\nGEMINI_MODEL=deepseek-flash\nGOOGLE_GEMINI_BASE_URL=https://gateway.example/v1beta\n",
		},
		{
			name: "dotenv CRLF keeps model and base URL after API key",
			in:   "GEMINI_API_KEY=" + secret + "\r\nGEMINI_MODEL=deepseek-flash\r\nGOOGLE_GEMINI_BASE_URL=https://gateway.example/v1beta\r\n",
			want: "GEMINI_API_KEY=" + maskedValueContent + "\r\nGEMINI_MODEL=deepseek-flash\r\nGOOGLE_GEMINI_BASE_URL=https://gateway.example/v1beta\r\n",
		},
		{
			name: "quoted dotenv value keeps quote style and following lines",
			in:   "GEMINI_API_KEY=\"" + secret + "\"\r\nGEMINI_MODEL=deepseek-flash\r\nGOOGLE_GEMINI_BASE_URL=https://gateway.example/v1beta\r\n",
			want: "GEMINI_API_KEY=\"" + maskedValueContent + "\"\r\nGEMINI_MODEL=deepseek-flash\r\nGOOGLE_GEMINI_BASE_URL=https://gateway.example/v1beta\r\n",
		},
		{
			name: "flow JSON masks key but preserves sibling fields",
			in:   `{"env":{"apiKey":"` + secret + `","model":"deepseek-flash","baseUrl":"https://gateway.example/v1beta"}}`,
			want: `{"env":{"apiKey":"` + maskedValueContent + `","model":"deepseek-flash","baseUrl":"https://gateway.example/v1beta"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := maskContent(tc.in)
			if strings.Contains(got, secret) {
				t.Fatal("content mask leaked the synthetic API key")
			}
			if got != tc.want {
				t.Fatalf("maskContent changed bytes outside the secret value:\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}
