package siteimport

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// DumpInfo is what InspectDump reads from a dump.
type DumpInfo struct {
	// Gzip reports whether the dump is gzip-compressed.
	Gzip bool `json:"gzip"`
	// TablePrefix is the WordPress table prefix, e.g. "wp_".
	TablePrefix string `json:"table_prefix"`
	// Tables counts the tables the dump creates.
	Tables int `json:"tables"`
	// Home and SiteURL are the site's home and siteurl options: its
	// public URL and WordPress's own.
	Home    string `json:"home"`
	SiteURL string `json:"siteurl"`
	// DBVersion is the db_version option: the database schema version of
	// the WordPress core that last upgraded the database.
	DBVersion int `json:"db_version"`
	// BlogName is the site's title (blogname), which may be empty.
	BlogName string `json:"blogname,omitempty"`
	// Stylesheet is the active theme (the child theme, if any).
	Stylesheet string `json:"stylesheet,omitempty"`
}

// wantedOptions are the options InspectDump reads.
var wantedOptions = []string{"home", "siteurl", "db_version", "blogname", "stylesheet"}

// maxLineHead is how much of a line InspectDump and SanitizeDump look at
// to tell what it is; the rest of a longer line is streamed.
const maxLineHead = 1 << 20

var (
	createTable = regexp.MustCompile("^(?i:CREATE\\s+TABLE)\\s+(?i:IF\\s+NOT\\s+EXISTS\\s+)?(`(?:[^`]|``)+`|[A-Za-z0-9_$]+)")
	// insertHead matches the head of an INSERT or REPLACE statement up to
	// VALUES: the table and the optional column list.
	insertHead = regexp.MustCompile("^(?i:INSERT|REPLACE)\\s+(?i:(?:LOW_PRIORITY|DELAYED|HIGH_PRIORITY|IGNORE)\\s+)*(?i:INTO\\s+)?" +
		"(`(?:[^`]|``)+`|[A-Za-z0-9_$]+)\\s*(\\([^)]*\\))?\\s*(?i:VALUES)\\s*")
	prefixPattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
)

// openDump returns the dump's text, decompressing a gzip-compressed dump.
func openDump(r io.Reader) (*bufio.Reader, bool, error) {
	br := bufio.NewReaderSize(r, maxLineHead)
	head, err := br.Peek(4)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	switch {
	case len(head) == 0:
		return nil, false, invalidDump("it is empty")
	case bytes.HasPrefix(head, []byte{0x1f, 0x8b}):
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, true, invalidDump("it can't be decompressed: %v", err)
		}
		return bufio.NewReaderSize(&dumpBudget{r: zr}, maxLineHead), true, nil
	case bytes.HasPrefix(head, []byte("PK\x03\x04")), bytes.HasPrefix(head, []byte("BZh")),
		bytes.HasPrefix(head, []byte{0xfd, '7', 'z', 'X'}), bytes.HasPrefix(head, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		return nil, false, invalidDump("it is a compressed file of a kind that isn't supported; send a .sql or .sql.gz file")
	}
	return bufio.NewReaderSize(&dumpBudget{r: br}, maxLineHead), false, nil
}

// dumpBudget fails once a dump's text, decompressed, passes
// maxUnpackedBytes, so a small gzip bomb can't fill the disk.
type dumpBudget struct {
	r    io.Reader
	read int64
}

func (d *dumpBudget) Read(p []byte) (int, error) {
	if room := maxUnpackedBytes - d.read; int64(len(p)) > room+1 {
		p = p[:room+1]
	}
	n, err := d.r.Read(p)
	d.read += int64(n)
	if d.read > maxUnpackedBytes {
		return n, invalidDump("it is larger than %d MiB unpacked", maxUnpackedBytes>>20)
	}
	return n, err
}

// lines calls fn with each chunk of the dump's text: whole lines, or
// pieces of lines longer than maxLineHead. start reports whether the chunk
// starts a line.
func lines(br *bufio.Reader, fn func(chunk []byte, start bool) error) error {
	start := true
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			if ferr := fn(chunk, start); ferr != nil {
				return ferr
			}
		}
		switch {
		case err == nil:
			start = true
		case errors.Is(err, bufio.ErrBufferFull):
			start = false
		case errors.Is(err, io.EOF):
			return nil
		default:
			return readError(err)
		}
	}
}

// readError reports a dump that can't be read to the end.
func readError(err error) error {
	if errors.Is(err, gzip.ErrChecksum) || errors.Is(err, gzip.ErrHeader) || errors.Is(err, io.ErrUnexpectedEOF) {
		return invalidDump("it can't be read to the end: %v", err)
	}
	return err
}

