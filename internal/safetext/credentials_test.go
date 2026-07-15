package safetext

import "testing"

func TestContainsCredentialLike(t *testing.T) {
	t.Parallel()
	unsafe := []string{
		"postgres://user:password@host/db",
		"https://user:pass@example.com/path",
		"OPENAI_API_KEY=sk-proj-abcdefghijklmnop",
		"AWS_SECRET_ACCESS_KEY: abcdefghijklmnop",
		"GCP_CREDENTIALS=/tmp/service-account.json",
		"xai_api_key=xai_abcdefghijklmnop",
		"api key = sk-proj-abcdefghijklmnop",
		"access token: abcdefghijklmnop",
		"Authorization: Bearer abcdefghijklmnop",
		"Authorization: Basic dXNlcjpwYXNzd29yZA==",
		"token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijk",
		"-----BEGIN RSA PRIVATE KEY-----",
		"AKIAABCDEFGHIJKLMNOP",
		"AIzaSyD-abcdefghijklmnopqrstuvwx",
	}
	for _, value := range unsafe {
		value := value
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			if !ContainsCredentialLike(value) {
				t.Fatalf("credential-like value was accepted: %q", value)
			}
		})
	}

	safe := []string{
		"Add OpenAI API support and keep the key in Secret Manager",
		"Rotate AWS credentials before release",
		"Investigate token accounting in the mesh",
		"Basic remote attach over Tailscale",
	}
	for _, value := range safe {
		if ContainsCredentialLike(value) {
			t.Fatalf("safe summary was rejected: %q", value)
		}
	}
}
