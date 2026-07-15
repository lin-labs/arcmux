// Package safetext owns fail-closed checks for bounded human-readable text
// that may cross an arcmux trust boundary.
package safetext

import "regexp"

var credentialPatterns = []*regexp.Regexp{
	// URI userinfo is credential-bearing even when it does not name the field.
	regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s/@]+@`),
	// PEM material is never safe, even when a model has shortened the body.
	regexp.MustCompile(`(?i)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`),
	// Authorization headers and prose containing actual auth schemes.
	regexp.MustCompile(`(?i)\b(?:authorization\s*[:=]\s*)?(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`),
	// JWTs are recognizable independent of the surrounding variable name.
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\b`),
	// Environment/config assignments, including provider-specific names such as
	// OPENAI_API_KEY, XAI_API_KEY, AWS_SECRET_ACCESS_KEY, and GCP_CREDENTIALS.
	regexp.MustCompile(`(?i)\b[A-Z0-9_]*(?:API[_-]?KEY|ACCESS[_-]?KEY|SECRET(?:[_-]?ACCESS[_-]?KEY)?|TOKEN|PASSWORD|PASSWD|PRIVATE[_-]?KEY|CREDENTIALS|AUTHORIZATION)[A-Z0-9_]*\s*[:=]\s*["']?[^\s,"';]{4,}`),
	// Human-readable config labels use spaces and hyphens as well as underscores.
	regexp.MustCompile(`(?i)\b(?:api[ _-]?key|access[ _-]?(?:key|token)|auth[ _-]?token|authorization|password|passwd|secret(?:[ _-]?access[ _-]?key)?|private[ _-]?key|credentials?)\s*[:=]\s*["']?[^\s,"';]{4,}`),
	// Common provider/token formats that may appear without an assignment.
	regexp.MustCompile(`(?i)\b(?:ghp[_-]|github_pat[_-]|xox[baprs]-|xai[_-]|sk[_-](?:proj[_-])?)[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`),
	regexp.MustCompile(`\bAIza[A-Za-z0-9_-]{20,}\b`),
}

// ContainsCredentialLike reports whether value resembles credential material.
// Callers must reject or omit the whole field; replacing only the matched text
// risks transmitting an unrecognized fragment of the same secret.
func ContainsCredentialLike(value string) bool {
	for _, pattern := range credentialPatterns {
		if pattern.MatchString(value) {
			return true
		}
	}
	return false
}
