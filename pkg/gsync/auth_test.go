package gsync

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/obmondo/gfetch/pkg/config"
	"github.com/obmondo/gfetch/pkg/secrets"
	"github.com/obmondo/gfetch/pkg/telemetry"
)

const (
	testAuthRepoName   = "test-repo"
	testAuthRepoURL    = "git@github.com:test/repo.git"
	testOpenBaoAddr    = "http://openbao:8200"
	testOpenBaoRole    = "gfetch"
	testOpenBaoSAToken = "/var/run/secrets/token"
	testOpenBaoField   = "private_key"
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

// generateEd25519PEM returns a freshly-generated Ed25519 private key in PKCS#8
// PEM form as raw bytes.
func generateEd25519PEM(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
}

// fakeSecretReader is an inline test fake for the secretReader interface.
// With errAfter == 0 every read returns (data, err); with errAfter > 0 the
// first errAfter reads return data and later reads return err.
type fakeSecretReader struct {
	data     []byte
	err      error
	errAfter int

	mu            sync.Mutex
	reads         int
	invalidated   int
	invalidateAll int
}

func (f *fakeSecretReader) Read(_ context.Context, _ string, _ *secrets.OpenBaoConfig, _ string) ([]byte, error) {
	f.mu.Lock()
	f.reads++
	reads := f.reads
	f.mu.Unlock()
	if f.errAfter > 0 {
		if reads > f.errAfter {
			return nil, f.err
		}
		return f.data, nil
	}
	return f.data, f.err
}

func (f *fakeSecretReader) Invalidate(_ *secrets.OpenBaoConfig, _ string) {
	f.mu.Lock()
	f.invalidated++
	f.mu.Unlock()
}

func (f *fakeSecretReader) InvalidateAll() {
	f.mu.Lock()
	f.invalidateAll++
	f.mu.Unlock()
}

// TestSSHAuthFromPEM_SameAlgorithmsAsFilePath verifies that a PEM-based auth
// produces the same HostKeyAlgorithms as a file-based auth for the same key
// and host.
func TestSSHAuthFromPEM_SameAlgorithmsAsFilePath(t *testing.T) {
	keyPath := writeTempEd25519Key(t)
	pemBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	hostPort := "nonexistent.example.invalid:22"

	fileAuth, err := sshAuth(hostPort, keyPath, "")
	if err != nil {
		t.Fatalf("sshAuth: %v", err)
	}

	pemAuth, err := sshAuthFromPEM(hostPort, "test key", pemBytes, "")
	if err != nil {
		t.Fatalf("sshAuthFromPEM: %v", err)
	}

	if !slices.Equal(fileAuth.HostKeyAlgorithms, pemAuth.HostKeyAlgorithms) {
		t.Errorf("HostKeyAlgorithms mismatch.\n file: %v\n  pem: %v",
			fileAuth.HostKeyAlgorithms, pemAuth.HostKeyAlgorithms)
	}
}

// TestSSHAuthFromPEM_InvalidPEM_NoKeyInError verifies that a parse error for
// invalid PEM does not leak key bytes, and that the error names the source.
func TestSSHAuthFromPEM_InvalidPEM_NoKeyInError(t *testing.T) {
	badPEM := []byte("not-a-real-key-SUPERSECRET")

	// PEM path: source label must appear, key bytes must not.
	_, err := sshAuthFromPEM("host:22", "secret ssh-keys/acme/kubeaid", badPEM, "")
	if err == nil {
		t.Fatal("expected error for invalid PEM")
	}
	if strings.Contains(err.Error(), "SUPERSECRET") {
		t.Errorf("error message contains key bytes: %v", err)
	}
	if !strings.Contains(err.Error(), "secret ssh-keys/acme/kubeaid") {
		t.Errorf("error message does not contain the source label: %v", err)
	}

	// File path: write the same garbage to a temp file and go through sshAuth.
	tmpDir := t.TempDir()
	badKeyFile := filepath.Join(tmpDir, "bad_key")
	if werr := os.WriteFile(badKeyFile, badPEM, 0o600); werr != nil {
		t.Fatalf("write temp key file: %v", werr)
	}
	_, err = sshAuth("host:22", badKeyFile, "")
	if err == nil {
		t.Fatal("expected error for invalid PEM via sshAuth")
	}
	if strings.Contains(err.Error(), "SUPERSECRET") {
		t.Errorf("sshAuth error message contains key bytes: %v", err)
	}
	if !strings.Contains(err.Error(), badKeyFile) {
		t.Errorf("sshAuth error message does not contain the file path: %v", err)
	}
}

// TestResolveAuth_SecretReader verifies that when repo.OpenBao is set, the
// Syncer uses s.secrets.Read to get the key PEM, and that reader errors
// propagate without leaking secret material.
func TestResolveAuth_SecretReader(t *testing.T) {
	pemBytes := generateEd25519PEM(t)

	tests := []struct {
		name     string
		reader   *fakeSecretReader
		wantCode FailureCode
		wantErr  []error
	}{
		{
			name:   "success",
			reader: &fakeSecretReader{data: pemBytes},
		},
		{
			name:     "reader error propagates",
			reader:   &fakeSecretReader{err: fmt.Errorf("backend unavailable")},
			wantCode: CodeSecretUnavailable,
		},
		{
			name:     "invalid pem",
			reader:   &fakeSecretReader{data: []byte("not-a-real-key")},
			wantCode: CodeSecretInvalid,
			wantErr:  []error{secrets.ErrInvalidSecret},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(WithSecretReader(tt.reader))

			repo := &config.RepoConfig{
				SSHKeyPath: "ssh-keys/acme/kubeaid",
				Name:       testAuthRepoName,
				URL:        testAuthRepoURL,
				OpenBao: &secrets.OpenBaoConfig{
					Addr:                    testOpenBaoAddr,
					Role:                    testOpenBaoRole,
					ServiceAccountTokenPath: testOpenBaoSAToken,
					KVMount:                 "kv",
					Field:                   testOpenBaoField,
				},
			}

			auth, err := s.resolveAuth(context.Background(), repo)
			if tt.wantCode != "" {
				var se *secretError
				if !errors.As(err, &se) {
					t.Fatalf("error %v is not a *secretError", err)
				}
				if se.code != tt.wantCode {
					t.Errorf("code = %q, want %q", se.code, tt.wantCode)
				}
				for _, want := range tt.wantErr {
					if !errors.Is(err, want) {
						t.Errorf("error %q is not %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveAuth: %v", err)
			}
			if auth == nil {
				t.Fatal("expected non-nil auth")
			}
		})
	}
}

// TestResolveAuth_FilePathUnchanged verifies that when repo.OpenBao is nil,
// the file-based path is used.
func TestResolveAuth_FilePathUnchanged(t *testing.T) {
	keyPath := writeTempEd25519Key(t)
	s := New()

	repo := &config.RepoConfig{
		SSHKeyPath: keyPath,
		Name:       testAuthRepoName,
		URL:        testAuthRepoURL,
	}

	auth, err := s.resolveAuth(context.Background(), repo)
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if auth == nil {
		t.Fatal("expected non-nil auth")
	}
}

// startRejectingSSHServer starts an in-process SSH server that rejects every
// public key authentication attempt. Returns the listener address, host
// public key and connection count.
func startRejectingSSHServer(t *testing.T) (addr string, hostPub ssh.PublicKey, conns *atomic.Int32) {
	t.Helper()

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("host signer: %v", err)
	}
	hostPub = hostSigner.PublicKey()

	sshCfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, _ ssh.PublicKey) (*ssh.Permissions, error) {
			return nil, fmt.Errorf("rejected")
		},
	}
	sshCfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	connCount := &atomic.Int32{}
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			connCount.Add(1)
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_, _, _, _ = ssh.NewServerConn(c, sshCfg)
			}(conn)
		}
	}()

	return ln.Addr().String(), hostPub, connCount
}

