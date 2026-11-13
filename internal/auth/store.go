package auth

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

// Credentials holds persisted Slack authentication state for the local user.
type Credentials struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	ClientID     string    `json:"client_id,omitempty"`
	TeamID       string    `json:"team_id,omitempty"`
	TeamName     string    `json:"team_name,omitempty"`
	UserID       string    `json:"user_id,omitempty"`
	Scope        string    `json:"scope,omitempty"`
}

// Store persists user Slack credentials securely on the local machine.
type Store interface {
	Load(ctx context.Context) (Credentials, error)
	Save(ctx context.Context, creds Credentials) error
	Delete(ctx context.Context) error
	Description() string
}

// DefaultStore returns the platform-appropriate credential store:
// macOS Keychain on darwin, and a 0600 JSON file under UserConfigDir elsewhere.
func DefaultStore(getenv func(string) string) Store {
	service := resolveKeychainService(getenv, DefaultKeychainService)
	if runtime.GOOS == "darwin" {
		account := ""
		if getenv != nil {
			account = strings.TrimSpace(getenv("USER"))
		}
		if account == "" {
			account = "default"
		}
		return KeychainStore{
			Service: service,
			Account: account,
			Run:     runSecurityCommand,
		}
	}

	return FileStore{
		Path: defaultCredentialsFilePath(getenv),
	}
}

func defaultCredentialsFilePath(getenv func(string) string) string {
	if getenv != nil {
		if xdg := strings.TrimSpace(getenv("XDG_CONFIG_HOME")); xdg != "" {
			return filepath.Join(xdg, "slack-fetcher", "credentials.json")
		}
	}
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "slack-fetcher", "credentials.json")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "slack-fetcher", "credentials.json")
	}
	return filepath.Join(os.TempDir(), "slack-fetcher", "credentials.json")
}

// ParseStoredCredentials decodes JSON Credentials or falls back to legacy raw token strings.
func ParseStoredCredentials(raw string) (Credentials, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Credentials{}, ErrTokenNotFound
	}
	if strings.HasPrefix(trimmed, "{") {
		var creds Credentials
		if err := json.Unmarshal([]byte(trimmed), &creds); err != nil {
			return Credentials{}, fmt.Errorf("parse stored credentials: %w", err)
		}
		creds.AccessToken = strings.TrimSpace(creds.AccessToken)
		if creds.AccessToken == "" {
			return Credentials{}, ErrTokenNotFound
		}
		return creds, nil
	}
	return Credentials{AccessToken: trimmed}, nil
}

// EncodeCredentialsASCII serializes Credentials as single-line printable ASCII JSON
// so macOS `security find-generic-password -w` always returns text rather than hex.
func EncodeCredentialsASCII(creds Credentials) (string, error) {
	raw, err := json.Marshal(creds)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.Grow(len(raw))
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRune(raw[i:])
		if r < 0x80 {
			b.WriteByte(raw[i])
			i += size
			continue
		}
		if r <= 0xFFFF {
			fmt.Fprintf(&b, `\u%04x`, r)
		} else {
			r1, r2 := utf16SurrogatePair(r)
			fmt.Fprintf(&b, `\u%04x\u%04x`, r1, r2)
		}
		i += size
	}
	return b.String(), nil
}

func utf16SurrogatePair(r rune) (rune, rune) {
	r -= 0x10000
	return 0xD800 + (r>>10)&0x3FF, 0xDC00 + r&0x3FF
}

// KeychainStore persists credentials in the macOS Keychain via `/usr/bin/security`.
type KeychainStore struct {
	Service string
	Account string
	Run     func(ctx context.Context, stdin string, args ...string) (stdout, stderr string, err error)
}

func (s KeychainStore) Description() string {
	return fmt.Sprintf("macOS Keychain (service=%q)", s.service())
}

func (s KeychainStore) service() string {
	if strings.TrimSpace(s.Service) != "" {
		return strings.TrimSpace(s.Service)
	}
	return DefaultKeychainService
}

func (s KeychainStore) account() string {
	if strings.TrimSpace(s.Account) != "" {
		return strings.TrimSpace(s.Account)
	}
	return "default"
}

