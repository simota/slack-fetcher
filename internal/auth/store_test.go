package auth

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCredentialsASCIIEncodingAndLegacyCompatibility(t *testing.T) {
	orig := Credentials{
		AccessToken:  "xoxp-12345",
		RefreshToken: "xoxe-1-refresh",
		ExpiresAt:    time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
		ClientID:     "111.222",
		TeamID:       "T01",
		TeamName:     "開発チーム🚀",
		UserID:       "U01",
	}

	encoded, err := EncodeCredentialsASCII(orig)
	if err != nil {
		t.Fatalf("EncodeCredentialsASCII: %v", err)
	}
	for i := 0; i < len(encoded); i++ {
		if encoded[i] < 0x20 || encoded[i] >= 0x7f {
			t.Fatalf("encoded JSON contains non-printable/non-ASCII byte 0x%02x: %q", encoded[i], encoded)
		}
	}

	parsed, err := ParseStoredCredentials(encoded)
	if err != nil {
		t.Fatalf("ParseStoredCredentials(JSON): %v", err)
	}
	if parsed.TeamName != orig.TeamName || parsed.AccessToken != orig.AccessToken || parsed.RefreshToken != orig.RefreshToken {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", parsed, orig)
	}

	legacy, err := ParseStoredCredentials("  xoxp-legacy-raw-token\n")
	if err != nil {
		t.Fatalf("ParseStoredCredentials(legacy): %v", err)
	}
	if legacy.AccessToken != "xoxp-legacy-raw-token" {
		t.Fatalf("legacy AccessToken = %q", legacy.AccessToken)
	}
}

func TestKeychainStore_SaveLoadDeleteWithoutLeakingSecretInArgv(t *testing.T) {
	var storedHex string
	var sawSecretInArgv bool

	store := KeychainStore{
		Service: "slack-fetcher-test",
		Account: "alice",
		Run: func(_ context.Context, stdin string, args ...string) (string, string, error) {
			for _, a := range args {
				if strings.Contains(a, "xoxp-secret") {
					sawSecretInArgv = true
				}
			}
			if len(args) > 0 && args[0] == "-i" {
				if !strings.HasPrefix(stdin, "add-generic-password -U -a alice -s slack-fetcher-test -X ") {
					t.Fatalf("unexpected stdin command: %q", stdin)
				}
				fields := strings.Fields(stdin)
				storedHex = fields[len(fields)-1]
				return "", "", nil
			}
			if len(args) > 0 && args[0] == "find-generic-password" {
				if storedHex == "" {
					return "", "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.", errors.New("exit status 44")
				}
				raw, err := hex.DecodeString(storedHex)
				if err != nil {
					t.Fatalf("decode stored hex: %v", err)
				}
				return string(raw) + "\n", "", nil
			}
			if len(args) > 0 && args[0] == "delete-generic-password" {
				storedHex = ""
				return "", "", nil
			}
			t.Fatalf("unexpected args: %v", args)
			return "", "", nil
		},
	}

	creds := Credentials{AccessToken: "xoxp-secret-abc", TeamName: "テスト社"}
	if err := store.Save(context.Background(), creds); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if sawSecretInArgv {
		t.Fatalf("secret appeared in command argv")
	}

	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.AccessToken != creds.AccessToken || loaded.TeamName != creds.TeamName {
		t.Fatalf("loaded = %+v, want %+v", loaded, creds)
	}

	if err := store.Delete(context.Background()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Load(context.Background()); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("after Delete Load err = %v, want ErrTokenNotFound", err)
	}
}

func TestKeychainStore_RejectsUnsafeIdentifiers(t *testing.T) {
	store := KeychainStore{Service: "bad\"service", Account: "alice"}
	if err := store.Save(context.Background(), Credentials{AccessToken: "xoxp-1"}); err == nil {
		t.Fatalf("expected error for unsafe service identifier")
	}
}

func TestFileStore_RoundTripAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	path := filepath.Join(dir, "credentials.json")
	store := FileStore{Path: path}

	if _, err := store.Load(context.Background()); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("initial Load err = %v, want ErrTokenNotFound", err)
	}

	want := Credentials{AccessToken: "xoxp-file-token", TeamID: "T1", TeamName: "Corp"}
	if err := store.Save(context.Background(), want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("file perm = %o, want 0600", perm)
	}

	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.AccessToken != want.AccessToken || got.TeamName != want.TeamName {
		t.Fatalf("got = %+v, want %+v", got, want)
	}

	if err := store.Delete(context.Background()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Load(context.Background()); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("after Delete Load err = %v, want ErrTokenNotFound", err)
	}
}
