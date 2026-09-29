package e2e_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/uploads"
)

// cutShort sends some bytes of a chunk, then fails, as when a client's
// connection drops mid-request.
type cutShort struct {
	r io.Reader
}

func (c *cutShort) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if errors.Is(err, io.EOF) {
		return n, errors.New("connection dropped")
	}
	return n, err
}

// TestUploads checks the resumable upload API (#111) against a real
// control plane, through its HTTP server's body limits and timeouts:
//
//   - a 40 MiB upload sent as a chunk of the largest size (32 MiB) and a
//     chunk that is cut short mid-request, which counts for nothing; the
//     upload resumes from the end of the first chunk and completes with
//     its SHA-256 checksum verified;
//   - the file sits in <data_dir>/uploads/ with the uploaded size;
//   - a chunk over the chunk limit is refused with 413;
//   - a cancelled upload, and one whose content doesn't match its
//     checksum, leave no file behind.
//
// Expiry and removal after the consuming job are covered by the uploads
// package's tests with a fake clock.
func TestUploads(t *testing.T) {
	p := e2e.Start(t)
	ctx := t.Context()
	groupID := createGroup(t, p, "E2E Upload Agency")
	uploadsDir := config.Layout{Root: p.DataDir}.UploadsDir()

	data := make([]byte, 40<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	sumHex := hex.EncodeToString(sum[:])
	create := func(size int, sha256 *string) client.Upload {
		t.Helper()
		resp, err := p.Client.CreateUploadWithResponse(ctx, client.CreateUploadJSONRequestBody{
			Purpose: client.SiteImport, ClientGroupID: &groupID, SizeBytes: int64(size), Sha256: sha256,
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.JSON201 == nil {
			t.Fatalf("create upload: status %d: %s", resp.StatusCode(), resp.Body)
		}
		return *resp.JSON201
	}
	chunk := func(uploadID string, offset int, body io.Reader) (*client.AppendUploadChunkResponse, error) {
		return p.Client.AppendUploadChunkWithBodyWithResponse(ctx, uploadID, &client.AppendUploadChunkParams{Offset: int64(offset)},
			"application/octet-stream", body)
	}
	get := func(uploadID string) (client.Upload, int) {
		t.Helper()
		resp, err := p.Client.GetUploadWithResponse(ctx, uploadID)
		if err != nil {
			t.Fatal(err)
		}
		if resp.JSON200 == nil {
			return client.Upload{}, resp.StatusCode()
		}
		return *resp.JSON200, resp.StatusCode()
	}
	fileGone := func(uploadID string) bool {
		_, err := os.Stat(filepath.Join(uploadsDir, uploadID))
		return errors.Is(err, os.ErrNotExist)
	}

	u := create(len(data), &sumHex)
	if u.MaxChunkBytes != uploads.MaxChunkBytes {
		t.Errorf("max_chunk_bytes = %d, want %d", u.MaxChunkBytes, uploads.MaxChunkBytes)
	}
	first := int(u.MaxChunkBytes)
	resp, err := chunk(u.ID, 0, bytes.NewReader(data[:first]))
	if err != nil {
		t.Fatal(err)
	}
	if resp.JSON200 == nil || resp.JSON200.ReceivedBytes != int64(first) {
		t.Fatalf("first chunk: status %d: %s", resp.StatusCode(), resp.Body)
	}

	// The connection drops halfway through the second chunk.
	if _, err := chunk(u.ID, first, &cutShort{r: bytes.NewReader(data[first : first+(len(data)-first)/2])}); err == nil {
		t.Fatal("a chunk cut short succeeded")
	}
	got, status := get(u.ID)
	if status != http.StatusOK || got.ReceivedBytes != int64(first) || got.Status != client.Uploading {
		t.Fatalf("after the interrupted chunk: status %d, %+v; want %d bytes received", status, got, first)
	}

	// Resume from the last chunk and complete.
	resp, err = chunk(u.ID, int(got.ReceivedBytes), bytes.NewReader(data[got.ReceivedBytes:]))
	if err != nil {
		t.Fatal(err)
	}
	if resp.JSON200 == nil || resp.JSON200.ReceivedBytes != int64(len(data)) {
		t.Fatalf("resumed chunk: status %d: %s", resp.StatusCode(), resp.Body)
	}
	done, err := p.Client.CompleteUploadWithResponse(ctx, u.ID, client.CompleteUploadJSONRequestBody{})
	if err != nil {
		t.Fatal(err)
	}
	if done.JSON200 == nil || done.JSON200.Status != client.Complete || done.JSON200.Sha256 == nil || *done.JSON200.Sha256 != sumHex {
		t.Fatalf("complete: status %d: %s", done.StatusCode(), done.Body)
	}
	if info, err := os.Stat(filepath.Join(uploadsDir, u.ID)); err != nil || info.Size() != int64(len(data)) {
		t.Errorf("uploaded file: %v, %v; want %d bytes", info, err, len(data))
	}

	// A chunk over the limit is refused.
	big := create(int(uploads.MaxChunkBytes)+1, nil)
	resp, err = chunk(big.ID, 0, bytes.NewReader(make([]byte, uploads.MaxChunkBytes+1)))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != http.StatusRequestEntityTooLarge {
		t.Errorf("chunk over the limit: status %d: %s", resp.StatusCode(), resp.Body)
	}

	// Cancelling removes the file.
	for _, id := range []string{u.ID, big.ID} {
		del, err := p.Client.DeleteUploadWithResponse(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if del.StatusCode() != http.StatusNoContent {
			t.Errorf("delete: status %d: %s", del.StatusCode(), del.Body)
		}
		if _, status := get(id); status != http.StatusNotFound || !fileGone(id) {
			t.Errorf("deleted upload: status %d, file gone %v", status, fileGone(id))
		}
	}

	// Content that doesn't match its checksum is discarded.
	other := sha256.Sum256([]byte("something else"))
	otherHex := hex.EncodeToString(other[:])
	bad := create(1024, &otherHex)
	if resp, err := chunk(bad.ID, 0, bytes.NewReader(data[:1024])); err != nil || resp.JSON200 == nil {
		t.Fatalf("chunk: %v", err)
	}
	done, err = p.Client.CompleteUploadWithResponse(ctx, bad.ID, client.CompleteUploadJSONRequestBody{})
	if err != nil {
		t.Fatal(err)
	}
	if done.StatusCode() != http.StatusUnprocessableEntity {
		t.Errorf("complete with a wrong checksum: status %d: %s", done.StatusCode(), done.Body)
	}
	if _, status := get(bad.ID); status != http.StatusNotFound || !fileGone(bad.ID) {
		t.Errorf("mismatched upload: status %d, file gone %v", status, fileGone(bad.ID))
	}
}
