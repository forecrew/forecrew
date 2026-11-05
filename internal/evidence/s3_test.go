package evidence

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
)

// fakeS3 serves the three path-style calls S3 uses: PUT, GET and DELETE of
// /<bucket>/<key>. Over plain HTTP the client signs the body in chunks
// (aws-chunked), which put unwraps.
type fakeS3 struct {
	mu       sync.Mutex
	objects  map[string][]byte
	types    map[string]string
	listFail bool
	pageMax  int // keys per listing page, 0 for no cap
	fail     int // status to answer every request with, when set
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string][]byte{}, types: map[string]string{}} }

// set changes the fake's state under its lock: a listing the client abandoned
// early can still be served in the background.
func (f *fakeS3) set(fn func(*fakeS3)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// object returns the stored body of key and how many objects are stored.
func (f *fakeS3) object(key string) (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.objects[key]), len(f.objects)
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.TrimPrefix(r.URL.Path, "/")
	if f.fail != 0 {
		w.WriteHeader(f.fail)
		return
	}
	switch r.Method {
	case http.MethodPut:
		body := r.Body
		var data []byte
		var err error
		if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
			data, err = unchunk(body)
		} else {
			data, err = io.ReadAll(body)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.objects[key], f.types[key] = data, r.Header.Get("Content-Type")
		w.Header().Set("ETag", `"abc"`)
	case http.MethodGet, http.MethodHead:
		if r.URL.Query().Get("list-type") == "2" {
			f.list(w, r)
			return
		}
		data, ok := f.objects[key]
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			if r.Method == http.MethodHead {
				return
			}
			_, _ = io.WriteString(w, `<?xml version="1.0"?><Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`)
			return
		}
		w.Header().Set("Content-Type", f.types[key])
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("ETag", `"abc"`)
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	case http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// list answers ListObjectsV2 with the keys of the bucket in order, honouring
// prefix, start-after, continuation-token and max-keys. A page holds at most
// pageMax keys (when set); a cut page is reported truncated, with the last
// key as continuation token.
func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	max, _ := strconv.Atoi(q.Get("max-keys"))
	var keys []string
	for k := range f.objects {
		k = strings.TrimPrefix(k, "bkt/")
		if strings.HasPrefix(k, q.Get("prefix")) && k > q.Get("start-after") {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	if tok := q.Get("continuation-token"); tok != "" {
		keys = slices.DeleteFunc(keys, func(k string) bool { return k <= tok })
	}
	if f.pageMax > 0 && (max == 0 || f.pageMax < max) {
		max = f.pageMax
	}
	truncated := max > 0 && len(keys) > max
	if truncated {
		keys = keys[:max]
	}
	if f.listFail {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><ListBucketResult><Name>bkt</Name><IsTruncated>` + strconv.FormatBool(truncated) + `</IsTruncated>`)
	if truncated {
		b.WriteString(`<NextContinuationToken>` + keys[len(keys)-1] + `</NextContinuationToken>`)
	}
	for _, k := range keys {
		fmt.Fprintf(&b, `<Contents><Key>%s</Key><LastModified>2026-01-02T03:04:05.000Z</LastModified><Size>%d</Size></Contents>`, k, len(f.objects["bkt/"+k]))
	}
	b.WriteString(`</ListBucketResult>`)
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, b.String())
}

// unchunk decodes an aws-chunked body: "<hex size>;chunk-signature=...\r\n<data>\r\n".
func unchunk(r io.Reader) ([]byte, error) {
	br := bufio.NewReader(r)
	var out bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, _, _ := strings.Cut(strings.TrimSpace(line), ";")
		n, err := strconv.ParseInt(size, 16, 64)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return out.Bytes(), nil
		}
		if _, err := io.CopyN(&out, br, n); err != nil {
			return nil, err
		}
		if _, err := br.Discard(2); err != nil {
			return nil, err
		}
	}
}

func newTestS3(t *testing.T, srv *fakeS3) *S3 {
	t.Helper()
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	s, err := NewS3(&config.Evidence{Endpoint: ts.URL, Bucket: "bkt", Region: "us-east-1", PathStyle: true, AccessKeyID: "AKID", SecretAccessKey: "SECRET"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestS3RoundTrip(t *testing.T) {
	srv := newFakeS3()
	s := newTestS3(t, srv)
	ctx := t.Context()
	key := JobKey(domain.NewID(), domain.NewID(), "reports/unit.xml")

	if err := s.Put(ctx, key, strings.NewReader("<xml/>"), 6, "text/xml"); err != nil {
		t.Fatal(err)
	}
	if got, _ := srv.object("bkt/" + key); got != "<xml/>" {
		t.Fatalf("stored %q", got)
	}
	rc, err := s.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(data) != "<xml/>" {
		t.Fatalf("get = %q", data)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("deleting twice: %v", err)
	}
}

func TestS3RefusesUnknownSize(t *testing.T) {
	srv := newFakeS3()
	s := newTestS3(t, srv)
	err := s.Put(t.Context(), "k", strings.NewReader("x"), -1, "")
	if _, n := srv.object(""); !errors.Is(err, ErrUnknownSize) || n != 0 {
		t.Fatalf("err = %v, objects %d", err, n)
	}
}

func TestS3Errors(t *testing.T) {
	srv := newFakeS3()
	s := newTestS3(t, srv)
	ctx := t.Context()
	srv.set(func(f *fakeS3) { f.fail = http.StatusForbidden }) // not retried by the client
	if err := s.Put(ctx, "k", strings.NewReader("x"), 1, ""); err == nil {
		t.Error("put: no error")
	}
	if _, err := s.Get(ctx, "k"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("get = %v, want a plain error", err)
	}
	if err := s.Delete(ctx, "k"); err == nil {
		t.Error("delete: no error")
	}
}

func TestNewS3(t *testing.T) {
	for _, endpoint := range []string{"", "://bad", "no-host"} {
		if _, err := NewS3(&config.Evidence{Endpoint: endpoint, Bucket: "b"}); err == nil {
			t.Errorf("endpoint %q: no error", endpoint)
		}
	}
	for _, c := range []config.Evidence{
		{Endpoint: "https://s3.example.com", Bucket: "b", Region: "eu-west-1"},
		{Endpoint: "http://garage:3900", Bucket: "b", PathStyle: true},
	} {
		if _, err := NewS3(&c); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
}

func TestJobKey(t *testing.T) {
	a, j := domain.NewID(), domain.NewID()
	want := "jobs/" + a.String() + "/" + j.String() + "/reports/unit.xml"
	if got := JobKey(a, j, "reports/unit.xml"); got != want {
		t.Errorf("JobKey = %q, want %q", got, want)
	}
}

func TestNewKey(t *testing.T) {
	a, j := domain.NewID(), domain.NewID()
	k1, k2 := NewKey(a, j, "reports/unit.xml"), NewKey(a, j, "reports/unit.xml")
	prefix := JobKey(a, j, "reports/unit.xml") + "~"
	if k1 == k2 || !strings.HasPrefix(k1, prefix) || !strings.HasPrefix(k2, prefix) {
		t.Errorf("keys = %q, %q", k1, k2)
	}
}

func TestS3List(t *testing.T) {
	srv := newFakeS3()
	s := newTestS3(t, srv)
	ctx := t.Context()
	for _, k := range []string{"jobs/a/1", "jobs/a/2", "jobs/b/1", "other/x"} {
		if err := s.Put(ctx, k, strings.NewReader("abc"), 3, ""); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.List(ctx, "jobs/", "", 2)
	if err != nil || len(got) != 2 || got[0].Key != "jobs/a/1" || got[1].Key != "jobs/a/2" || got[0].Size != 3 ||
		!got[0].LastModified.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("first page = %+v, %v", got, err)
	}
	got, err = s.List(ctx, "jobs/", "jobs/a/2", 10)
	if err != nil || len(got) != 1 || got[0].Key != "jobs/b/1" {
		t.Fatalf("second page = %+v, %v", got, err)
	}
	if got, err := s.List(ctx, "jobs/", "", 0); err != nil || len(got) != 0 {
		t.Fatalf("limit 0 = %+v, %v", got, err)
	}

	// The server cuts listings into pages of 2; List still honours its limit
	// and reads across pages.
	srv.set(func(f *fakeS3) { f.pageMax = 2 })
	got, err = s.List(ctx, "jobs/", "", 3)
	if err != nil || len(got) != 3 || got[2].Key != "jobs/b/1" {
		t.Fatalf("limit 3 over pages of 2 = %+v, %v", got, err)
	}
	if got, err := s.List(ctx, "", "", 10); err != nil || len(got) != 4 {
		t.Fatalf("all over pages of 2 = %+v, %v", got, err)
	}
	srv.set(func(f *fakeS3) { f.listFail = true })
	if _, err := s.List(ctx, "jobs/", "", 10); err == nil {
		t.Fatal("a failing listing was accepted")
	}
}
