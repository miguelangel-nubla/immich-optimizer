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
	stored = `{"v":1,"versions":[{"sha1":"bb","size":1},{"size":1,"task":{"name":"old"}}],"library":{"replaces":"x"}}`
	ensure(false)
	if len(puts) != 1 {
		t.Fatalf("an existing marker was overwritten: %q", puts)
	}
	// ...and with refresh it is written again exactly as it is.
	ensure(true)
	if len(puts) != 2 || !strings.Contains(puts[1], stored) || strings.Contains(puts[1], "new") {
		t.Fatalf("refresh must write the existing marker back unchanged: %q", puts)
	}

	// A record the optimizer cannot read (here the shape written before
	// versions) is replaced by the new one, with or without refresh.
	for _, refresh := range []bool{false, true} {
		stored = `{"v":1,"original":{"sha1":"bb","size":1},"conversion":{"task":{"name":"old"}}}`
		n := len(puts)
		ensure(refresh)
		if len(puts) != n+1 || !strings.Contains(puts[n], `"name":"new"`) || strings.Contains(puts[n], `"original"`) {
			t.Fatalf("refresh %v: an unreadable marker was not replaced: %q", refresh, puts[n:])
		}
	}
	stored = `{"v":1,"versions":[{"sha1":"bb","size":1},{"size":1,"task":{"name":"old"}}],"library":{"replaces":"x"}}`

	got, err := GetMarker(context.Background(), srv.Client(), srv.URL, auth, "id")
	if err != nil || got == nil || got.SHA1 != "bb" {
		t.Fatalf("GetMarker = %+v, %v", got, err)
	}
	stored = ""
	if got, err := GetMarker(context.Background(), srv.Client(), srv.URL, auth, "id"); err != nil || got != nil {
		t.Fatalf("GetMarker without a marker = %+v, %v", got, err)
	}
}

// An answer that says nothing about the marker (Immich in trouble) must not
// be taken for "no marker": writing a new one would lose the existing one.
func TestEnsureMarkerDoesNotWriteWhenTheReadFails(t *testing.T) {
	puts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts++
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	err := EnsureMarker(context.Background(), srv.Client(), srv.URL, http.Header{}, "id", &entity.Marker{V: 1, SHA1: "aa"}, true)
	if err == nil || puts != 0 {
		t.Fatalf("err = %v, %d write(s)", err, puts)
	}
}
