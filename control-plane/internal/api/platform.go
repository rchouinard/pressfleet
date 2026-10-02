package api

import (
	"context"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
)

// GetPlatform reports the install's connection details (#123). Every role
// that may see sites may read it: it only tells them where to connect.
func (h *Handlers) GetPlatform(ctx context.Context, _ gen.GetPlatformRequestObject) (gen.GetPlatformResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewSites, authz.Platform()); err != nil {
		return nil, err
	}
	return gen.GetPlatform200JSONResponse{Sftp: gen.PlatformSftp{
		Enabled: h.sftp.Enabled,
		Host:    h.sftp.Host,
		Port:    h.sftp.Port,
	}}, nil
}
