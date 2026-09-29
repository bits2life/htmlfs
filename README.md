# htmlfs

`htmlfs` wraps an `fs.FS` and renders HTML files as they are read, with page
templates and include fragments. It uses only the standard library and serves
through `http.FileServer` unchanged.

```go
import "go.bits2life.com/htmlfs"

site := htmlfs.New(os.DirFS("public"),
	htmlfs.Templates("templates"),
	htmlfs.Fragments("fragments"),
	htmlfs.Fragment("IMPORTMAP", importmaps.GetInlineScriptTag),
)
http.Handle("/", http.FileServer(http.FS(site)))
```

## Syntax

`public/index.html`:

```html
![TEMPLATE default]
<h1>Hello</h1>
```

`public/templates/default.html`:

```html
<html>
<head>$[IMPORTMAP]</head>
<body>
  $[header]
  $[page]
</body>
</html>
```

- `![TEMPLATE name]` on the first line of a page wraps it in the template
  `name`. `$[page]` in the template is replaced by the rest of the page.
- `$[name]` is replaced by the fragment `name`, anywhere in a page, template or
  fragment. Fragments may include other fragments. Names may contain letters,
  digits, `_`, `-`, `.` and `/`; anything else between `$[` and `]` is left
  as it is.
- `$$[` produces a literal `$[`, for inline scripts such as `a$$[i]`.
- A missing fragment renders as `<!-- Fragment "name" not found -->`. A missing
  template is an error, which `http.FileServer` reports as a 500.
- A fragment that includes itself, directly or through others, renders as a
  comment instead of recursing. Nesting is limited to `MaxDepth` levels.

Only files ending in `.html` are rendered; everything else is passed through.

## Resolving content

Templates and fragments come from a chain of `ContentHook`s, tried in order:

- `Templates(dir)` loads `dir/<name>.html` from the wrapped FS.
- `Fragments(dir)` loads `dir/<name>.html` from the wrapped FS.
- `Fragment(name, func() string)` renders a dynamic fragment on every use.

A custom hook returns `nil, nil` for content it does not handle:

```go
func(c htmlfs.Content) (*htmlfs.Resolved, error) {
	if c.Kind != htmlfs.KindFragment || c.Name != "year" {
		return nil, nil
	}
	return &htmlfs.Resolved{Data: []byte(strconv.Itoa(time.Now().Year()))}, nil
}
```

## Caching

A rendered page reports the newest modification time of the page, its template
and its fragments, so `http.FileServer` answers `If-Modified-Since` correctly
when only a fragment changed. If a page uses a dynamic fragment (a `Resolved`
with a zero `ModTime`) or a missing one, it reports no modification time and is
never answered with a 304.

Nothing is cached in memory: every read renders the page again.

## Things to know

- This is text substitution, not `html/template`. Nothing is escaped, so
  templates and fragments must come from trusted authors.
- Rendering has no access to the HTTP request, so per-request content (the
  signed-in user, CSRF tokens) belongs in JavaScript or a separate handler.
- Template and fragment directories are ordinary files in the wrapped FS, and
  `http.FileServer` serves their unrendered source unless you keep them
  elsewhere or filter those paths.

## License

MIT
