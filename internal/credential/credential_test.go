package credential

import "testing"

func TestFingerprintIsStableAndDoesNotExposeCredential(t *testing.T) {
	const token = "github_pat_secret"
	first := Fingerprint(token)
	if first != Fingerprint("  "+token+"\r\n") {
		t.Fatal("fingerprint should ignore surrounding console whitespace")
	}
	if first == token || len(first) != 64 {
		t.Fatalf("unsafe fingerprint %q", first)
	}
}
