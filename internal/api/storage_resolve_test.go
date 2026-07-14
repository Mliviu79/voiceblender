package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/storage"
)

// The per-request S3 backend must inherit the operator's insecure-endpoint
// decision from server config — a caller must not be able to downgrade the
// transport, and an operator who has opted in must not be blocked. Both call
// sites build storage.S3Config as a named-field literal, so dropping the field
// still compiles: this test is what catches that.
func TestResolveStorage_AllowInsecure(t *testing.T) {
	// httptest serves http://127.0.0.1:..., i.e. a genuinely plaintext
	// endpoint, which is exactly the condition under test.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && r.URL.Path == "/test-bucket" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	req := RecordRequest{
		Storage:     "s3",
		S3Bucket:    "test-bucket",
		S3Endpoint:  srv.URL,
		S3AccessKey: "key",
		S3SecretKey: "secret",
	}

	t.Run("operator opted in", func(t *testing.T) {
		s := &Server{Config: config.Config{S3AllowInsecureEndpoint: true}}

		backend, err := s.resolveStorage(req)
		if errors.Is(err, storage.ErrInsecureEndpoint) {
			t.Fatalf("S3_ALLOW_INSECURE_ENDPOINT=true must reach the endpoint, got %v", err)
		}
		if err != nil {
			t.Fatalf("expected the backend to be created, got %v", err)
		}
		if backend == nil {
			t.Fatal("expected a non-nil backend")
		}
	})

	t.Run("insecure endpoint refused by default", func(t *testing.T) {
		s := &Server{Config: config.Config{S3AllowInsecureEndpoint: false}}

		_, err := s.resolveStorage(req)
		if !errors.Is(err, storage.ErrInsecureEndpoint) {
			t.Fatalf("expected ErrInsecureEndpoint (surfaced to the caller as 400), got %v", err)
		}
	})
}
