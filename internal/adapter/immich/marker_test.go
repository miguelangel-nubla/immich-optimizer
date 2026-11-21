package immich

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

func TestEnsureMarkerWritesOnlyWhenNeeded(t *testing.T) {
	stored := ""
	var puts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet:
			if stored == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"key":"immich-optimizer","value":`+stored+`,"updatedAt":"x"}`)
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			puts = append(puts, string(b))
			_, _ = io.WriteString(w, `[]`)
		}
	}))
	defer srv.Close()
	auth := http.Header{"X-Api-Key": []string{"k"}}
	m := &entity.Marker{V: 1, SHA1: "aa", Task: "new"}
	ensure := func(refresh bool) {
		t.Helper()
		if err := EnsureMarker(context.Background(), srv.Client(), srv.URL, auth, "id", m, refresh); err != nil {
			t.Fatal(err)
		}
	}

	ensure(false)
	if len(puts) != 1 || !strings.Contains(puts[0], `"name":"new"`) {
		t.Fatalf("an asset without a marker must get it: %q", puts)
	}

	// An existing marker is left alone...
	stored = `{"v":1,"original":{"sha1":"bb","size":1},"conversion":{"task":{"name":"old"}},"library":{"replaces":"x"}}`
	ensure(false)
	if len(puts) != 1 {
		t.Fatalf("an existing marker was overwritten: %q", puts)
	}
	// ...and with refresh it is written again exactly as it is.
	ensure(true)
	if len(puts) != 2 || !strings.Contains(puts[1], stored) || strings.Contains(puts[1], "new") {
		t.Fatalf("refresh must write the existing marker back unchanged: %q", puts)
	}

	got, err := GetMarker(context.Background(), srv.Client(), srv.URL, auth, "id")
	if err != nil || got == nil || got.SHA1 != "bb" {
		t.Fatalf("GetMarker = %+v, %v", got, err)
	}
	stored = ""
	if got, err := GetMarker(context.Background(), srv.Client(), srv.URL, auth, "id"); err != nil || got != nil {
		t.Fatalf("GetMarker without a marker = %+v, %v", got, err)
	}
}
