// Package torrentmeta reads the parts of a .torrent metainfo file the bot
// shows before a download: what the torrent is called, and which files it
// contains. Prowlarr publishes neither as structured data, so the only place
// to learn them is the metainfo itself.
//
// It is a reader, not a BitTorrent implementation: nothing here re-encodes
// bencode or computes an info hash, so the decoder does not need to be
// canonical — it only has to be strict about what it accepts. The largest
// field, info.pieces, is never copied; strings are slices into the caller's
// buffer, which matters because the caller may hand over tens of megabytes.
package torrentmeta

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// maxDepth bounds bencode nesting. Real metainfo nests three or four levels
// deep; the limit is what stops a hand-crafted file from recursing until the
// stack gives out.
const maxDepth = 16

// File is one file inside the torrent.
type File struct {
	Path   string // slash-joined path as the torrent states it
	Length int64  // bytes
}

// Meta is the metainfo, reduced to what is worth showing in a chat message.
type Meta struct {
	Name      string // torrent name: the file for a single-file torrent, the directory otherwise
	Files     []File // always at least one entry
	TotalSize int64  // sum of all file lengths
	Comment   string // free-text comment, often the tracker's release page
	CreatedBy string // client that created the torrent
	Private   bool   // private flag: no DHT/PEX
}

// Parse decodes .torrent metainfo bytes. It fails rather than guessing: a
// truncated download or an HTML error page served in place of a torrent must
// read as an error, not as a torrent with no files.
func Parse(data []byte) (Meta, error) {
	d := &decoder{buf: data}
	root, err := d.value(0)
	if err != nil {
		return Meta{}, err
	}
	if d.pos != len(d.buf) {
		return Meta{}, fmt.Errorf("metainfo: %d trailing bytes after the top-level value", len(d.buf)-d.pos)
	}
	top, ok := root.(map[string]any)
	if !ok {
		return Meta{}, errors.New("metainfo: top-level value is not a dictionary")
	}

	infoVal, ok := top["info"]
	if !ok {
		return Meta{}, errors.New("metainfo: no info dictionary")
	}
	info, ok := infoVal.(map[string]any)
	if !ok {
		return Meta{}, errors.New("metainfo: info is not a dictionary")
	}

	name, ok := text(pick(info, "name.utf-8", "name"))
	if !ok || name == "" {
		return Meta{}, errors.New("metainfo: no name")
	}

	m := Meta{Name: name}
	m.Comment, _ = text(pick(top, "comment.utf-8", "comment"))
	m.CreatedBy, _ = text(pick(top, "created by.utf-8", "created by"))
	if p, ok := integer(info["private"]); ok && p == 1 {
		m.Private = true
	}

	files, err := fileList(info, name)
	if err != nil {
		return Meta{}, err
	}
	m.Files = files
	for _, f := range files {
		m.TotalSize += f.Length
	}
	return m, nil
}

// fileList reads the multi-file "files" list, falling back to the single-file
// "length" key. A torrent has exactly one of the two.
func fileList(info map[string]any, name string) ([]File, error) {
	entries, ok := info["files"].([]any)
	if !ok {
		length, ok := integer(info["length"])
		if !ok {
			return nil, errors.New("metainfo: info has neither length nor files")
		}
		if length < 0 {
			return nil, fmt.Errorf("metainfo: negative length %d", length)
		}
		return []File{{Path: name, Length: length}}, nil
	}
	if len(entries) == 0 {
		return nil, errors.New("metainfo: files list is empty")
	}

	files := make([]File, 0, len(entries))
	for i, e := range entries {
		entry, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("metainfo: file %d is not a dictionary", i)
		}
		length, ok := integer(entry["length"])
		if !ok {
			return nil, fmt.Errorf("metainfo: file %d has no length", i)
		}
		if length < 0 {
			return nil, fmt.Errorf("metainfo: file %d has negative length %d", i, length)
		}
		parts, ok := pick(entry, "path.utf-8", "path").([]any)
		if !ok || len(parts) == 0 {
			return nil, fmt.Errorf("metainfo: file %d has no path", i)
		}
		names := make([]string, 0, len(parts))
		for _, p := range parts {
			s, ok := text(p)
			if !ok {
				return nil, fmt.Errorf("metainfo: file %d has a non-string path element", i)
			}
			names = append(names, s)
		}
		files = append(files, File{Path: strings.Join(names, "/"), Length: length})
	}
	return files, nil
}

