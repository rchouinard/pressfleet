package auth_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// sshPublicKey returns the SSH public key of a crypto public key.
func sshPublicKey(t *testing.T, pub any) ssh.PublicKey {
	t.Helper()
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func ed25519Key(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return sshPublicKey(t, pub)
}

func authorizedKey(k ssh.PublicKey, comment string) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) + " " + comment
}

func TestParseSSHPublicKey(t *testing.T) {
	ed := ed25519Key(t)
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsaSmall, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	rsaOK, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	edLine := authorizedKey(ed, "dev@laptop")
	tests := []struct {
		name, in, comment string
		ok                bool
	}{
		{"ed25519", edLine, "dev@laptop", true},
		{"surrounding space", "  " + edLine + "\n", "dev@laptop", true},
		{"no comment", strings.TrimSpace(string(ssh.MarshalAuthorizedKey(ed))), "", true},
		{"ecdsa", authorizedKey(sshPublicKey(t, &ec.PublicKey), "c"), "c", true},
		{"rsa 2048", authorizedKey(sshPublicKey(t, &rsaOK.PublicKey), "r"), "r", true},
		{"rsa 1024", authorizedKey(sshPublicKey(t, &rsaSmall.PublicKey), "r"), "", false},
		{"empty", "", "", false},
		{"garbage", "hello world", "", false},
		{"options", `command="ls" ` + edLine, "", false},
		{"two keys", edLine + "\n" + edLine, "", false},
		{"private key", "-----BEGIN OPENSSH PRIVATE KEY-----", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, comment, err := auth.ParseSSHPublicKey(tt.in)
			if !tt.ok {
				if !errors.Is(err, auth.ErrInvalidSSHKey) {
					t.Errorf("ParseSSHPublicKey = %v, want ErrInvalidSSHKey", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSSHPublicKey: %v", err)
			}
			if comment != tt.comment {
				t.Errorf("comment = %q, want %q", comment, tt.comment)
			}
			if got := auth.FormatSSHPublicKey(key); strings.Count(got, " ") != 1 || !strings.HasPrefix(got, key.Type()+" ") {
				t.Errorf("FormatSSHPublicKey = %q, want \"<type> <base64>\"", got)
			}
		})
	}
}

func TestSSHKeys(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, clk := newService(t, db)
		op := mustCreate(t, svc, "op@example.com", store.RoleOperator)
		other := mustCreate(t, svc, "other@example.com", store.RoleViewer)
		key := ed25519Key(t)

		k, err := svc.AddSSHKey(ctx, op.ID, auth.NewSSHKey{PublicKey: authorizedKey(key, "op@laptop")})
		if err != nil {
			t.Fatal(err)
		}
		if k.Name != "op@laptop" || k.Fingerprint != ssh.FingerprintSHA256(key) || k.PublicKey != auth.FormatSSHPublicKey(key) {
			t.Errorf("AddSSHKey = %+v", k)
		}
		named, err := svc.AddSSHKey(ctx, op.ID, auth.NewSSHKey{Name: " CI ", PublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(ed25519Key(t))))})
		if err != nil || named.Name != "CI" {
			t.Errorf("AddSSHKey with a name = %+v, %v", named, err)
		}
		bare, err := svc.AddSSHKey(ctx, op.ID, auth.NewSSHKey{PublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(ed25519Key(t))))})
		if err != nil || bare.Name != ssh.KeyAlgoED25519 {
			t.Errorf("AddSSHKey without name or comment = %+v, %v", bare, err)
		}

		// A key identifies one user.
		if _, err := svc.AddSSHKey(ctx, other.ID, auth.NewSSHKey{PublicKey: authorizedKey(key, "copy")}); !errors.Is(err, auth.ErrSSHKeyTaken) {
			t.Errorf("registering another user's key: %v, want ErrSSHKeyTaken", err)
		}
		if _, err := svc.AddSSHKey(ctx, op.ID, auth.NewSSHKey{PublicKey: "nope"}); !errors.Is(err, auth.ErrInvalidSSHKey) {
			t.Errorf("invalid key: %v", err)
		}
		if _, err := svc.AddSSHKey(ctx, op.ID, auth.NewSSHKey{Name: strings.Repeat("n", 101), PublicKey: authorizedKey(ed25519Key(t), "")}); !errors.Is(err, auth.ErrInvalidSSHKeyName) {
			t.Errorf("long name: %v", err)
		}
		if long, err := svc.AddSSHKey(ctx, other.ID, auth.NewSSHKey{PublicKey: authorizedKey(ed25519Key(t), strings.Repeat("c", 200))}); err != nil || len(long.Name) != 100 {
			t.Errorf("a long comment should be shortened: %+v, %v", long, err)
		}
		if _, err := svc.AddSSHKey(ctx, "01m3nh4d6pknrsr3h3cbe1xctr", auth.NewSSHKey{PublicKey: authorizedKey(ed25519Key(t), "")}); !errors.Is(err, auth.ErrUserNotFound) {
			t.Errorf("key for a missing user: %v", err)
		}
		if keys, err := svc.ListSSHKeys(ctx, op.ID, "", 0); err != nil || len(keys) != 3 {
			t.Errorf("ListSSHKeys = %+v, %v", keys, err)
		}

		// Authentication by key.
		p, got, err := svc.AuthenticateSSHKey(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if p.UserID != op.ID || p.Role != store.RoleOperator || p.Method != auth.MethodSSHKey || p.SSHKeyID != k.ID || got.ID != k.ID {
			t.Errorf("AuthenticateSSHKey = %+v, %+v", p, got)
		}
		if unused, _ := store.NewSSHKeys(db).Get(ctx, k.ID); unused.LastUsedAt != nil {
			t.Error("AuthenticateSSHKey recorded the key's use; only NoteSSHKeyUsed may")
		}
		svc.NoteSSHKeyUsed(ctx, got)
		used, _ := store.NewSSHKeys(db).Get(ctx, k.ID)
		if used.LastUsedAt == nil {
			t.Fatal("NoteSSHKeyUsed didn't record the key's use")
		}
		clk.Add(30 * time.Second)
		svc.NoteSSHKeyUsed(ctx, used)
		if again, _ := store.NewSSHKeys(db).Get(ctx, k.ID); !again.LastUsedAt.Equal(*used.LastUsedAt) {
			t.Error("NoteSSHKeyUsed wrote again within a minute")
		}
		if _, _, err := svc.AuthenticateSSHKey(ctx, ed25519Key(t)); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("unknown key: %v, want ErrUnauthenticated", err)
		}
		if p, err := svc.RecheckSSHKey(ctx, k.ID); err != nil || p.UserID != op.ID {
			t.Errorf("RecheckSSHKey = %+v, %v", p, err)
		}

		// Role changes show at once; disabling the user ends access.
		viewer := store.RoleViewer
		if _, err := svc.UpdateUser(ctx, op.ID, auth.UserChanges{Role: &viewer}); err != nil {
			t.Fatal(err)
		}
		if p, err := svc.RecheckSSHKey(ctx, k.ID); err != nil || p.Role != store.RoleViewer {
			t.Errorf("after demotion: %+v, %v", p, err)
		}
		disabled := true
		if _, err := svc.UpdateUser(ctx, op.ID, auth.UserChanges{Disabled: &disabled}); err != nil {
			t.Fatal(err)
		}
		clk.Add(time.Minute)
		if _, _, err := svc.AuthenticateSSHKey(ctx, key); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("disabled user's key: %v, want ErrUnauthenticated", err)
		}
		if _, err := svc.RecheckSSHKey(ctx, k.ID); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("recheck of a disabled user's key: %v, want ErrUnauthenticated", err)
		}
		enabled := false
		if _, err := svc.UpdateUser(ctx, op.ID, auth.UserChanges{Disabled: &enabled}); err != nil {
			t.Fatal(err)
		}

		// Removing the key ends its access; only its owner's removal counts.
		if err := svc.DeleteSSHKey(ctx, other.ID, k.ID); !errors.Is(err, auth.ErrSSHKeyNotFound) {
			t.Errorf("deleting another user's key: %v", err)
		}
		if err := svc.DeleteSSHKey(ctx, op.ID, k.ID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := svc.AuthenticateSSHKey(ctx, key); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("removed key: %v, want ErrUnauthenticated", err)
		}
		if _, err := svc.RecheckSSHKey(ctx, k.ID); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("recheck of a removed key: %v, want ErrUnauthenticated", err)
		}

		// Changing the password leaves SSH keys alone: they are separate
		// credentials the user manages.
		if err := svc.ChangePassword(ctx, auth.Principal{UserID: op.ID, Email: op.Email}, pw, redact.Secret(pw+"!")); err != nil {
			t.Fatal(err)
		}
		if keys, _ := svc.ListSSHKeys(ctx, op.ID, "", 0); len(keys) != 2 {
			t.Errorf("keys after a password change: %d, want 2", len(keys))
		}
	})
}