// sshRepoConfig returns a repo config syncing main from the SSH server at addr.
func sshRepoConfig(t *testing.T, name, addr, knownHosts string) *config.RepoConfig {
	t.Helper()
	repo := &config.RepoConfig{
		Name:          name,
		URL:           fmt.Sprintf("ssh://git@%s/r.git", addr),
		SSHKeyPath:    "ssh-keys/test",
		SSHKnownHosts: knownHosts,
	}
	repo.LocalPath = t.TempDir()
	repo.Branches = []config.Pattern{{Raw: "main"}}
	repo.PollInterval = config.Duration(60_000_000_000) // 1m
	return repo
}

// TestSyncRepo_SSHAuthFailureRefetchesKeyOnce verifies that when SSH auth
// fails with an OpenBao key, the key is invalidated, refetched, and one retry
// is attempted. With a file-based key, no invalidation happens.
func TestSyncRepo_SSHAuthFailureRefetchesKeyOnce(t *testing.T) {
	sshAddr, hostPub, sshConns := startRejectingSSHServer(t)
	knownHosts := knownhosts.Line([]string{sshAddr}, hostPub)
	keyPath := writeTempEd25519Key(t)
	pemBytes := generateEd25519PEM(t)

	testOBCfg := &secrets.OpenBaoConfig{
		Addr:                    testOpenBaoAddr,
		Role:                    testOpenBaoRole,
		ServiceAccountTokenPath: testOpenBaoSAToken,
		KVMount:                 "kv",
		Field:                   testOpenBaoField,
	}

	tests := []struct {
		name            string
		openvox         bool
		openBao         *secrets.OpenBaoConfig
		readerErr       error
		errAfter        int
		wantReads       int
		wantInvalidated int
		wantConns       int32
		wantRefetch     float64
		wantCode        FailureCode
	}{
		{
			name:            "openbao standard",
			openBao:         testOBCfg,
			wantReads:       2,
			wantInvalidated: 1,
			wantConns:       2,
			wantRefetch:     1,
			wantCode:        CodeGitAuthFailed,
		},
		{
			name:            "openbao openvox",
			openvox:         true,
			openBao:         testOBCfg,
			wantReads:       2,
			wantInvalidated: 1,
			wantConns:       2,
			wantRefetch:     1,
			wantCode:        CodeGitAuthFailed,
		},
		{
			name:            "file key no invalidation",
			wantReads:       0, // file path, no secret reader call
			wantInvalidated: 0,
			wantConns:       1,
			wantRefetch:     0,
			wantCode:        CodeGitAuthFailed,
		},
		{
			name:            "openbao refetch fails",
			openBao:         testOBCfg,
			readerErr:       secrets.ErrUnavailable,
			errAfter:        1,
			wantReads:       2,
			wantInvalidated: 1,
			wantConns:       1,
			wantRefetch:     1,
			wantCode:        CodeSecretUnavailable,
		},
		{
			name:            "openbao openvox refetch fails",
			openvox:         true,
			openBao:         testOBCfg,
			readerErr:       secrets.ErrUnavailable,
			errAfter:        1,
			wantReads:       2,
			wantInvalidated: 1,
			wantConns:       1,
			wantRefetch:     1,
			wantCode:        CodeSecretUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sshConns.Store(0)

			reader := &fakeSecretReader{data: pemBytes, err: tt.readerErr, errAfter: tt.errAfter}
			s := New(WithSecretReader(reader))

			repo := sshRepoConfig(t, "ssh-fail-"+tt.name, sshAddr, knownHosts)
			repo.OpenBao = tt.openBao
			repo.OpenVox = &tt.openvox

			if tt.openBao == nil {
				repo.SSHKeyPath = keyPath
			}

			before := testutil.ToFloat64(telemetry.SecretAuthRefetchTotal.WithLabelValues(repo.Name))

			result := s.SyncRepo(context.Background(), repo, SyncOptions{})

			if result.Err == nil {
				t.Fatal("expected error (SSH auth should always fail against rejecting server)")
			}
			if got := Classify(result.Err); got != tt.wantCode {
				t.Errorf("Classify(%q) = %q, want %q", result.Err, got, tt.wantCode)
			}
			if !strings.Contains(result.Err.Error(), "ssh: unable to authenticate") {
				t.Errorf("error %q does not carry the ssh auth failure text", result.Err)
			}

			reader.mu.Lock()
			reads := reader.reads
			invalidated := reader.invalidated
			reader.mu.Unlock()

			if reads != tt.wantReads {
				t.Errorf("reads = %d, want %d", reads, tt.wantReads)
			}
			if invalidated != tt.wantInvalidated {
				t.Errorf("invalidated = %d, want %d", invalidated, tt.wantInvalidated)
			}
			if got := sshConns.Load(); got != tt.wantConns {
				t.Errorf("ssh conns = %d, want %d", got, tt.wantConns)
			}

			after := testutil.ToFloat64(telemetry.SecretAuthRefetchTotal.WithLabelValues(repo.Name))
			if delta := after - before; delta != tt.wantRefetch {
				t.Errorf("refetch counter delta = %v, want %v", delta, tt.wantRefetch)
			}
		})
	}
}