// pick returns the first of keys present in d, so a ".utf-8" variant can take
// precedence over the legacy key it shadows.
func pick(d map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := d[k]; ok {
			return v
		}
	}
	return nil
}

// text renders a decoded bencode string; decoded strings are raw bytes,
// because a torrent may name a file in any encoding.
func text(v any) (string, bool) {
	b, ok := v.([]byte)
	if !ok {
		return "", false
	}
	return string(b), true
}

// integer renders a decoded bencode integer.
func integer(v any) (int64, bool) {
	n, ok := v.(int64)
	return n, ok
}

// decoder walks a bencode document. Decoded values are []byte (strings),
// int64, []any, or map[string]any.
type decoder struct {
	buf []byte
	pos int
}

func (d *decoder) value(depth int) (any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("bencode: nesting deeper than %d", maxDepth)
	}
	if d.pos >= len(d.buf) {
		return nil, errors.New("bencode: unexpected end of input")
	}
	switch c := d.buf[d.pos]; {
	case c == 'i':
		return d.integer()
	case c == 'l':
		return d.list(depth)
	case c == 'd':
		return d.dict(depth)
	case c >= '0' && c <= '9':
		return d.str()
	default:
		return nil, fmt.Errorf("bencode: unexpected byte %q at offset %d", c, d.pos)
	}
}

// integer reads "i<digits>e".
func (d *decoder) integer() (int64, error) {
	end := d.index('e')
	if end < 0 {
		return 0, errors.New("bencode: unterminated integer")
	}
	n, err := strconv.ParseInt(string(d.buf[d.pos+1:end]), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bencode: bad integer at offset %d", d.pos)
	}
	d.pos = end + 1
	return n, nil
}

// str reads "<length>:<bytes>", returning a slice into the buffer rather than
// a copy — info.pieces alone can be megabytes.
func (d *decoder) str() ([]byte, error) {
	colon := d.index(':')
	if colon < 0 {
		return nil, errors.New("bencode: string without a length separator")
	}
	n, err := strconv.Atoi(string(d.buf[d.pos:colon]))
	if err != nil || n < 0 {
		return nil, fmt.Errorf("bencode: bad string length at offset %d", d.pos)
	}
	start := colon + 1
	if start+n > len(d.buf) {
		return nil, errors.New("bencode: string runs past the end of the input")
	}
	d.pos = start + n
	return d.buf[start : start+n], nil
}

// list reads "l<values>e".
func (d *decoder) list(depth int) ([]any, error) {
	d.pos++ // 'l'
	items := []any{}
	for {
		if d.pos >= len(d.buf) {
			return nil, errors.New("bencode: unterminated list")
		}
		if d.buf[d.pos] == 'e' {
			d.pos++
			return items, nil
		}
		v, err := d.value(depth + 1)
		if err != nil {
			return nil, err
		}
		items = append(items, v)
	}
}

// dict reads "d<key><value>…e"; keys must be strings.
func (d *decoder) dict(depth int) (map[string]any, error) {
	d.pos++ // 'd'
	m := map[string]any{}
	for {
		if d.pos >= len(d.buf) {
			return nil, errors.New("bencode: unterminated dictionary")
		}
		if d.buf[d.pos] == 'e' {
			d.pos++
			return m, nil
		}
		key, err := d.str()
		if err != nil {
			return nil, fmt.Errorf("bencode: dictionary key: %w", err)
		}
		v, err := d.value(depth + 1)
		if err != nil {
			return nil, err
		}
		m[string(key)] = v
	}
}

// index finds the next b at or after the current position.
func (d *decoder) index(b byte) int {
	for i := d.pos; i < len(d.buf); i++ {
		if d.buf[i] == b {
			return i
		}
	}
	return -1
}
