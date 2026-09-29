package htmlfs

import (
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

var (
	older = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
)

func file(data string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(data), ModTime: older}
}

func read(t *testing.T, fsys fs.FS, name string) string {
	t.Helper()
	body, err := fs.ReadFile(fsys, name)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", name, err)
	}
	return string(body)
}

func TestRendersTemplateAndFragments(t *testing.T) {
	source := fstest.MapFS{
		"index.html":             file("![TEMPLATE default]\n<h1>Hello</h1>"),
		"templates/default.html": file("<html><head>$[IMPORTMAP]</head><body>$[header]$[page]</body></html>"),
		"fragments/header.html":  file("<nav>Header</nav>"),
	}
	fsys := New(source,
		Templates("templates"),
		Fragments("fragments"),
		Fragment("IMPORTMAP", func() string { return `<script type="importmap">{}</script>` }),
	)

	got := read(t, fsys, "index.html")
	want := `<html><head><script type="importmap">{}</script></head><body><nav>Header</nav><h1>Hello</h1></body></html>`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestWorksWithHTTPFileServer(t *testing.T) {
	source := fstest.MapFS{
		"index.html":             file("![TEMPLATE default]\n<h1>Hello</h1>"),
		"templates/default.html": file("<main>$[page]</main>"),
	}
	handler := http.FileServer(http.FS(New(source, Templates("templates"))))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.String() != "<main><h1>Hello</h1></main>" {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestPassesThroughNonHTMLFiles(t *testing.T) {
	source := fstest.MapFS{"js/app.js": file(`console.log("$[x]")`)}
	if got := read(t, New(source, Fragments("f")), "js/app.js"); got != `console.log("$[x]")` {
		t.Fatalf("body = %s", got)
	}
}

func TestMissingFragmentRendersComment(t *testing.T) {
	source := fstest.MapFS{"index.html": file("<main>$[missing]</main>")}
	got := read(t, New(source, Fragments("fragments")), "index.html")
	if got != `<main><!-- Fragment "missing" not found --></main>` {
		t.Fatalf("body = %s", got)
	}
}

func TestMissingTemplateIsAnError(t *testing.T) {
	source := fstest.MapFS{"index.html": file("![TEMPLATE nope]\nhi")}
	if _, err := fs.ReadFile(New(source, Templates("templates")), "index.html"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestNestedFragments(t *testing.T) {
	source := fstest.MapFS{
		"index.html":   file("$[a]"),
		"f/a.html":     file("<a>$[b]</a>"),
		"f/b.html":     file("<b>$[sub/c]</b>"),
		"f/sub/c.html": file("c"),
	}
	if got := read(t, New(source, Fragments("f")), "index.html"); got != "<a><b>c</b></a>" {
		t.Fatalf("body = %s", got)
	}
}

func TestRecursiveFragmentsStop(t *testing.T) {
	source := fstest.MapFS{
		"self.html":   file("$[self]"),
		"loop.html":   file("$[a]"),
		"f/self.html": file("<s>$[self]</s>"),
		"f/a.html":    file("<a>$[b]</a>"),
		"f/b.html":    file("<b>$[a]</b>"),
	}
	fsys := New(source, Fragments("f"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		if got := read(t, fsys, "self.html"); got != `<s><!-- Fragment "self" skipped: recursive include --></s>` {
			t.Errorf("self: %s", got)
		}
		if got := read(t, fsys, "loop.html"); got != `<a><b><!-- Fragment "a" skipped: recursive include --></b></a>` {
			t.Errorf("loop: %s", got)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("rendering did not terminate")
	}
}

func TestDepthLimit(t *testing.T) {
	var deep int
	fsys := New(fstest.MapFS{"index.html": file("$[x]")}, func(c Content) (*Resolved, error) {
		deep++
		// Every fragment includes a fresh one, so there is no cycle to detect.
		return &Resolved{Data: []byte("$[x" + strings.Repeat("x", deep) + "]"), ModTime: older}, nil
	})
	got := read(t, fsys, "index.html")
	if !strings.Contains(got, "skipped: recursive include") || deep != MaxDepth {
		t.Fatalf("resolved %d fragments, body = %s", deep, got)
	}
}

func TestPageCannotIncludeItself(t *testing.T) {
	source := fstest.MapFS{
		"index.html": file("![TEMPLATE t]\n<p>$[page]</p>"),
		"t/t.html":   file("<main>$[page]</main>"),
	}
	got := read(t, New(source, Templates("t")), "index.html")
	if got != `<main><p><!-- Fragment "page" skipped: recursive include --></p></main>` {
		t.Fatalf("body = %s", got)
	}
}

func TestEscapedDirective(t *testing.T) {
	source := fstest.MapFS{
		"index.html": file("![TEMPLATE t]\n<script>a$$[i] = b$$[j]</script>"),
		"t/t.html":   file("<main>$[page] $$[page]</main>"),
	}
	got := read(t, New(source, Templates("t")), "index.html")
	if got != "<main><script>a$[i] = b$[j]</script> $[page]</main>" {
		t.Fatalf("body = %s", got)
	}
}

func TestInvalidNamesAreLeftAlone(t *testing.T) {
	source := fstest.MapFS{"index.html": file(`<script>$['x'] + $[ a ] + $[] + $[unclosed</script>`)}
	got := read(t, New(source, Fragments("f")), "index.html")
	if got != `<script>$['x'] + $[ a ] + $[] + $[unclosed</script>` {
		t.Fatalf("body = %s", got)
	}
}

func TestFragmentsCannotLeaveTheirDirectory(t *testing.T) {
	source := fstest.MapFS{
		"index.html":          file("$[../private/secret]"),
		"private/secret.html": file("SECRET"),
	}
	got := read(t, New(source, Fragments("f")), "index.html")
	if strings.Contains(got, "SECRET") {
		t.Fatalf("body = %s", got)
	}
}

func TestTemplateDirectiveVariants(t *testing.T) {
	for name, page := range map[string]string{
		"crlf": "![TEMPLATE t]\r\nx",
		"bom":  "\xef\xbb\xbf![TEMPLATE t]\nx",
	} {
		source := fstest.MapFS{"index.html": file(page), "t/t.html": file("<main>$[page]</main>")}
		if got := read(t, New(source, Templates("t")), "index.html"); got != "<main>x</main>" {
			t.Errorf("%s: body = %q", name, got)
		}
	}

	source := fstest.MapFS{"index.html": file("![TEMPLATE t]"), "t/t.html": file("<main>$[page]</main>")}
	if got := read(t, New(source, Templates("t")), "index.html"); got != "<main></main>" {
		t.Errorf("directive only: body = %q", got)
	}
}

func TestHookErrorsAreReturned(t *testing.T) {
	boom := errors.New("boom")
	fsys := New(fstest.MapFS{"index.html": file("$[x]")}, func(Content) (*Resolved, error) {
		return nil, boom
	})
	if _, err := fs.ReadFile(fsys, "index.html"); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestModTimeIncludesDependencies(t *testing.T) {
	source := fstest.MapFS{
		"index.html":   file("![TEMPLATE t]\n$[h]"),
		"t/t.html":     file("$[page]"),
		"f/h.html":     {Data: []byte("h"), ModTime: newer},
		"dynamic.html": file("$[d]"),
		"missing.html": file("$[nope]"),
	}
	fsys := New(source, Templates("t"), Fragments("f"),
		Fragment("d", func() string { return "d" }))

	for name, want := range map[string]time.Time{
		"index.html":   newer,
		"dynamic.html": {},
		"missing.html": {},
	} {
		info, err := fs.Stat(fsys, name)
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(want) {
			t.Errorf("%s: ModTime = %v, want %v", name, info.ModTime(), want)
		}
	}
}

func TestNoStaleNotModified(t *testing.T) {
	source := fstest.MapFS{
		"index.html":            file("$[header]"),
		"fragments/header.html": {Data: []byte("v2"), ModTime: newer},
	}
	handler := http.FileServer(http.FS(New(source, Fragments("fragments"))))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("If-Modified-Since", older.Add(time.Hour).Format(http.TimeFormat))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "v2" {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
}

func TestInvalidPathsAreRejected(t *testing.T) {
	fsys := New(fstest.MapFS{"index.html": file("hi")})
	for _, name := range []string{"/index.html", "../index.html", "./index.html", "a/../index.html"} {
		if _, err := fsys.Open(name); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("Open(%q) err = %v", name, err)
		}
		if _, err := fs.ReadFile(fsys, name); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("ReadFile(%q) err = %v", name, err)
		}
	}
}

func TestFSContract(t *testing.T) {
	source := fstest.MapFS{
		"index.html":             file("![TEMPLATE default]\n<h1>Hello</h1>$[header]"),
		"about/index.html":       file("![TEMPLATE default]\nAbout"),
		"templates/default.html": file("<main>$[page]</main>"),
		"fragments/header.html":  {Data: []byte("<nav>longer than the directive</nav>"), ModTime: newer},
		"js/app.js":              file("app"),
	}
	fsys := New(source, Templates("templates"), Fragments("fragments"))
	if err := fstest.TestFS(fsys,
		"index.html", "about/index.html", "templates/default.html",
		"fragments/header.html", "js/app.js",
	); err != nil {
		t.Fatal(err)
	}
}
