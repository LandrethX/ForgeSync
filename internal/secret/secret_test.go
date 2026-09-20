package secret

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey(t *testing.T) *Key {
	t.Helper()
	k, err := NewKey(bytes.Repeat([]byte{7}, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSealAndOpenRoundTrip(t *testing.T) {
	k := testKey(t)
	for _, want := range []string{"", "a", "3f9c1e07b2d48516c0af93e27db5140ce8a7b62x", strings.Repeat("long ", 500)} {
		sealed, err := k.Seal(want)
		if err != nil {
			t.Fatal(err)
		}
		// Only worth asserting for a plaintext long enough that finding
		// it in random-looking bytes would mean something: a single byte
		// turns up in a 28-byte ciphertext about one time in ten, which
		// says nothing about the sealing and everything about chance.
		if len(want) >= 8 && bytes.Contains(sealed, []byte(want)) {
			t.Fatalf("the sealed value contains the plaintext")
		}
		got, err := k.Open(sealed)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("round trip gave %q, want %q", got, want)
		}
	}
}

// Sealing the same value twice must not give the same bytes, or the
// database would say which nodes share a token.
func TestSealingTwiceDiffers(t *testing.T) {
	k := testKey(t)
	a, err := k.Seal("the same token")
	if err != nil {
		t.Fatal(err)
	}
	b, err := k.Seal("the same token")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two seals of one value are identical, so the nonce is not fresh")
	}
}

// The whole point: the database alone is not enough.
func TestAnotherKeyCannotOpenIt(t *testing.T) {
	k := testKey(t)
	sealed, err := k.Seal("site-admin token")
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewKey(bytes.Repeat([]byte{8}, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Open(sealed); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("opening with another key gave %v, want ErrWrongKey", err)
	}
}

// A changed byte must be refused rather than decrypted to something else.
func TestTamperingIsRefused(t *testing.T) {
	k := testKey(t)
	sealed, err := k.Seal("site-admin token")
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, len(sealed) / 2, len(sealed) - 1} {
		bad := bytes.Clone(sealed)
		bad[i] ^= 0xff
		if _, err := k.Open(bad); !errors.Is(err, ErrWrongKey) {
			t.Fatalf("a value with byte %d flipped opened: %v", i, err)
		}
	}
	if _, err := k.Open(sealed[:3]); !errors.Is(err, ErrWrongKey) {
		t.Fatal("a truncated value opened")
	}
}

func TestKeySizeIsEnforced(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33} {
		if _, err := NewKey(make([]byte, n)); err == nil {
			t.Fatalf("a %d-byte key was accepted", n)
		}
	}
}

func TestGenerateAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "node-key")
	if err := GenerateKey(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("key file mode is %o, want 600", got)
	}
	k, err := LoadKey(path)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := k.Seal("token")
	if err != nil {
		t.Fatal(err)
	}
	again, err := LoadKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := again.Open(sealed); err != nil || got != "token" {
		t.Fatalf("a reloaded key gave %q, %v", got, err)
	}

	// Generating over an existing key would make every sealed token in the
	// database unopenable, so it has to fail rather than succeed quietly.
	if err := GenerateKey(path); err == nil {
		t.Fatal("GenerateKey overwrote an existing key")
	}
}

// openssl rand -hex 32 and openssl rand -base64 32 both have to work: they
// are what a person will actually type.
func TestLoadAcceptsHexAndBase64(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"hex":           "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20\n",
		"base64":        "AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyA=\n",
		"hex-untrimmed": "  0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20  \n",
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadKey(path); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := filepath.Join(dir, "rubbish")
	if err := os.WriteFile(bad, []byte("not a key at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(bad); err == nil {
		t.Error("a file that is not a key was accepted")
	}
	if _, err := LoadKey(filepath.Join(dir, "empty-file-that-is-not-there")); err == nil {
		t.Error("a missing key file was accepted")
	}
}
