package immich

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

// The contract test checks every request this package sends against Immich's
// own description of its API (its OpenAPI file), for the Immich version the
// end-to-end tests run: the path and method exist, every query parameter and
// every field of the body is one Immich knows, and none is deprecated.
//
// A deprecated field still works until Immich removes it, which is when the
// optimizer would stop working: this test says so first. The end-to-end tests
// check the answers; this one only needs the description, so it runs with the
// unit tests, offline.
//
// The description is kept in a compact form in testdata (only what the test
// reads). To move to another Immich version, download its
// open-api/immich-openapi-specs.json and run
//
//	IO_IMMICH_SPEC=/path/to/immich-openapi-specs.json IO_IMMICH_VERSION=v3.5.0 \
//	  go test ./internal/adapter/immich -run TestImmichAPIContract
const contractFile = "testdata/immich-api.json"

// apiContract is the compact description.
type apiContract struct {
	Version    string                `json:"version"`
	Schemas    map[string]*shapeNode `json:"schemas"`
	Operations map[string]*operation `json:"operations"`
}

type operation struct {
	Query      []string   `json:"query,omitempty"`
	Deprecated bool       `json:"deprecated,omitempty"`
	BodyType   string     `json:"bodyType,omitempty"` // "json" or "multipart"
	Body       *shapeNode `json:"body,omitempty"`
}

// shapeNode is what a value may hold: named fields, the items of a list, or
// anything at all.
type shapeNode struct {
	Ref        string                `json:"ref,omitempty"`
	Deprecated bool                  `json:"deprecated,omitempty"`
	Props      map[string]*shapeNode `json:"props,omitempty"`
	Items      *shapeNode            `json:"items,omitempty"`
	Any        bool                  `json:"any,omitempty"`
}

// ---- building the compact form from Immich's OpenAPI file ----

type openAPISchema struct {
	Ref                  string                    `json:"$ref"`
	Type                 any                       `json:"type"`
	Deprecated           bool                      `json:"deprecated"`
	Properties           map[string]*openAPISchema `json:"properties"`
	Items                *openAPISchema            `json:"items"`
	AllOf                []*openAPISchema          `json:"allOf"`
	AnyOf                []*openAPISchema          `json:"anyOf"`
	OneOf                []*openAPISchema          `json:"oneOf"`
	AdditionalProperties any                       `json:"additionalProperties"`
}

func compact(s *openAPISchema) *shapeNode {
	if s == nil {
		return &shapeNode{Any: true}
	}
	if s.Ref != "" {
		return &shapeNode{Ref: strings.TrimPrefix(s.Ref, "#/components/schemas/"), Deprecated: s.Deprecated}
	}
	n := &shapeNode{Deprecated: s.Deprecated}
	merge := func(parts []*openAPISchema) {
		for _, p := range parts {
			c := compact(p)
			if c.Ref != "" || c.Any || c.Items != nil {
				// A union with a reference or a list: anything that one
				// of them takes is taken.
				if len(parts) == 1 && len(n.Props) == 0 {
					*n = *c
					n.Deprecated = n.Deprecated || s.Deprecated
					continue
				}
				n.Any = true
				continue
			}
			for k, v := range c.Props {
				if n.Props == nil {
					n.Props = map[string]*shapeNode{}
				}
				n.Props[k] = v
			}
		}
	}
	merge(s.AllOf)
	merge(s.AnyOf)
	merge(s.OneOf)
	if s.Items != nil {
		n.Items = compact(s.Items)
	}
	for k, v := range s.Properties {
		if n.Props == nil {
			n.Props = map[string]*shapeNode{}
		}
		n.Props[k] = compact(v)
	}
	if s.AdditionalProperties != nil && s.AdditionalProperties != false {
		n.Any = true
	}
	if n.Props == nil && n.Items == nil && n.Ref == "" && isObject(s.Type) {
		n.Any = true // an object without a description of its fields
	}
	return n
}

func isObject(t any) bool {
	switch v := t.(type) {
	case string:
		return v == "object"
	case []any:
		for _, x := range v {
			if x == "object" {
				return true
			}
		}
	}
	return false
}