func (s KeychainStore) Load(ctx context.Context) (Credentials, error) {
	if err := validateKeychainIdentifier(s.service()); err != nil {
		return Credentials{}, fmt.Errorf("invalid keychain service: %w", err)
	}
	stdout, stderr, err := s.runner()(ctx, "", "find-generic-password", "-s", s.service(), "-w")
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Credentials{}, fmt.Errorf("read macOS Keychain (%s): %w", s.service(), err)
		}
		// Exit code 44 is errSecItemNotFound in macOS security CLI.
		if strings.Contains(stderr, "could not be found") || strings.Contains(stderr, "SecKeychainSearchCopyNext") {
			return Credentials{}, ErrTokenNotFound
		}
		return Credentials{}, ErrTokenNotFound
	}
	return ParseStoredCredentials(stdout)
}

func (s KeychainStore) Save(ctx context.Context, creds Credentials) error {
	svc := s.service()
	acc := s.account()
	if err := validateKeychainIdentifier(svc); err != nil {
		return fmt.Errorf("invalid keychain service: %w", err)
	}
	if err := validateKeychainIdentifier(acc); err != nil {
		return fmt.Errorf("invalid keychain account: %w", err)
	}

	encoded, err := EncodeCredentialsASCII(creds)
	if err != nil {
		return err
	}
	hexPayload := hex.EncodeToString([]byte(encoded))
	// Feed via `security -i` stdin with hex -X so secrets never appear in argv (`ps`).
	cmdLine := fmt.Sprintf("add-generic-password -U -a %s -s %s -X %s\n", acc, svc, hexPayload)

	_, stderr, err := s.runner()(ctx, cmdLine, "-i")
	if err != nil {
		return fmt.Errorf("save credentials to macOS Keychain (%s): %w (%s)", svc, err, strings.TrimSpace(stderr))
	}
	if strings.Contains(stderr, "security:") || strings.Contains(stderr, "Error:") {
		return fmt.Errorf("save credentials to macOS Keychain (%s): %s", svc, strings.TrimSpace(stderr))
	}
	return nil
}

func (s KeychainStore) Delete(ctx context.Context) error {
	svc := s.service()
	if err := validateKeychainIdentifier(svc); err != nil {
		return fmt.Errorf("invalid keychain service: %w", err)
	}
	_, stderr, err := s.runner()(ctx, "", "delete-generic-password", "-s", svc)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if strings.Contains(stderr, "could not be found") {
			return nil
		}
		return fmt.Errorf("delete macOS Keychain item (%s): %w", svc, err)
	}
	return nil
}

func (s KeychainStore) runner() func(ctx context.Context, stdin string, args ...string) (string, string, error) {
	if s.Run != nil {
		return s.Run
	}
	return runSecurityCommand
}

func validateKeychainIdentifier(v string) error {
	if v == "" {
		return errors.New("identifier is empty")
	}
	for _, r := range v {
		if r <= 0x20 || r == 0x7f || r == '"' || r == '\'' || r == '\\' {
			return fmt.Errorf("disallowed character %q", r)
		}
	}
	return nil
}

func runSecurityCommand(ctx context.Context, stdin string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "security", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// FileStore persists credentials in a user-only (0600) JSON file.
type FileStore struct {
	Path string
}

func (s FileStore) Description() string {
	return fmt.Sprintf("credentials file (%s)", s.Path)
}

func (s FileStore) Load(ctx context.Context) (Credentials, error) {
	if err := ctx.Err(); err != nil {
		return Credentials{}, err
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Credentials{}, ErrTokenNotFound
		}
		return Credentials{}, fmt.Errorf("read credentials file: %w", err)
	}
	return ParseStoredCredentials(string(data))
}

func (s FileStore) Save(ctx context.Context, creds Credentials) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".credentials-*")
	if err != nil {
		return fmt.Errorf("create temp credentials file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		_ = tmp.Close()
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod credentials file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write credentials file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close credentials file: %w", err)
	}
	if err := os.Rename(tmpPath, s.Path); err != nil {
		return fmt.Errorf("replace credentials file: %w", err)
	}
	cleanup = false
	return nil
}

func (s FileStore) Delete(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(s.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove credentials file: %w", err)
	}
	return nil
}