// isData reports whether a line is part of an INSERT's values.
func isData(line []byte) bool {
	l := bytes.TrimLeft(line, " \t")
	return len(l) > 0 && l[0] == '(' || hasPrefixFold(l, "INSERT") || hasPrefixFold(l, "REPLACE")
}

func hasPrefixFold(b []byte, prefix string) bool {
	return len(b) >= len(prefix) && strings.EqualFold(string(b[:len(prefix)]), prefix)
}

// unquoteIdent removes an identifier's backquotes.
func unquoteIdent(s string) string {
	if len(s) >= 2 && s[0] == '`' {
		return strings.ReplaceAll(s[1:len(s)-1], "``", "`")
	}
	return s
}

// InspectDump checks that r is a SQL dump of one WordPress site's
// database and reads what the import needs from it; see the package
// documentation. Problems are ErrInvalidDump (ErrMultisite for a
// multisite network).
func InspectDump(r io.Reader) (DumpInfo, error) {
	br, gz, err := openDump(r)
	if err != nil {
		return DumpInfo{}, err
	}
	info := DumpInfo{Gzip: gz}
	var tables []string
	// Options of every table named *options, by table: the prefix isn't
	// known until all tables are.
	options := map[string]map[string]string{}
	var values *valuesParser
	checkedText := false
	err = lines(br, func(chunk []byte, start bool) error {
		if !checkedText {
			checkedText = true
			if bytes.IndexByte(chunk, 0) >= 0 {
				return invalidDump("it is binary data, not a SQL dump")
			}
		}
		if values != nil {
			done, err := values.feed(chunk)
			if err != nil {
				return err
			}
			if done {
				values = nil
			}
			return nil
		}
		if !start {
			return nil
		}
		line := bytes.TrimLeft(chunk, " \t")
		if m := createTable.FindSubmatch(line); m != nil {
			tables = append(tables, unquoteIdent(string(m[1])))
			return nil
		}
		m := insertHead.FindSubmatchIndex(line)
		if m == nil {
			return nil
		}
		table := unquoteIdent(string(line[m[2]:m[3]]))
		if !strings.HasSuffix(table, "options") {
			return nil
		}
		cols := []string{"option_id", "option_name", "option_value", "autoload"}
		if m[4] >= 0 {
			cols = nil
			for _, c := range strings.Split(strings.Trim(string(line[m[4]:m[5]]), "()"), ",") {
				cols = append(cols, unquoteIdent(strings.TrimSpace(c)))
			}
		}
		name, value := slices.Index(cols, "option_name"), slices.Index(cols, "option_value")
		if name < 0 || value < 0 {
			return nil
		}
		if options[table] == nil {
			options[table] = map[string]string{}
		}
		opts := options[table]
		values = &valuesParser{onRow: func(row []string) {
			if name < len(row) && value < len(row) && slices.Contains(wantedOptions, row[name]) {
				opts[row[name]] = row[value]
			}
		}}
		done, err := values.feed(line[m[1]:])
		if err != nil {
			return err
		}
		if done {
			values = nil
		}
		return nil
	})
	if err != nil {
		return DumpInfo{}, err
	}
	if len(tables) == 0 {
		return DumpInfo{}, invalidDump("it creates no tables; send a SQL dump of the site's database, e.g. from `wp db export` or mysqldump")
	}
	info.Tables = len(tables)
	prefix, err := wpPrefix(tables)
	if err != nil {
		return DumpInfo{}, err
	}
	info.TablePrefix = prefix
	opts := options[prefix+"options"]
	for _, o := range []struct {
		name string
		dst  *string
	}{{"home", &info.Home}, {"siteurl", &info.SiteURL}} {
		v, ok := opts[o.name]
		if !ok {
			return DumpInfo{}, invalidDump("it has no %s option in %soptions", o.name, prefix)
		}
		u, err := url.Parse(strings.TrimSpace(v))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return DumpInfo{}, invalidDump("its %s option %q isn't an http or https URL", o.name, v)
		}
		*o.dst = strings.TrimRight(strings.TrimSpace(v), "/")
	}
	v, ok := opts["db_version"]
	if !ok {
		return DumpInfo{}, invalidDump("it has no db_version option in %soptions, which says which WordPress version the database is for", prefix)
	}
	if info.DBVersion, err = strconv.Atoi(strings.TrimSpace(v)); err != nil || info.DBVersion <= 0 {
		return DumpInfo{}, invalidDump("its db_version option %q isn't a database version", v)
	}
	info.BlogName, info.Stylesheet = opts["blogname"], opts["stylesheet"]
	return info, nil
}

