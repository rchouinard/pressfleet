package api

import (
	"context"
	"errors"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

func TestAuthorizeRejectsUndeclaredAction(t *testing.T) {
	h := &Handlers{}
	admin := auth.Principal{UserID: "u", Role: store.RoleAdmin, Method: auth.MethodSession}
	ctx := auth.WithPrincipal(context.Background(), admin)
	ctx = context.WithValue(ctx, operationKey{}, &Operation{ID: "getSite", Action: authz.ViewSites})

	if _, err := h.authorize(ctx, authz.ViewSites, authz.Site("s")); err != nil {
		t.Errorf("declared action: %v", err)
	}
	if _, err := h.authorize(ctx, authz.DeleteSite, authz.Site("s")); !errors.Is(err, errActionMismatch) {
		t.Errorf("undeclared action: err = %v, want errActionMismatch", err)
	}
	// An operation whose action depends on its target accepts its
	// alternatives too.
	upload := context.WithValue(auth.WithPrincipal(context.Background(), admin), operationKey{},
		&Operation{ID: "getUpload", Action: authz.UploadForImport, AltActions: []authz.Action{authz.UploadForDeploy}})
	for _, a := range []authz.Action{authz.UploadForImport, authz.UploadForDeploy} {
		if _, err := h.authorize(upload, a, authz.Upload("u")); err != nil {
			t.Errorf("declared action %s: %v", a, err)
		}
	}
	if _, err := h.authorize(upload, authz.DeployCode, authz.Upload("u")); !errors.Is(err, errActionMismatch) {
		t.Errorf("undeclared alternative: err = %v, want errActionMismatch", err)
	}
	if _, err := h.authorize(context.Background(), authz.ViewSites, authz.Site("s")); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("no principal: err = %v, want ErrUnauthenticated", err)
	}
}

func TestCheckActions(t *testing.T) {
	ok := operationTable{
		"GET /a": {ID: "getSite", Action: authz.ViewSites},
		"GET /h": {ID: "getHealth", Public: true},
	}
	// Every declared operation must exist, so this table is incomplete on
	// purpose; only the per-operation rules are checked here.
	for name, tbl := range map[string]operationTable{
		"protected without action": {"GET /a": {ID: "getSite"}},
		"unknown action":           {"GET /a": {ID: "getSite", Action: "site.explode"}},
		"public with action":       {"GET /h": {ID: "getHealth", Public: true, Action: authz.ViewSites}},
		"unknown alternative":      {"GET /a": {ID: "getSite", Action: authz.ViewSites, AltActions: []authz.Action{"site.explode"}}},
		"public with alternative":  {"GET /h": {ID: "getHealth", Public: true, AltActions: []authz.Action{authz.ViewSites}}},
	} {
		if err := tbl.checkActions(); err == nil {
			t.Errorf("%s: checkActions passed", name)
		}
	}
	if err := ok.checkActions(); err == nil {
		t.Error("undeclared-but-listed operations weren't reported")
	}
	doc, err := loadSpec()
	if err != nil {
		t.Fatal(err)
	}
	if err := newOperationTable(doc).checkActions(); err != nil {
		t.Errorf("the real operation table: %v", err)
	}
}
