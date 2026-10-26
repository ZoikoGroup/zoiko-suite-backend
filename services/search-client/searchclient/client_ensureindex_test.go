package searchclient_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"zoiko.io/search-client/searchclient"
)

// EnsureIndex must distinguish "the index does not exist" from every other
// failure by status code, not by searching the error text. opensearch-go
// returns a response and an error for a 404, so reading only the error loses
// the one reliable signal.
func TestEnsureIndex_CreatesOnlyOnNotFound(t *testing.T) {
	tests := []struct {
		name       string
		headStatus int
		wantCreate bool
		wantErr    bool
	}{
		{name: "missing index creates", headStatus: http.StatusNotFound, wantCreate: true},
		{name: "existing index is left alone", headStatus: http.StatusOK},
		{name: "cluster error is not a missing index", headStatus: http.StatusInternalServerError, wantErr: true},
		{name: "forbidden is not a missing index", headStatus: http.StatusForbidden, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var heads, creates atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodHead:
					heads.Add(1)
					w.WriteHeader(tt.headStatus)
				case http.MethodPut:
					creates.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprint(w, `{"acknowledged":true,"shards_acknowledged":true,"index":"zoiko-test"}`)
				default:
					t.Errorf("unexpected method %s", r.Method)
					w.WriteHeader(http.StatusMethodNotAllowed)
				}
			}))
			defer srv.Close()

			c, err := searchclient.New(searchclient.Config{Addresses: []string{srv.URL}})
			require.NoError(t, err)

			err = c.EnsureIndex(context.Background(), "zoiko-test")
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.EqualValues(t, 1, heads.Load(), "EnsureIndex must check existence exactly once")
			if tt.wantCreate {
				require.EqualValues(t, 1, creates.Load(), "missing index must be created exactly once")
			} else {
				require.Zero(t, creates.Load(), "no create request may be sent")
			}
		})
	}
}
