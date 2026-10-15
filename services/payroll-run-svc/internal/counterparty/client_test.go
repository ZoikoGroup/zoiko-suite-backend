package counterparty

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
)

func serve(t *testing.T, status int, body string, seen *http.Header) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = r.Header.Clone()
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, nil)
}

func TestValidateAuthority(t *testing.T) {
	var hdr http.Header
	c := serve(t, 200, `{"counterparty_id":"cp-1","legal_entity_id":"le-1","status":"ACTIVE"}`, &hdr)
	require.NoError(t, c.ValidateAuthority(context.Background(), "t-1", "le-1", "cp-1"))
	assert.Equal(t, "t-1", hdr.Get("X-Tenant-Id"))
	assert.Equal(t, "le-1", hdr.Get("X-Legal-Entity-Id"))

	for name, tc := range map[string]struct {
		status int
		body   string
		want   error
	}{
		"not found":         {404, ``, domain.ErrAuthorityInvalid},
		"other entity":      {200, `{"counterparty_id":"cp-1","legal_entity_id":"le-2","status":"ACTIVE"}`, domain.ErrAuthorityInvalid},
		"suspended":         {200, `{"counterparty_id":"cp-1","legal_entity_id":"le-1","status":"SUSPENDED"}`, domain.ErrAuthorityInvalid},
		"different party":   {200, `{"counterparty_id":"cp-9","legal_entity_id":"le-1","status":"ACTIVE"}`, domain.ErrAuthorityInvalid},
		"server error":      {500, ``, domain.ErrAuthorityUnavailable},
		"forbidden":         {403, ``, domain.ErrAuthorityUnavailable},
		"unreadable answer": {200, `<html>`, domain.ErrAuthorityUnavailable},
	} {
		err := serve(t, tc.status, tc.body, nil).ValidateAuthority(context.Background(), "t-1", "le-1", "cp-1")
		assert.ErrorIs(t, err, tc.want, name)
	}
}

func TestUnreachableIsUnknownNotValid(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	err := NewClient(url, nil).ValidateAuthority(context.Background(), "t", "le-1", "cp-1")
	assert.ErrorIs(t, err, domain.ErrAuthorityUnavailable)
}