// TestSyncRepo_HostKeyMismatchClassified verifies that a host key not matching
// the known_hosts entry for the dialed host:port classifies as a host key
// mismatch, for both the standard and the openvox flow.
func TestSyncRepo_HostKeyMismatchClassified(t *testing.T) {
	sshAddr, _, _ := startRejectingSSHServer(t)
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate other host key: %v", err)
	}
	otherSigner, err := ssh.NewSignerFromKey(otherPriv)
	if err != nil {
		t.Fatalf("other host signer: %v", err)
	}
	knownHosts := knownhosts.Line([]string{sshAddr}, otherSigner.PublicKey())
	keyPath := writeTempEd25519Key(t)

	tests := []struct {
		name    string
		openvox bool
	}{
		{name: "standard"},
		{name: "openvox", openvox: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New()
			repo := sshRepoConfig(t, "host-key-mismatch-"+tt.name, sshAddr, knownHosts)
			repo.SSHKeyPath = keyPath
			repo.OpenVox = &tt.openvox

			result := s.SyncRepo(context.Background(), repo, SyncOptions{})
			if result.Err == nil {
				t.Fatal("expected error for host key mismatch")
			}
			if got := Classify(result.Err); got != CodeGitHostKeyMismatch {
				t.Errorf("Classify(%q) = %q, want %q", result.Err, got, CodeGitHostKeyMismatch)
			}
		})
	}
}
