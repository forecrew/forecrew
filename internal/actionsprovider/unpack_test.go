package actionsprovider

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

type entry struct {
	name string
	body string
	mode fs.FileMode // 0 is a regular file
}

func makeZip(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.mode != 0 {
			h.SetMode(e.mode)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUnpackRefusals(t *testing.T) {
	data := makeZip(t,
		entry{name: "ok.txt", body: "ok"},
		entry{name: "dir/", mode: fs.ModeDir | 0o755},
		entry{name: "dir/nested.txt", body: "n"},
		entry{name: "link", body: "/etc/passwd", mode: fs.ModeSymlink | 0o777},
		entry{name: "../escape.txt", body: "x"},
		entry{name: "/abs.txt", body: "x"},
		entry{name: "a/../b.txt", body: "x"},
		entry{name: `back\slash.txt`, body: "x"},
		entry{name: ".hidden", body: "x"},
		entry{name: "ok.txt", body: "again"},
		entry{name: "big.bin", body: strings.Repeat("x", 100)},
	)
	got, err := unpack(data, 50, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range got.Files {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "ok.txt,dir/nested.txt" {
		t.Errorf("files = %v", names)
	}
	want := []string{"link: not a regular file", "../escape.txt: not a valid artifact name", "/abs.txt: not a valid artifact name",
		"a/../b.txt: not a valid artifact name", `back\slash.txt: not a valid artifact name`, ".hidden: not a valid artifact name",
		"ok.txt: duplicate", "big.bin: over the artifact size limit"}
	if strings.Join(got.Skipped, "|") != strings.Join(want, "|") {
		t.Errorf("skipped = %q\nwant      %q", got.Skipped, want)
	}
}

func TestUnpackFileCount(t *testing.T) {
	var es []entry
	for i := range MaxFiles + 5 {
		es = append(es, entry{name: fmt.Sprintf("f%03d.txt", i), body: "x"})
	}
	got, err := unpack(makeZip(t, es...), 10, 1<<20)
	if err != nil || len(got.Files) != MaxFiles || len(got.Skipped) != 5 || !strings.Contains(got.Skipped[0], "more than 200 files") {
		t.Errorf("files %d skipped %d, %v", len(got.Files), len(got.Skipped), err)
	}
}

func TestUnpackBudget(t *testing.T) {
	chunk := strings.Repeat("0", 400)
	data := makeZip(t, entry{name: "a", body: chunk}, entry{name: "b", body: chunk}, entry{name: "c", body: chunk})
	if _, err := unpack(data, 500, 1000); !errors.Is(err, errUnpack) || !strings.Contains(err.Error(), "unpacks to more than 1000 bytes") {
		t.Errorf("err = %v, want the budget refusal", err)
	}
}

// An entry that says it is small but is not is cut at the limit.
func TestUnpackLyingHeader(t *testing.T) {
	data := makeZip(t, entry{name: "a.txt", body: strings.Repeat("x", 1000)}, entry{name: "b.txt", body: "ok"})
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readEntry(zr.File[0], 10); !errors.Is(err, errTooBig) {
		t.Errorf("err = %v, want errTooBig", err)
	}
	if b, err := readEntry(zr.File[1], 10); err != nil || string(b) != "ok" {
		t.Errorf("read = %q, %v", b, err)
	}
}

func TestUnpackNotAZip(t *testing.T) {
	if _, err := unpack([]byte("nope"), 10, 10); !errors.Is(err, errUnpack) {
		t.Errorf("err = %v", err)
	}
}

func TestJoinLimited(t *testing.T) {
	if got := joinLimited([]string{"a", "b"}); got != "a, b" {
		t.Errorf("got %q", got)
	}
	var many []string
	for range 13 {
		many = append(many, "x")
	}
	if got := joinLimited(many); !strings.HasSuffix(got, ", and 3 more") {
		t.Errorf("got %q", got)
	}
}
