package gsync

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"golang.org/x/crypto/ssh"
)

// TestSSHAuth_FallsBackToOpenSSHDefaultsForUnknownHost verifies the
// no-promotion path: when the dialed host has no known_hosts entries, the
// HostKeyAlgorithms slice is exactly the OpenSSH 9.x default order (cert
// variants first, then plain keys; Ed25519 first within each group;
// deprecated ssh-rsa / ssh-dss absent).
func TestSSHAuth_FallsBackToOpenSSHDefaultsForUnknownHost(t *testing.T) {
	keyPath := writeTempEd25519Key(t)

	auth, err := sshAuth("nonexistent.example.invalid:22", keyPath, "")
	if err != nil {
		t.Fatalf("sshAuth: %v", err)
	}

	want := []string{
		ssh.CertAlgoED25519v01,
		ssh.CertAlgoECDSA256v01,
		ssh.CertAlgoECDSA384v01,
		ssh.CertAlgoECDSA521v01,
		ssh.CertAlgoSKED25519v01,
		ssh.CertAlgoSKECDSA256v01,
		ssh.CertAlgoRSASHA512v01,
		ssh.CertAlgoRSASHA256v01,
		ssh.KeyAlgoED25519,
		ssh.KeyAlgoECDSA256,
		ssh.KeyAlgoECDSA384,
		ssh.KeyAlgoECDSA521,
		ssh.KeyAlgoSKED25519,
		ssh.KeyAlgoSKECDSA256,
		ssh.KeyAlgoRSASHA512,
		ssh.KeyAlgoRSASHA256,
	}
	if !slices.Equal(auth.HostKeyAlgorithms, want) {
		t.Errorf("HostKeyAlgorithms mismatch.\n got:  %v\n want: %v", auth.HostKeyAlgorithms, want)
	}
	if slices.Contains(auth.HostKeyAlgorithms, ssh.KeyAlgoRSA) {
		t.Errorf("HostKeyAlgorithms contains deprecated %q; OpenSSH excludes it from defaults", ssh.KeyAlgoRSA)
	}
}

// TestSSHAuth_PromotesEntriesForKnownHost asserts the dynamic-promotion
// contract for the bracketed / non-default-port path: for a host we have
// known_hosts entries for at a non-22 port (stored as [host]:port in
// known_hosts, e.g. [ssh.github.com]:443), the library must surface those
// algorithms first so mergeAlgorithms promotes them ahead of the static
// OpenSSH fallback. If this stops working, negotiation would fall back to
// the static order and would not target the key types we actually have
// entries for.
func TestSSHAuth_PromotesEntriesForKnownHost(t *testing.T) {
	keyPath := writeTempEd25519Key(t)

	auth, err := sshAuth("ssh.github.com:443", keyPath, "")
	if err != nil {
		t.Fatalf("sshAuth: %v", err)
	}
	if len(auth.HostKeyAlgorithms) == 0 {
		t.Fatal("expected non-empty HostKeyAlgorithms")
	}
	// [ssh.github.com]:443 has ed25519, ecdsa, rsa entries in defaultKnownHosts.
	// One of those algorithms must be at position 0 — if the bracketed lookup
	// had failed, position 0 would be a cert variant from the static OpenSSH
	// fallback instead.
	wantFamilies := []string{
		ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256,
		ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA,
	}
	if !slices.Contains(wantFamilies, auth.HostKeyAlgorithms[0]) {
		t.Errorf("bracketed-port lookup didn't promote any per-host algorithm; got %q first (full list: %v)",
			auth.HostKeyAlgorithms[0], auth.HostKeyAlgorithms)
	}
	// The rest of the OpenSSH defaults must still be present as fallback.
	for _, must := range []string{ssh.KeyAlgoECDSA256, ssh.KeyAlgoRSASHA512} {
		if !slices.Contains(auth.HostKeyAlgorithms, must) {
			t.Errorf("expected fallback algorithm %q to remain in list, got %v", must, auth.HostKeyAlgorithms)
		}
	}
}

// TestSSHAuth_PromotesUnbracketedDefaultPortEntry guards the port-22
// normalization: known_hosts stores port-22 hosts unbracketed (`github.com
// ssh-ed25519 ...`) but our sshHostPort always returns "host:port"
// ("github.com:22"). The library must treat the two equivalently — this test
// fails if that ever stops being true.
func TestSSHAuth_PromotesUnbracketedDefaultPortEntry(t *testing.T) {
	keyPath := writeTempEd25519Key(t)
	auth, err := sshAuth("github.com:22", keyPath, "")
	if err != nil {
		t.Fatalf("sshAuth: %v", err)
	}
	// github.com has ed25519, ecdsa, rsa entries in defaultKnownHosts (all in
	// the unbracketed/port-22 form). One of those algorithms must be at
	// position 0 — if the lookup had failed, position 0 would be a cert
	// variant from the static OpenSSH fallback instead.
	wantFamilies := []string{
		ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256,
		ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA,
	}
	if !slices.Contains(wantFamilies, auth.HostKeyAlgorithms[0]) {
		t.Errorf("port-22 lookup didn't promote any per-host algorithm; got %q first (full list: %v)",
			auth.HostKeyAlgorithms[0], auth.HostKeyAlgorithms)
	}
}

