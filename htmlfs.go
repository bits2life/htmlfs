// Package htmlfs wraps an fs.FS and renders HTML files as they are read.
//
// A page that starts with a "![TEMPLATE name]" line is wrapped in the named
// template, with "$[page]" in the template replaced by the rest of the page.
// "$[name]" anywhere in a page, template or fragment is replaced by the named
// fragment, and "$$[" produces a literal "$[".
//
// Templates and fragments are resolved by ContentHooks, see Templates,
// Fragments and Fragment. Serve the result with the standard library:
//
//	http.FileServer(http.FS(htmlfs.New(publicFS, ...)))
package htmlfs

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"
)

const (
	KindTemplate = "template"
	KindFragment = "fragment"
)

// MaxDepth limits how deeply fragments may include other fragments.
const MaxDepth = 32

// Content identifies a template or fragment requested during rendering.
type Content struct {
	FS   fs.FS
	Kind string
	Name string
}

// Resolved is the content returned by a ContentHook.
//
// A zero ModTime marks the content as dynamic: pages that include it are
// served without a modification time, so clients never get a stale 304.
type Resolved struct {
	Data    []byte
	ModTime time.Time
}

// ContentHook resolves a template or fragment. It returns nil, nil when it
// does not handle the requested content, letting the next hook try.
type ContentHook func(Content) (*Resolved, error)

// Templates loads templates from dir/<name>.html in the wrapped FS.
func Templates(dir string) ContentHook {
	return fileHook(KindTemplate, dir)
}

// Fragments loads fragments from dir/<name>.html in the wrapped FS.
func Fragments(dir string) ContentHook {
	return fileHook(KindFragment, dir)
}

// Fragment registers a dynamic fragment, rendered each time it is used.
func Fragment(name string, render func() string) ContentHook {
	return func(content Content) (*Resolved, error) {
		if content.Kind != KindFragment || content.Name != name {
			return nil, nil
		}
		return &Resolved{Data: []byte(render())}, nil
	}
}

func fileHook(kind, dir string) ContentHook {
	return func(content Content) (*Resolved, error) {
		if content.Kind != kind || !fs.ValidPath(content.Name) {
			return nil, nil
		}
		name := path.Join(dir, content.Name+".html")
		info, err := fs.Stat(content.FS, name)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		data, err := fs.ReadFile(content.FS, name)
		if err != nil {
			return nil, err
		}
		return &Resolved{Data: data, ModTime: info.ModTime()}, nil
	}
}

type renderFS struct {
	source fs.FS
	hooks  []ContentHook
}

// New wraps source so that .html files are rendered with the given hooks.
// Other files and directories are passed through unchanged.
func New(source fs.FS, hooks ...ContentHook) fs.FS {
	return &renderFS{
		source: source,
		hooks:  hooks,
	}
}

func (f *renderFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}

	info, err := fs.Stat(f.source, name)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		file, err := f.source.Open(name)
		if err != nil {
			return nil, err
		}
		return &dirFile{File: file, fsys: f, dir: name}, nil
	}
	if !isHTML(name) {
		return f.source.Open(name)
	}

	data, rendered, err := f.renderFile(name)
	if err != nil {
		return nil, err
	}
	return &memoryFile{Reader: bytes.NewReader(data), info: rendered}, nil
}

func (f *renderFS) ReadFile(name string) ([]byte, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "readfile", Path: name, Err: fs.ErrInvalid}
	}
	if !isHTML(name) {
		return fs.ReadFile(f.source, name)
	}
	data, _, err := f.renderFile(name)
	return data, err
}

func (f *renderFS) renderFile(name string) ([]byte, fs.FileInfo, error) {
	info, err := fs.Stat(f.source, name)
	if err != nil {
		return nil, nil, err
	}
	data, err := fs.ReadFile(f.source, name)
	if err != nil {
		return nil, nil, err
	}

	r := &renderer{fsys: f}
	r.track(info.ModTime())
	out, err := r.render(data)
	if err != nil {
		return nil, nil, &fs.PathError{Op: "render", Path: name, Err: err}
	}
	return out, renderedFileInfo{FileInfo: info, size: int64(len(out)), modTime: r.modTime}, nil
}

func isHTML(name string) bool {
	return path.Ext(name) == ".html"
}

// renderer holds the state of rendering a single page.
type renderer struct {
	fsys    *renderFS
	page    []byte
	modTime time.Time
	dynamic bool
}

// track records the modification time of something the page depends on.
// The rendered page is as new as its newest dependency, or dynamic if any
// dependency has no modification time.
func (r *renderer) track(t time.Time) {
	if r.dynamic {
		return
	}
	if t.IsZero() {
		r.dynamic = true
		r.modTime = time.Time{}
		return
	}
	if t.After(r.modTime) {
		r.modTime = t
	}
}