func buildContract(t *testing.T, specPath, version string) *apiContract {
	t.Helper()
	raw, err := os.ReadFile(specPath) // #nosec G304 -- a path the developer gives
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]struct {
			Deprecated bool `json:"deprecated"`
			Parameters []struct {
				Name string `json:"name"`
				In   string `json:"in"`
			} `json:"parameters"`
			RequestBody struct {
				Content map[string]struct {
					Schema *openAPISchema `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]*openAPISchema `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	c := &apiContract{Version: version, Schemas: map[string]*shapeNode{}, Operations: map[string]*operation{}}
	for name, s := range spec.Components.Schemas {
		c.Schemas[name] = compact(s)
	}
	for path, methods := range spec.Paths {
		for method, op := range methods {
			o := &operation{Deprecated: op.Deprecated}
			for _, p := range op.Parameters {
				if p.In == "query" {
					o.Query = append(o.Query, p.Name)
				}
			}
			sort.Strings(o.Query)
			if b, ok := op.RequestBody.Content["application/json"]; ok {
				o.BodyType, o.Body = "json", compact(b.Schema)
			} else if b, ok := op.RequestBody.Content["multipart/form-data"]; ok {
				o.BodyType, o.Body = "multipart", compact(b.Schema)
			}
			c.Operations[strings.ToUpper(method)+" "+path] = o
		}
	}
	return c
}

// ---- checking requests against it ----

func (c *apiContract) resolve(n *shapeNode) *shapeNode {
	for i := 0; n != nil && n.Ref != "" && i < 20; i++ {
		dep := n.Deprecated
		n = c.Schemas[n.Ref]
		if n != nil && dep {
			cp := *n
			cp.Deprecated = true
			n = &cp
		}
	}
	return n
}

// find returns the operation for a request, by matching {parameters} in
// Immich's paths against any one path segment. A literal segment wins over a
// parameter: /tags/assets is not /tags/{id}.
func (c *apiContract) find(method, path string) (string, *operation) {
	segs := strings.Split(path, "/")
	bestKey, best, bestParams := "", (*operation)(nil), -1
	for key, op := range c.Operations {
		m, p, _ := strings.Cut(key, " ")
		if m != method {
			continue
		}
		want := strings.Split(p, "/")
		if len(want) != len(segs) {
			continue
		}
		ok, params := true, 0
		for i := range want {
			if want[i] == segs[i] {
				continue
			}
			if strings.HasPrefix(want[i], "{") && segs[i] != "" {
				params++
				continue
			}
			ok = false
			break
		}
		if ok && (best == nil || params < bestParams) {
			bestKey, best, bestParams = key, op, params
		}
	}
	return bestKey, best
}

// checkValue reports every field of v that n does not have, or has as
// deprecated.
func (c *apiContract) checkValue(where string, v any, n *shapeNode, problems *[]string) {
	n = c.resolve(n)
	if n == nil || n.Any {
		return
	}
	switch x := v.(type) {
	case map[string]any:
		if n.Props == nil {
			return // an object where Immich describes no fields: a free-form value
		}
		for k, val := range x {
			field := n.Props[k]
			if field == nil {
				*problems = append(*problems, fmt.Sprintf("%s.%s is not a field Immich knows", where, k))
				continue
			}
			if c.resolveDeprecated(field) {
				*problems = append(*problems, fmt.Sprintf("%s.%s is deprecated in Immich", where, k))
			}
			c.checkValue(where+"."+k, val, field, problems)
		}
	case []any:
		if n.Items != nil {
			for _, it := range x {
				c.checkValue(where+"[]", it, n.Items, problems)
			}
		}
	}
}

func (c *apiContract) resolveDeprecated(n *shapeNode) bool {
	if n.Deprecated {
		return true
	}
	if r := c.resolve(n); r != nil {
		return r.Deprecated && n.Ref == ""
	}
	return false
}

func (c *apiContract) checkRequest(r *http.Request, body []byte) []string {
	path := strings.TrimPrefix(r.URL.Path, "/api")
	key, op := c.find(r.Method, path)
	if op == nil {
		return []string{fmt.Sprintf("%s %s: Immich %s has no such operation", r.Method, path, c.Version)}
	}
	var problems []string
	if op.Deprecated {
		problems = append(problems, key+" is deprecated in Immich")
	}
	for q := range r.URL.Query() {
		found := false
		for _, known := range op.Query {
			found = found || known == q
		}
		if !found {
			problems = append(problems, fmt.Sprintf("%s: query parameter %q is not one Immich knows", key, q))
		}
	}
	if len(body) == 0 {
		return problems
	}
	ct, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch {
	case op.Body == nil:
		problems = append(problems, key+": Immich takes no body here")
	case ct == "application/json":
		if op.BodyType != "json" {
			problems = append(problems, key+": Immich wants "+op.BodyType)
			break
		}
		var v any
		if err := json.Unmarshal(body, &v); err != nil {
			problems = append(problems, key+": the body is not JSON")
			break
		}
		c.checkValue(key, v, op.Body, &problems)
	case ct == "multipart/form-data":
		if op.BodyType != "multipart" {
			problems = append(problems, key+": Immich wants "+op.BodyType)
			break
		}
		mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		fields := map[string]any{}
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			fields[p.FormName()] = nil
		}
		c.checkValue(key, fields, op.Body, &problems)
	}
	return problems
}

func loadContract(t *testing.T) *apiContract {
	t.Helper()
	if spec := os.Getenv("IO_IMMICH_SPEC"); spec != "" {
		version := os.Getenv("IO_IMMICH_VERSION")
		if version == "" {
			t.Fatal("set IO_IMMICH_VERSION to the Immich version of the file in IO_IMMICH_SPEC")
		}
		c := buildContract(t, spec, version)
		out, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(contractFile), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(contractFile, out, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s for Immich %s (%d operations)", contractFile, version, len(c.Operations))
		return c
	}
	raw, err := os.ReadFile(contractFile)
	if err != nil {
		t.Fatalf("%v (see the comment at the top of this file)", err)
	}
	var c apiContract
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// contractServer answers every request with an empty answer of the right
// kind, after checking it. What the client then does with the answer does not
// matter here.
type contractServer struct {
	c        *apiContract
	mu       sync.Mutex
	seen     map[string]bool
	problems []string
}

func (s *contractServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	key, _ := s.c.find(r.Method, strings.TrimPrefix(r.URL.Path, "/api"))
	s.mu.Lock()
	s.problems = append(s.problems, s.c.checkRequest(r, body)...)
	s.seen[key] = true
	s.mu.Unlock()
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/original"):
		_, _ = io.WriteString(w, "file")
	case r.Method == http.MethodGet && (path == "/api/tags" || path == "/api/albums" || path == "/api/faces" ||
		path == "/api/shared-links" || path == "/api/memories" || strings.HasSuffix(path, "/metadata") || strings.HasSuffix(path, "/edits")),
		r.Method == http.MethodPut && (path == "/api/tags" || strings.HasSuffix(path, "/assets")):
		_, _ = io.WriteString(w, "[]")
	default:
		_, _ = io.WriteString(w, "{}")
	}
}

func TestImmichAPIContract(t *testing.T) {
	c := loadContract(t)
	srv := &contractServer{c: c, seen: map[string]bool{}}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	ctx := context.Background()

	lc := NewLibraryClient(ts.URL, "key", ts.Client())
	const id, other = "2f908a71-185e-4fb6-960c-278a8d87e66b", "3f908a71-185e-4fb6-960c-278a8d87e66b"
	str, num, yes := "x", 4.0, true
	dir := t.TempDir()
	file := filepath.Join(dir, "a.jxl")
	if err := os.WriteFile(file, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := json.RawMessage(`{"v":1}`)
	when := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)

	// Every call the optimizer makes. Errors from decoding the empty
	// answers are of no interest here.
	_, _ = lc.CurrentUser(ctx)
	_, _ = lc.AssetsCreatedSince(ctx, time.Time{})
	_, _ = lc.AssetsCreatedSince(ctx, when)
	_, _ = lc.Tags(ctx)
	_, _ = lc.UpsertTags(ctx, []string{"immich-optimizer/convert"})
	_, _ = lc.TaggedAssetIDs(ctx, id)
	_ = lc.DeleteTag(ctx, id)
	_, _ = lc.TagAssets(ctx, id, []string{other})
	_, _ = lc.UntagAssets(ctx, id, []string{other})
	_, _ = lc.GetAsset(ctx, id)
	_, _ = lc.GetMetadata(ctx, id)
	_ = lc.PutMetadata(ctx, id, []entity.MetadataItem{{Key: entity.MarkerKey, Value: marker}})
	_ = lc.DeleteMetadata(ctx, id, entity.MarkerKey)
	_, _ = lc.DownloadOriginal(ctx, id, filepath.Join(dir, "dl"))
	_, _ = lc.Upload(ctx, entity.LibraryUpload{Path: file, Filename: "a.jxl", FileCreatedAt: when, FileModifiedAt: when,
		IsFavorite: true, Visibility: "timeline", Metadata: []entity.MetadataItem{{Key: entity.MarkerKey, Value: marker}}})
	_ = lc.CopyAsset(ctx, id, other)
	_ = lc.UpdateAsset(ctx, id, entity.LibraryAssetUpdate{Description: &str, Rating: &num, DateTimeOriginal: &str,
		Latitude: &num, Longitude: &num, IsFavorite: &yes, Visibility: &str, LivePhotoVideoID: &str})
	_, _ = lc.Albums(ctx, id)
	_, _ = lc.ActivityCount(ctx, other, id)
	_ = lc.TagAsset(ctx, id, []string{other})
	_, _ = lc.Faces(ctx, id)
	_ = lc.CreateFace(ctx, id, entity.LibraryFace{PersonID: other, X1: 1, Y1: 2, X2: 3, Y2: 4, ImageWidth: 10, ImageHeight: 10})
	_, _ = lc.SharedLinkIDs(ctx, id)
	_, _ = lc.MemoryIDs(ctx)
	_ = lc.AddToMemory(ctx, other, id)
	_ = lc.AddToSharedLink(ctx, other, id)
	_ = lc.SetStackPrimary(ctx, other, id)
	_, _ = lc.Edits(ctx, id)
	_ = lc.SetEdits(ctx, id, []entity.LibraryEdit{{Action: "rotate", Parameters: json.RawMessage(`{"angle":90}`)}})
	_ = lc.Trash(ctx, []string{id})
	_ = lc.Delete(ctx, []string{id})

	// The marker, read and written with a person's own credentials (proxy).
	auth := http.Header{"X-Api-Key": []string{"key"}}
	_, _ = GetMarker(ctx, ts.Client(), ts.URL, auth, id)
	_ = EnsureMarker(ctx, ts.Client(), ts.URL, auth, id, &entity.Marker{V: 1, SHA1: "aa"}, true)

	// Watcher mode's upload.
	up := NewClient(ts.URL, "key", ts.Client(), nil)
	_, _ = up.UploadAsset(ctx, entity.Upload{Path: file, Filename: "a.jxl", ModTime: when, Marker: &entity.Marker{V: 1, SHA1: "aa"}})

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.seen) < 25 {
		t.Errorf("only %d different operations were called: is the test still calling everything?", len(srv.seen))
	}
	sort.Strings(srv.problems)
	for _, p := range dedupe(srv.problems) {
		t.Errorf("against Immich %s: %s", c.Version, p)
	}
}

// The checker itself: it must notice what it is there for.
func TestImmichAPIContractNoticesProblems(t *testing.T) {
	c := loadContract(t)
	req := func(method, target, body string) []string {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		return c.checkRequest(r, []byte(body))
	}
	cases := map[string][]string{
		"unknown path":       req(http.MethodGet, "/api/no/such/thing", ""),
		"deprecated field":   req(http.MethodPost, "/api/search/metadata", `{"page":1}`),
		"unknown field":      req(http.MethodPost, "/api/search/metadata", `{"filter":{"nope":1}}`),
		"unknown nested":     req(http.MethodPost, "/api/search/metadata", `{"filter":{"createdAt":{"gte":"x","nope":1}}}`),
		"unknown query":      req(http.MethodGet, "/api/albums?nope=1", ""),
		"body where no body": req(http.MethodGet, "/api/users/me", `{"a":1}`),
	}
	for name, problems := range cases {
		if len(problems) == 0 {
			t.Errorf("%s: not noticed", name)
		}
	}
	if p := req(http.MethodPost, "/api/search/metadata", `{"filter":{"createdAt":{"gte":"x"}},"cursor":"c","size":1}`); len(p) != 0 {
		t.Errorf("a valid request was refused: %v", p)
	}
}

func dedupe(sorted []string) []string {
	out := sorted[:0]
	for i, v := range sorted {
		if i == 0 || v != sorted[i-1] {
			out = append(out, v)
		}
	}
	return out
}