// TestSSHAuth_PromotesCustomerExtraEntry is the "future customer" case the
// dynamic promotion exists to handle: a tenant drops only an RSA entry for
// their server. Without promotion, our static defaults would still negotiate
// Ed25519 first and we'd fail against an RSA-only known_hosts entry. With
// promotion, ssh-rsa is surfaced at the top of HostKeyAlgorithms for that
// host so negotiation lands on RSA — which is what we have an entry for.
func TestSSHAuth_PromotesCustomerExtraEntry(t *testing.T) {
	keyPath := writeTempEd25519Key(t)

	// Real-shaped RSA host-key bytes (re-using github.com's published
	// ssh-rsa entry from defaultKnownHosts — semantically meaningless for
	// the test, but keeps the parser happy).
	extra := "[customer.example.invalid]:22 ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCj7ndNxQowgcQnjshcLrqPEiiphnt+VTTvDP6mHBL9j1aNUkY4Ue1gvwnGLVlOhGeYrnZaMgRK6+PKCUXaDbC7qtbW8gIkhL7aGCsOr/C56SJMy/BCZfxd1nWzAOxSDPgVsmerOBYfNqltV9/hWCqBywINIR+5dIg6JTJ72pcEpEjcYgXkE2YEFXV1JHnsKgbLWNlhScqb2UmyRkQyytRLtL+38TGxkxCflmO+5Z8CSSNY7GidjMIZ7Q4zMjA2n1nGrlTDkzwDCsw+wqFPGQA179cnfGWOWRVruj16z6XyvxvjJwbz0wQZ75XK5tKSb7FNyeIEs4TT4jk+S4dhPeAUC5y+bDYirYgM4GC7uEnztnZyaVWQ7B381AK4Qdrwt51ZqExKbQpTUNn+EjqoTwvqNj4kqx5QUCI0ThS/YkOxJCXmPUWZbhjpCg56i+2aB6CmK2JGhn57K5mj0MNdBXA4/WnwH6XoPWJzK5Nyu2zB3nAZp+S5hpQs+p1vN1/wsjk="

	auth, err := sshAuth("customer.example.invalid:22", keyPath, extra)
	if err != nil {
		t.Fatalf("sshAuth: %v", err)
	}
	// The first entry should be an RSA-family algorithm, not Ed25519/ECDSA,
	// because that's what the customer's extra entry has.
	first := auth.HostKeyAlgorithms[0]
	rsaFamily := []string{
		ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA,
		ssh.CertAlgoRSASHA512v01, ssh.CertAlgoRSASHA256v01, ssh.CertAlgoRSAv01,
	}
	if !slices.Contains(rsaFamily, first) {
		t.Errorf("expected an RSA-family algorithm promoted to front for customer.example.invalid, got %q (full list: %v)",
			first, auth.HostKeyAlgorithms)
	}
}

func TestSSHHostPort(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"ssh://git@git.example.com:2223/foo/bar.git", "git.example.com:2223"},
		{"ssh://git@github.com/foo/bar.git", "github.com:22"},
		{"git@github.com:foo/bar.git", "github.com:22"},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			got, err := sshHostPort(tc.url)
			if err != nil {
				t.Fatalf("sshHostPort(%q): %v", tc.url, err)
			}
			if got != tc.want {
				t.Errorf("sshHostPort(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

func TestMergeAlgorithms(t *testing.T) {
	cases := []struct {
		name      string
		hostAlgos []string
		fallback  []string
		want      []string
	}{
		{
			name:      "empty hostAlgos returns fallback unchanged",
			hostAlgos: nil,
			fallback:  []string{"a", "b", "c"},
			want:      []string{"a", "b", "c"},
		},
		{
			name:      "hostAlgos prepended and not duplicated",
			hostAlgos: []string{"b"},
			fallback:  []string{"a", "b", "c"},
			want:      []string{"b", "a", "c"},
		},
		{
			name:      "duplicates within hostAlgos are collapsed",
			hostAlgos: []string{"x", "x", "y"},
			fallback:  []string{"y", "z"},
			want:      []string{"x", "y", "z"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeAlgorithms(tc.hostAlgos, tc.fallback)
			if !slices.Equal(got, tc.want) {
				t.Errorf("mergeAlgorithms(%v, %v) = %v, want %v", tc.hostAlgos, tc.fallback, got, tc.want)
			}
		})
	}
}

// writeTempEd25519Key writes a freshly-generated Ed25519 private key in PKCS#8
// PEM form to a temp file and returns the path. Used to satisfy sshAuth's
// NewPublicKeysFromFile, which actually parses the PEM.
func writeTempEd25519Key(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}
