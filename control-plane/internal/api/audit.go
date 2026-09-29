package api

import (
	"context"
	"log/slog"
	"maps"
	"net/http"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// The audit log (#104, design doc §4.6).
//
// Events are gathered at the authorization check point: authorize notes the
// action and the resolved target in the request's auditRecord, and whether
// it denied the request. When the handler has finished, auditRequests
// writes one event for every denial (whatever the method) and for every
// authenticated unsafe request (success or failure by the response
// status). Unsafe requests that fail before reaching the handler, such as
// validation errors, are recorded with the operation's declared action.
// Reads that are allowed aren't recorded, except those whose handler asks
// for it (auditRead), such as downloads of database exports. Login is
// public, so its handler fills in the record itself.
//
// Events hold only IDs, the action, the result, and the client address;
// request bodies and secrets never reach them. They are written after the
// response is complete and outside the handler's transaction, so a failed
// write is logged rather than failing a request that already happened.

// auditActionLogin is the audit action for logging in. Logging out is
// authz.Logout.
const auditActionLogin = "self.login"

// auditRecord collects what the audit event for a request needs.
type auditRecord struct {
	// action and target are set by authorize (or a public handler).
	action string
	target authz.Target
	denied bool
	// userID is the actor for public operations (login), which have no
	// principal.
	userID string
	// read asks for a read to be recorded even when it is allowed (see
	// auditRead).
	read bool
	// details are the event's extra facts (see setAuditDetails).
	details map[string]string
}

type auditKey struct{}

func auditFromContext(ctx context.Context) *auditRecord {
	rec, _ := ctx.Value(auditKey{}).(*auditRecord)
	return rec
}

// noteCheck records the authorization check in the request's audit record.
func noteCheck(ctx context.Context, action authz.Action, target authz.Target, denied bool) {
	if rec := auditFromContext(ctx); rec != nil {
		rec.action, rec.target, rec.denied = string(action), target, denied
	}
}

// noteLogin asks for a login to be recorded, with the user who logged in,
// or none for a failed attempt.
func noteLogin(ctx context.Context, userID string) {
	if rec := auditFromContext(ctx); rec != nil {
		rec.action, rec.userID = auditActionLogin, userID
		if userID != "" {
			rec.target = authz.User(userID)
		}
	}
}

// auditRead asks for a read to be recorded like a change, whatever its
// result, for reads that hand out sensitive data, such as a database
// export's download. Call it before authorize.
func auditRead(ctx context.Context) {
	if rec := auditFromContext(ctx); rec != nil {
		rec.read = true
	}
}

// setAuditTarget replaces the event's target, for handlers that create
// the target and only know its ID afterwards.
func setAuditTarget(ctx context.Context, t authz.Target) {
	if rec := auditFromContext(ctx); rec != nil {
		rec.target = t
	}
}

// setAuditDetails adds extra facts to the event, such as the ID of the
// database session a request opened. They must never hold secrets.
func setAuditDetails(ctx context.Context, details map[string]string) {
	if rec := auditFromContext(ctx); rec != nil {
		if rec.details == nil {
			rec.details = map[string]string{}
		}
		maps.Copy(rec.details, details)
	}
}

// statusRecorder captures the response status.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// auditRequests writes the audit event for each request that needs one.
// It runs after authentication, so the principal is in the context. With
// no database, nothing is recorded.
func auditRequests(log *slog.Logger, db *store.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &auditRecord{}
			sw := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), auditKey{}, rec)))
			if db == nil {
				return
			}
			e, ok := auditEvent(r, rec, sw.status)
			if !ok {
				return
			}
			// The client may have gone; the event must still be written.
			ctx := context.WithoutCancel(r.Context())
			if err := store.NewAuditEvents(db).Insert(ctx, &e); err != nil {
				httpserver.LoggerFromContext(r.Context(), log).Error("writing audit event failed",
					"action", e.Action, "result", string(e.Result), "error", err)
			}
		})
	}
}

// auditEvent decides whether a request is recorded, and builds its event.
func auditEvent(r *http.Request, rec *auditRecord, status int) (store.AuditEvent, bool) {
	p, authed := auth.PrincipalFromContext(r.Context())
	var result store.AuditResult
	switch {
	case rec.denied:
		result = store.AuditDenied
	case safeMethod(r.Method) && !rec.read:
		return store.AuditEvent{}, false
	case !authed && rec.action == "":
		// Nothing to attribute: a public operation that didn't ask to be
		// recorded.
		return store.AuditEvent{}, false
	case status < http.StatusBadRequest: // 0: nothing written, an implicit 200
		result = store.AuditSuccess
	default:
		result = store.AuditFailure
	}
	action := rec.action
	if action == "" {
		if op, ok := OperationFromContext(r.Context()); ok {
			action = string(op.Action)
			if action == "" {
				action = op.ID
			}
		}
	}
	if action == "" {
		return store.AuditEvent{}, false
	}
	e := store.AuditEvent{
		ActorUserID:  p.UserID,
		ActorTokenID: p.TokenID,
		Action:       action,
		TargetType:   string(rec.target.Type),
		TargetID:     rec.target.ID,
		Result:       result,
		SourceIP:     clientFromContext(r.Context()).IP,
		Details:      rec.details,
	}
	if !authed {
		e.ActorUserID = rec.userID
	}
	return e, true
}

// ListAuditEvents lists audit events, newest first. The cursor is the ID of
// the last event on the previous page.
func (h *Handlers) ListAuditEvents(ctx context.Context, req gen.ListAuditEventsRequestObject) (gen.ListAuditEventsResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewAuditLog, authz.Platform()); err != nil {
		return nil, err
	}
	if h.db == nil {
		return nil, errNoDatabase
	}
	p := req.Params
	f := store.AuditFilter{
		ActorUserID: deref(p.ActorUserID),
		Action:      deref(p.Action),
		TargetType:  deref(p.TargetType),
		TargetID:    deref(p.TargetID),
		Since:       deref(p.Since),
		Until:       deref(p.Until),
		Limit:       deref(p.Limit),
	}
	if p.Cursor != nil {
		if !id.Valid(*p.Cursor) {
			return nil, httpserver.NewProblem(http.StatusBadRequest, "The cursor is invalid; pass the next_cursor of a previous page.")
		}
		f.Before = *p.Cursor
	}
	page, err := store.NewAuditEvents(h.db).List(ctx, f)
	if err != nil {
		return nil, err
	}
	out := gen.ListAuditEvents200JSONResponse{Items: make([]gen.AuditEvent, len(page.Events))}
	for i, e := range page.Events {
		out.Items[i] = gen.AuditEvent{
			ID:           e.ID,
			OccurredAt:   e.OccurredAt,
			ActorUserID:  optional(e.ActorUserID),
			ActorTokenID: optional(e.ActorTokenID),
			Action:       e.Action,
			TargetType:   optional(e.TargetType),
			TargetID:     optional(e.TargetID),
			Result:       gen.AuditResult(e.Result),
			SourceIP:     e.SourceIP,
		}
		if len(e.Details) > 0 {
			out.Items[i].Details = &e.Details
		}
	}
	if page.NextCursor != "" {
		out.NextCursor = &page.NextCursor
	}
	return out, nil
}

// optional returns nil for an empty string, for nullable response fields.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
