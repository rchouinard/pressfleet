package api_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// newAuthorizedKey returns a fresh Ed25519 public key in authorized_keys
// format, with a comment.
func newAuthorizedKey(t *testing.T, comment string) (string, ssh.PublicKey) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) + " " + comment, k
}

type sshKeyBody struct {
	ID, Name, Type, Fingerprint string
	PublicKey                   string  `json:"public_key"`
	LastUsedAt                  *string `json:"last_used_at"`
}

func keyJSON(t *testing.T, name, key string) string {
	t.Helper()
	b := map[string]string{"public_key": key}
	if name != "" {
		b["name"] = name
	}
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestSSHKeysOverHTTP(t *testing.T) {
	e := newAuthEnv(t, api.Options{})
	admin := e.mustLogin()
	ctx := context.Background()
	viewer, err := e.svc.CreateUser(ctx, auth.NewUser{Email: "viewer@example.com", Password: testPassword, Role: store.RoleViewer})
	if err != nil {
		t.Fatal(err)
	}
	vs, w := e.login("viewer@example.com", testPassword, nil)
	if vs.cookie == nil {
		t.Fatalf("viewer login: %d %s", w.Code, w.Body)
	}

	// Register one's own key.
	line, key := newAuthorizedKey(t, "admin@laptop")
	w = e.do(http.MethodPost, "/api/v1/users/me/ssh-keys", keyJSON(t, "", line), admin.creds())
	expectStatus(t, w, http.StatusCreated, "add own key")
	var k sshKeyBody
	if err := json.Unmarshal(w.Body.Bytes(), &k); err != nil {
		t.Fatal(err)
	}
	if k.Name != "admin@laptop" || k.Type != "ssh-ed25519" || k.Fingerprint != ssh.FingerprintSHA256(key) ||
		k.PublicKey != strings.TrimSuffix(line, " admin@laptop") || k.LastUsedAt != nil ||
		w.Header().Get("Location") != "/api/v1/users/me/ssh-keys/"+k.ID {
		t.Errorf("added key: %s (Location %q)", w.Body, w.Header().Get("Location"))
	}
	w = e.do(http.MethodGet, "/api/v1/users/me/ssh-keys", "", admin.creds())
	expectStatus(t, w, http.StatusOK, "list own keys")
	var list struct {
		Items      []sshKeyBody
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Items) != 1 || list.Items[0].ID != k.ID || list.NextCursor != nil {
		t.Errorf("own keys: %s", w.Body)
	}

	// A key belongs to one user; bad keys say what is wrong.
	expectStatus(t, e.do(http.MethodPost, "/api/v1/users/me/ssh-keys", keyJSON(t, "", line), vs.creds()), http.StatusConflict, "someone else's key")
	w = e.do(http.MethodPost, "/api/v1/users/me/ssh-keys", keyJSON(t, "", "ssh-dss AAAAB3NzaC1kc3M="), admin.creds())
	expectStatus(t, w, http.StatusUnprocessableEntity, "unsupported key")
	if !strings.Contains(w.Body.String(), "/public_key") {
		t.Errorf("unsupported key problem doesn't name the field: %s", w.Body)
	}

	// Viewers manage their own keys, but not others'.
	vline, _ := newAuthorizedKey(t, "viewer@laptop")
	w = e.do(http.MethodPost, "/api/v1/users/me/ssh-keys", keyJSON(t, "Viewer's key", vline), vs.creds())
	expectStatus(t, w, http.StatusCreated, "viewer adds own key")
	var vk sshKeyBody
	_ = json.Unmarshal(w.Body.Bytes(), &vk)
	if vk.Name != "Viewer's key" {
		t.Errorf("viewer's key name = %q", vk.Name)
	}
	userKeys := "/api/v1/users/" + viewer.ID + "/ssh-keys"
	expectStatus(t, e.do(http.MethodGet, "/api/v1/users/"+e.user.ID+"/ssh-keys", "", vs.creds()), http.StatusForbidden, "viewer lists admin's keys")
	expectStatus(t, e.do(http.MethodDelete, "/api/v1/users/me/ssh-keys/"+k.ID, "", vs.creds()), http.StatusNotFound, "viewer deletes admin's key as own")

	// Admins manage anyone's keys.
	w = e.do(http.MethodGet, userKeys, "", admin.creds())
	expectStatus(t, w, http.StatusOK, "admin lists viewer's keys")
	if !strings.Contains(w.Body.String(), vk.ID) {
		t.Errorf("viewer's keys: %s", w.Body)
	}
	other, _ := newAuthorizedKey(t, "")
	w = e.do(http.MethodPost, userKeys, keyJSON(t, "", other), admin.creds())
	expectStatus(t, w, http.StatusCreated, "admin adds a key for the viewer")
	var ok sshKeyBody
	_ = json.Unmarshal(w.Body.Bytes(), &ok)
	if ok.Name != "ssh-ed25519" || w.Header().Get("Location") != userKeys+"/"+ok.ID {
		t.Errorf("admin-added key: %s (Location %q)", w.Body, w.Header().Get("Location"))
	}
	expectStatus(t, e.do(http.MethodDelete, userKeys+"/"+ok.ID, "", admin.creds()), http.StatusNoContent, "admin deletes the viewer's key")
	expectStatus(t, e.do(http.MethodDelete, userKeys+"/"+k.ID, "", admin.creds()), http.StatusNotFound, "admin's key through the viewer")
	missing := "/api/v1/users/01m3nh4d6pknrsr3h3cbe1xctr/ssh-keys"
	expectStatus(t, e.do(http.MethodGet, missing, "", admin.creds()), http.StatusNotFound, "keys of a missing user")
	expectStatus(t, e.do(http.MethodPost, missing, keyJSON(t, "", other), admin.creds()), http.StatusNotFound, "key for a missing user")

	// Read-only tokens can list keys but not add them.
	_, ro := e.createToken(admin, `{"name":"ro","scope":"read-only"}`)
	expectStatus(t, e.do(http.MethodGet, "/api/v1/users/me/ssh-keys", "", creds{bearer: ro}), http.StatusOK, "read-only token lists")
	third, _ := newAuthorizedKey(t, "")
	expectStatus(t, e.do(http.MethodPost, "/api/v1/users/me/ssh-keys", keyJSON(t, "", third), creds{bearer: ro}), http.StatusForbidden, "read-only token adds")

	// Removing a key.
	expectStatus(t, e.do(http.MethodDelete, "/api/v1/users/me/ssh-keys/"+k.ID, "", admin.creds()), http.StatusNoContent, "delete own key")
	expectStatus(t, e.do(http.MethodDelete, "/api/v1/users/me/ssh-keys/"+k.ID, "", admin.creds()), http.StatusNotFound, "delete own key again")

	// Registering and removing keys is audited with the key as target.
	page, err := store.NewAuditEvents(e.db).List(ctx, store.AuditFilter{TargetType: "ssh_key", TargetID: k.ID})
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, ev := range page.Events {
		actions = append(actions, ev.Action+"/"+string(ev.Result))
	}
	if got := strings.Join(actions, " "); got != "self.ssh_keys.delete/failure self.ssh_keys.delete/success user.ssh_keys.manage/failure self.ssh_keys.delete/failure self.ssh_keys.create/success" {
		t.Errorf("audit events of the key, newest first: %s", got)
	}
}

func TestAuditEventDetailsOverHTTP(t *testing.T) {
	e := newAuthEnv(t, api.Options{})
	s := e.mustLogin()
	ev := store.AuditEvent{Action: "site.sftp.disconnect", Result: store.AuditSuccess, ActorUserID: e.user.ID,
		Details: map[string]string{"bytes_received": "42", "key_fingerprint": "SHA256:abc"}}
	if err := store.NewAuditEvents(e.db).Insert(context.Background(), &ev); err != nil {
		t.Fatal(err)
	}
	w := e.do(http.MethodGet, "/api/v1/audit-events?action=site.sftp.disconnect", "", s.creds())
	expectStatus(t, w, http.StatusOK, "list audit events")
	var body struct {
		Items []struct {
			Details map[string]string `json:"details"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Items) != 1 ||
		body.Items[0].Details["bytes_received"] != "42" || body.Items[0].Details["key_fingerprint"] != "SHA256:abc" {
		t.Errorf("audit events: %s", w.Body)
	}
	w = e.do(http.MethodGet, "/api/v1/audit-events?action=self.login", "", s.creds())
	if strings.Contains(w.Body.String(), `"details"`) {
		t.Errorf("events without details must omit them: %s", w.Body)
	}
}
