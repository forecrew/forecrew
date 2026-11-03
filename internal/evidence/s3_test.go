package evidence

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
)

// fakeS3 serves the three path-style calls S3 uses: PUT, GET and DELETE of
// /<bucket>/<key>. Over plain HTTP the client signs the body in chunks
// (aws-chunked), which put unwraps.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	types   map[string]string
	fail    int // status to answer every request with, when set
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string][]byte{}, types: map[string]string{}} }

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
	if got := string(srv.objects["bkt/"+key]); got != "<xml/>" {
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
	if err := s.Put(t.Context(), "k", strings.NewReader("x"), -1, ""); !errors.Is(err, ErrUnknownSize) || len(srv.objects) != 0 {
		t.Fatalf("err = %v, objects %d", err, len(srv.objects))
	}
}

func TestS3Errors(t *testing.T) {
	srv := newFakeS3()
	s := newTestS3(t, srv)
	ctx := t.Context()
	srv.fail = http.StatusForbidden // not retried by the client
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