// wpPrefix finds the table prefix of the one WordPress site whose tables
// are in the dump.
func wpPrefix(tables []string) (string, error) {
	has := map[string]bool{}
	for _, t := range tables {
		has[t] = true
	}
	var prefixes []string
	for _, t := range tables {
		p, ok := strings.CutSuffix(t, "options")
		if ok && has[p+"posts"] && has[p+"users"] && has[p+"usermeta"] && !slices.Contains(prefixes, p) {
			prefixes = append(prefixes, p)
		}
	}
	switch len(prefixes) {
	case 0:
		return "", invalidDump("it has no WordPress tables (an options, posts, users, and usermeta table with a common prefix)")
	case 1:
	default:
		slices.Sort(prefixes)
		return "", invalidDump("it has the tables of several WordPress sites (table prefixes %s); export one site's tables", strings.Join(prefixes, ", "))
	}
	p := prefixes[0]
	for _, t := range []string{"blogs", "site", "sitemeta"} {
		if has[p+t] {
			return "", fmt.Errorf("%w (it has the %s%s table)", ErrMultisite, p, t)
		}
	}
	if p == "" || !prefixPattern.MatchString(p) {
		return "", invalidDump("its table prefix %q isn't one WordPress accepts (letters, digits, and underscores)", p)
	}
	return p, nil
}

// valuesParser reads the rows of an INSERT's VALUES list, streamed in
// chunks, and calls onRow with each row's values as strings (NULL as "").
type valuesParser struct {
	onRow func([]string)

	state   int
	row     []string
	cur     strings.Builder
	escaped bool
	quote   byte
}

// valuesParser states.
const (
	vpBeforeRow = iota // expecting "(" or ";"
	vpInRow            // between values
	vpBare             // in an unquoted value
	vpQuoted           // in a quoted string
	vpQuoteEnd         // after a quote that closes the string, unless doubled
	vpAfterRow         // expecting "," or ";"
)

// maxOptionValue bounds a value the parser keeps; longer ones (big
// serialized options) are cut, since only short options are wanted.
const maxOptionValue = 64 << 10

// feed parses the next chunk and reports whether the statement ended.
func (p *valuesParser) feed(chunk []byte) (bool, error) {
	for i := 0; i < len(chunk); i++ {
		c := chunk[i]
		switch p.state {
		case vpBeforeRow:
			switch {
			case c == '(':
				p.state, p.row = vpInRow, p.row[:0]
			case c == ';':
				return true, nil
			case isSpace(c):
			default:
				return false, invalidDump("an INSERT into the options table can't be parsed (unexpected %q)", c)
			}
		case vpAfterRow:
			switch {
			case c == ',':
				p.state = vpBeforeRow
			case c == ';':
				return true, nil
			case isSpace(c):
			default:
				return false, invalidDump("an INSERT into the options table can't be parsed (unexpected %q after a row)", c)
			}
		case vpInRow:
			switch {
			case c == '\'' || c == '"':
				p.state, p.quote = vpQuoted, c
				p.cur.Reset()
			case c == ',', isSpace(c):
			case c == ')':
				p.onRow(p.row)
				p.state = vpAfterRow
			default:
				p.state = vpBare
				p.cur.Reset()
				p.cur.WriteByte(c)
			}
		case vpBare:
			switch {
			case c == '\'' || c == '"':
				// A charset introducer such as _utf8mb4'...'.
				p.state, p.quote = vpQuoted, c
				p.cur.Reset()
			case c == ',' || c == ')' || isSpace(c):
				v := p.cur.String()
				if strings.EqualFold(v, "NULL") {
					v = ""
				}
				// An introducer followed by a space ("_binary '...'") is
				// followed by its string, which is the value.
				if !strings.HasPrefix(v, "_") || !isSpace(c) {
					p.row = append(p.row, v)
				}
				p.state = vpInRow
				i-- // the separator belongs to the row
			default:
				p.cur.WriteByte(c)
			}
		case vpQuoted:
			switch {
			case p.escaped:
				p.escaped = false
				p.add(unescape(c))
			case c == '\\':
				p.escaped = true
			case c == p.quote:
				p.state = vpQuoteEnd
			default:
				p.add(c)
			}
		case vpQuoteEnd:
			if c == p.quote {
				// A doubled quote stands for itself.
				p.add(c)
				p.state = vpQuoted
				continue
			}
			p.row = append(p.row, p.cur.String())
			p.state = vpInRow
			i--
		}
	}
	return false, nil
}

func (p *valuesParser) add(c byte) {
	if p.cur.Len() < maxOptionValue {
		p.cur.WriteByte(c)
	}
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// unescape resolves a backslash escape in a MySQL string literal.
func unescape(c byte) byte {
	switch c {
	case 'n':
		return '\n'
	case 'r':
		return '\r'
	case 't':
		return '\t'
	case '0':
		return 0
	case 'Z':
		return 0x1a
	case 'b':
		return '\b'
	}
	return c
}