func (r *renderer) render(content []byte) ([]byte, error) {
	var buf bytes.Buffer
	if name, page, ok := splitTemplateDirective(content); ok {
		template, err := r.resolve(KindTemplate, name)
		if err != nil {
			return nil, err
		}
		if template == nil {
			return nil, fmt.Errorf("template %q not found", name)
		}
		r.page = page
		content = template.Data
	}
	if err := r.expand(&buf, content, nil); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (r *renderer) resolve(kind, name string) (*Resolved, error) {
	for _, hook := range r.fsys.hooks {
		resolved, err := hook(Content{
			FS:   r.fsys.source,
			Kind: kind,
			Name: name,
		})
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", kind, name, err)
		}
		if resolved != nil {
			r.track(resolved.ModTime)
			return resolved, nil
		}
	}
	return nil, nil
}

var (
	openDirective    = []byte("$[")
	escapedDirective = []byte("$$[")
)

// expand writes content to buf, replacing $[name] directives. stack holds
// the fragments currently being expanded, to stop recursive includes.
func (r *renderer) expand(buf *bytes.Buffer, content []byte, stack []string) error {
	for {
		start := bytes.Index(content, openDirective)
		if start == -1 {
			buf.Write(content)
			return nil
		}
		if start > 0 && content[start-1] == '$' {
			buf.Write(content[:start-1])
			buf.Write(openDirective)
			content = content[start+len(openDirective):]
			continue
		}

		rest := content[start+len(openDirective):]
		end := bytes.IndexByte(rest, ']')
		if end == -1 {
			buf.Write(content)
			return nil
		}
		name := string(rest[:end])
		if !validName(name) {
			buf.Write(content[:start+len(openDirective)])
			content = rest
			continue
		}

		buf.Write(content[:start])
		if err := r.fragment(buf, name, stack); err != nil {
			return err
		}
		content = rest[end+1:]
	}
}

func (r *renderer) fragment(buf *bytes.Buffer, name string, stack []string) error {
	if slices.Contains(stack, name) || len(stack) >= MaxDepth {
		fmt.Fprintf(buf, "<!-- Fragment %q skipped: recursive include -->", name)
		return nil
	}
	if name == "page" && r.page != nil {
		return r.expand(buf, r.page, append(stack, name))
	}

	resolved, err := r.resolve(KindFragment, name)
	if err != nil {
		return err
	}
	if resolved == nil {
		// The fragment may appear later, so the page must not be cached.
		r.track(time.Time{})
		fmt.Fprintf(buf, "<!-- Fragment %q not found -->", name)
		return nil
	}
	return r.expand(buf, resolved.Data, append(stack, name))
}

// validName reports whether name can be a fragment name. Anything else
// inside $[...] is left as it is.
func validName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '-', c == '.', c == '/':
		default:
			return false
		}
	}
	return true
}

var utf8BOM = []byte("\xef\xbb\xbf")

func splitTemplateDirective(content []byte) (name string, page []byte, ok bool) {
	content = bytes.TrimPrefix(content, utf8BOM)
	firstLine, rest, _ := bytes.Cut(content, []byte("\n"))
	line := strings.TrimSuffix(string(firstLine), "\r")
	if !strings.HasPrefix(line, "![TEMPLATE ") || !strings.HasSuffix(line, "]") {
		return "", nil, false
	}

	name = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "![TEMPLATE "), "]"))
	if name == "" {
		return "", nil, false
	}
	if rest == nil {
		rest = []byte{}
	}
	return name, rest, true
}

type memoryFile struct {
	*bytes.Reader
	info fs.FileInfo
}

func (f *memoryFile) Stat() (fs.FileInfo, error) {
	return f.info, nil
}

func (f *memoryFile) Close() error {
	return nil
}

type renderedFileInfo struct {
	fs.FileInfo
	size    int64
	modTime time.Time
}

func (i renderedFileInfo) Size() int64 {
	return i.size
}

func (i renderedFileInfo) ModTime() time.Time {
	return i.modTime
}

// dirFile reports rendered sizes and modification times for HTML entries.
type dirFile struct {
	fs.File
	fsys *renderFS
	dir  string
}

func (d *dirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	rd, ok := d.File.(fs.ReadDirFile)
	if !ok {
		return nil, &fs.PathError{Op: "readdir", Path: d.dir, Err: errors.New("not implemented")}
	}
	entries, err := rd.ReadDir(n)
	for i, entry := range entries {
		if !entry.IsDir() && isHTML(entry.Name()) {
			entries[i] = &renderedEntry{DirEntry: entry, fsys: d.fsys, name: path.Join(d.dir, entry.Name())}
		}
	}
	return entries, err
}

// renderedEntry renders its file lazily, only when Info is called.
type renderedEntry struct {
	fs.DirEntry
	fsys *renderFS
	name string
}

func (e *renderedEntry) Info() (fs.FileInfo, error) {
	_, info, err := e.fsys.renderFile(e.name)
	return info, err
}
